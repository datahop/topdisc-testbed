package main

import (
	"crypto/ecdsa"
	"encoding/binary"
	"github.com/datahop/topdisc-testbed/pkg/churn"
	"github.com/datahop/topdisc-testbed/pkg/scenario"
	"math"
	"math/rand"
	"net"
	"sync"
	"time"

	"github.com/marcopolo/simnet"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/p2p/discover"
	"github.com/ethereum/go-ethereum/p2p/discover/topicindex"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

// defaultMaxBootnodes caps how many predecessors a fresh node bootstraps from.
const defaultMaxBootnodes = 20

// testTopic is the legacy single-topic ID used when -topics is unset or 1.
var testTopic = topicindex.TopicID{0x55, 0x49, 0x43, 0x4e, 0x47, 0x54, 0x45, 0x53, 0x54}

// node* vars override topicindex.Config defaults when set; wired from flags.
var (
	nodeAdLifetime           time.Duration
	nodeSearchBucketSize     int
	nodeRegAttemptTimeout    time.Duration
	nodeRemoveOnExpiry       bool
	nodeNodesPerSourceBucket int
	nodeAdCacheSize          int
	nodeTopicNodesLimit      int
	nodeAuxNodesLimit        int
	nodeTopic                scenario.TopicConfig // table depths and registration bucket sizes
)

// makeTopic returns a deterministic 32-byte topic ID for index i.
func makeTopic(i int) topicindex.TopicID {
	// Spread topic ids uniformly across the keyspace with a golden-ratio (Weyl)
	// sequence on the high 64 bits, so any number of topics is maximally
	// separated (no accidental clustering). Low bits are hash-derived so ids are
	// still full-width and realistic.
	const phi = 0.6180339887498949 // (sqrt(5) - 1) / 2
	frac := math.Mod(float64(i+1)*phi, 1.0)
	h := crypto.Keccak256([]byte{0x74, 0x6f, 0x70, byte(i >> 24), byte(i >> 16), byte(i >> 8), byte(i)})
	var t topicindex.TopicID
	copy(t[:], h)
	binary.BigEndian.PutUint64(t[:8], uint64(frac*math.Ldexp(1, 63))<<1)
	return t
}

type nodeRec struct {
	idx    int
	key    *ecdsa.PrivateKey
	ln     *enode.LocalNode
	disc   *discover.UDPv5
	legacy bool
}

func pickLegacySet(total int, frac float64, seed int64) map[int]bool {
	count := int(float64(total) * frac)
	if count <= 0 {
		return nil
	}
	s := seed
	if s == 0 {
		s = time.Now().UnixNano()
	}
	rng := rand.New(rand.NewSource(s))
	set := make(map[int]bool, count)
	for _, i := range rng.Perm(total)[:count] {
		set[i] = true
	}
	return set
}

// sampleBootnodes picks up to max bootnodes from pool: the first node plus a
// random sample of the rest.
func sampleBootnodes(pool []nodeRec, max int, rng *rand.Rand) []*enode.Node {
	if len(pool) == 0 {
		return nil
	}
	if max <= 0 {
		max = defaultMaxBootnodes
	}
	boot := []*enode.Node{pool[0].ln.Node()}
	rest := pool[1:]
	n := max - 1
	if n > len(rest) {
		n = len(rest)
	}
	for _, idx := range rng.Perm(len(rest))[:n] {
		boot = append(boot, rest[idx].ln.Node())
	}
	return boot
}

// spawnNode creates one discv5 node with the given bootnodes and the flag-driven
// topic config overrides.
func spawnNode(sim *simnet.Simnet, settings simnet.NodeBiDiLinkSettings, idx int, legacy bool, boot []*enode.Node, refreshInterval time.Duration) nodeRec {
	key, err := crypto.GenerateKey()
	if err != nil {
		fatalf("generate key %d: %v", idx, err)
	}
	addr := &net.UDPAddr{IP: nodeIP(idx), Port: 30303}
	// A leveldb per node grows without bound (4 MiB memtable each, peer keys
	// rewritten on every revalidation); the in-memory table does not.
	db := enode.OpenMemoryDB()
	ln := enode.NewLocalNode(db, key)
	ln.SetStaticIP(addr.IP)
	ln.SetFallbackUDP(addr.Port)

	h := &host{idx: idx, key: key, ln: ln, addr: addr, legacy: legacy, sim: sim, settings: settings, boot: boot, refresh: refreshInterval}
	hostsMu.Lock()
	hosts[idx] = h
	hostsMu.Unlock()
	disc := h.start()
	rec := nodeRec{idx: idx, key: key, ln: ln, disc: disc, legacy: legacy}
	registerNodeRec(rec)
	return rec
}

// nodeAddrs, when set, draws addresses from the crawl model (population.addresses: crawl).
var nodeAddrs *addrPool

// nodeIP is the address node idx listens on and advertises. Public-style,
// never LAN, so the IP-diversity defences run: by default 33.(idx/256).(idx%256).1,
// one /24 per node; with the crawl pool, a /24 drawn from the node's topic.
func nodeIP(idx int) net.IP {
	if nodeAddrs != nil {
		if ip := nodeAddrs.ip(idx); ip != nil {
			return ip
		}
	}
	return net.IP{33, byte(idx / 256), byte(idx % 256), 1}
}

// addrPool hands out /24 prefixes from the crawl model's histogram of the
// node's topic, one host address per node within the /24 (254 per /24,
// then another draw), so nodes share prefixes as the crawled network does.
type addrPool struct {
	model   *churn.Model
	k       int
	seed    int64
	topicOf func(idx int) int // -1 when unknown

	mu   sync.Mutex
	used map[uint32]int // hosts handed out per /24
	hint map[int]int    // topic reserved for an index before its spawn (churn joins)
	n    int
}

func newAddrPool(m *churn.Model, k int, seed int64, topicOf func(int) int) *addrPool {
	return &addrPool{model: m, k: k, seed: seed, topicOf: topicOf, used: map[uint32]int{}, hint: map[int]int{}}
}

// reserve records the topic of a node that joins later, so its address is
// drawn from that topic's prefixes.
func (p *addrPool) reserve(idx, topic int) {
	p.mu.Lock()
	p.hint[idx] = topic
	p.mu.Unlock()
}

func (p *addrPool) ip(idx int) net.IP {
	p.mu.Lock()
	defer p.mu.Unlock()
	topic, ok := p.hint[idx]
	if !ok {
		topic = p.topicOf(idx)
	}
	rng := rand.New(rand.NewSource(p.seed*7919 + int64(idx)))
	for try := 0; try < 64; try++ {
		prefix := p.model.DrawPrefix(rng, topic, p.k)
		if prefix == 0 {
			return nil
		}
		if p.used[prefix] >= 254 {
			continue
		}
		p.used[prefix]++
		p.n++
		return net.IP{byte(prefix >> 16), byte(prefix >> 8), byte(prefix), byte(p.used[prefix])}
	}
	return nil
}

// summary: nodes placed, distinct /24s, and the largest /24.
func (p *addrPool) summary() (nodes, prefixes, largest int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, n := range p.used {
		if n > largest {
			largest = n
		}
	}
	return p.n, len(p.used), largest
}

func discoveryConfig(key *ecdsa.PrivateKey, boot []*enode.Node, refreshInterval time.Duration) discover.Config {
	cfg := discover.Config{PrivateKey: key}
	if refreshInterval > 0 {
		cfg.RefreshInterval = refreshInterval
	}
	cfg.Bootnodes = boot
	if nodeAdLifetime > 0 {
		cfg.Topic.AdLifetime = nodeAdLifetime
	}
	if nodeSearchBucketSize > 0 {
		cfg.Topic.SearchBucketSize = nodeSearchBucketSize
	}
	if nodeAdCacheSize > 0 {
		cfg.Topic.AdCacheSize = nodeAdCacheSize
	}
	if nodeTopicNodesLimit > 0 {
		cfg.Topic.TopicNodesLimit = nodeTopicNodesLimit
	}
	if nodeAuxNodesLimit > 0 {
		cfg.Topic.AuxNodesLimit = nodeAuxNodesLimit
	}
	if nodeRegAttemptTimeout > 0 {
		cfg.Topic.RegAttemptTimeout = nodeRegAttemptTimeout
	}
	cfg.Topic.SearchTableDepth = nodeTopic.SearchTableDepth
	cfg.Topic.RegTableDepth = nodeTopic.RegTableDepth
	cfg.Topic.RegBucketSize = nodeTopic.RegBucketSize
	cfg.Topic.RegBucketStandbyLimit = nodeTopic.RegBucketStandby
	cfg.Topic.SearchYieldFloor = nodeTopic.SearchYieldFloor
	cfg.Topic.SearchAuxRadius = nodeTopic.SearchAuxRadius
	return cfg
}

func spawnNodes(sim *simnet.Simnet, settings simnet.NodeBiDiLinkSettings, count int, legacySet map[int]bool, maxBootnodes int, spawnDelay time.Duration, refreshInterval time.Duration, adLifetime time.Duration) []nodeRec {
	nodeAdLifetime = adLifetime
	if maxBootnodes <= 0 {
		maxBootnodes = defaultMaxBootnodes
	}
	rng := rand.New(rand.NewSource(1))
	all := make([]nodeRec, 0, count)
	for i := 0; i < count; i++ {
		if i > 0 && spawnDelay > 0 {
			time.Sleep(spawnDelay)
		}
		boot := sampleBootnodes(all, maxBootnodes, rng)
		rec := spawnNode(sim, settings, i, legacySet[i], boot, refreshInterval)
		all = append(all, rec)
	}
	return all
}
