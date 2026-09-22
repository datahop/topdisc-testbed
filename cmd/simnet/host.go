package main

import (
	"crypto/ecdsa"
	"net"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/p2p/discover"
	"github.com/ethereum/go-ethereum/p2p/discover/topicindex"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/marcopolo/simnet"
)

// host is a simulated node's identity and place in the network across
// restarts. Session churn stops the node's discovery service when the
// schedule takes it away, so its ads expire on the registrars and it answers
// nothing, and starts a new service on the same key and address when it
// returns, as a real node restart would.
type host struct {
	idx      int
	key      *ecdsa.PrivateKey
	ln       *enode.LocalNode
	addr     *net.UDPAddr
	legacy   bool
	sim      *simnet.Simnet
	settings simnet.NodeBiDiLinkSettings
	boot     []*enode.Node
	refresh  time.Duration

	mu       sync.Mutex
	disc     *discover.UDPv5      // nil while the node is away
	topics   []topicindex.TopicID // registered again on every start
	restarts int
	// Counters of stopped instances, so a node's totals span its restarts.
	wire  map[string]discover.WireCounter
	ops   map[discover.OpKey]discover.OpCounter
	loads map[topicindex.TopicID]discover.TopicLoad
}

var (
	hostsMu sync.RWMutex
	hosts   = map[int]*host{}
)

func hostOf(idx int) *host {
	hostsMu.RLock()
	defer hostsMu.RUnlock()
	return hosts[idx]
}

// currentDisc is the node's running discovery service, nil while it is away.
func currentDisc(idx int) *discover.UDPv5 {
	h := hostOf(idx)
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.disc
}

// localTopicNodes is what the node's topic table holds; nothing while away.
func localTopicNodes(idx int, topic topicindex.TopicID) []*enode.Node {
	if d := currentDisc(idx); d != nil {
		return d.LocalTopicNodes(topic)
	}
	return nil
}

// start opens the endpoint and the discovery service. The router replaces
// the route to the address, so a restarted node is reachable where it was.
func (h *host) start() *discover.UDPv5 {
	conn := &simUDPConn{SimConn: h.sim.NewEndpoint(h.addr, h.settings), idx: h.idx}
	registerConn(conn)
	disc, err := discover.ListenV5(conn, h.ln, discoveryConfig(h.key, h.boot, h.refresh))
	if err != nil {
		fatalf("listen v5 on node %d: %v", h.idx, err)
	}
	if h.legacy {
		h.ln.Delete(new(topicindex.TopicDiscovery))
	}
	h.mu.Lock()
	h.disc = disc
	topics := h.topics
	h.mu.Unlock()
	for _, t := range topics {
		disc.RegisterTopic(t, uint64(h.idx))
	}
	return disc
}

// stop shuts the discovery service down, keeping its counters.
func (h *host) stop() {
	h.mu.Lock()
	disc := h.disc
	h.disc = nil
	h.mu.Unlock()
	if disc == nil {
		return
	}
	h.absorb(disc)
	disc.Close()
	h.mu.Lock()
	h.restarts++
	h.mu.Unlock()
}

func (h *host) setTopics(ts []topicindex.TopicID) {
	h.mu.Lock()
	h.topics = append([]topicindex.TopicID(nil), ts...)
	h.mu.Unlock()
}

func addWire(a, b discover.WireCounter) discover.WireCounter {
	a.TxMsgs += b.TxMsgs
	a.TxBytes += b.TxBytes
	a.RxMsgs += b.RxMsgs
	a.RxBytes += b.RxBytes
	return a
}

func mergeStats(wire map[string]discover.WireCounter, ops map[discover.OpKey]discover.OpCounter,
	loads map[topicindex.TopicID]discover.TopicLoad, d *discover.UDPv5) {
	for k, v := range d.WireStats() {
		wire[k] = addWire(wire[k], v)
	}
	for k, v := range d.OpStats() {
		o := ops[k]
		o.WireCounter = addWire(o.WireCounter, v.WireCounter)
		o.Nodes += v.Nodes
		ops[k] = o
	}
	for k, v := range d.TopicLoadStats() {
		l := loads[k]
		l.Regtopic = addWire(l.Regtopic, v.Regtopic)
		l.TopicQuery = addWire(l.TopicQuery, v.TopicQuery)
		loads[k] = l
	}
}

// absorb adds a stopped instance's counters to the host's totals.
func (h *host) absorb(d *discover.UDPv5) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.wire == nil {
		h.wire = map[string]discover.WireCounter{}
		h.ops = map[discover.OpKey]discover.OpCounter{}
		h.loads = map[topicindex.TopicID]discover.TopicLoad{}
	}
	mergeStats(h.wire, h.ops, h.loads, d)
}

// stats returns the node's counters over every instance it has run.
func (h *host) stats() (map[string]discover.WireCounter, map[discover.OpKey]discover.OpCounter, map[topicindex.TopicID]discover.TopicLoad) {
	h.mu.Lock()
	disc := h.disc
	wire := make(map[string]discover.WireCounter, len(h.wire))
	ops := make(map[discover.OpKey]discover.OpCounter, len(h.ops))
	loads := make(map[topicindex.TopicID]discover.TopicLoad, len(h.loads))
	for k, v := range h.wire {
		wire[k] = v
	}
	for k, v := range h.ops {
		ops[k] = v
	}
	for k, v := range h.loads {
		loads[k] = v
	}
	h.mu.Unlock()
	if disc != nil {
		mergeStats(wire, ops, loads, disc)
	}
	return wire, ops, loads
}

// hostRestarts counts the departures that stopped a node so far.
func hostRestarts() (n int) {
	hostsMu.RLock()
	defer hostsMu.RUnlock()
	for _, h := range hosts {
		h.mu.Lock()
		n += h.restarts
		h.mu.Unlock()
	}
	return n
}
