package assign

import (
	"bufio"
	"compress/gzip"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/netip"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/datahop/topdisc-testbed/pkg/scenario"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/p2p/discover"
	"github.com/ethereum/go-ethereum/p2p/discover/topicindex"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"
)

// ghostLayout is the number of ghosts per table bucket: index 0 holds every
// distance <= 240 (geth's bucket 0), index 16 distance 256.
type ghostLayout [17]int

const (
	ghostMinDist   = 240
	ghostBucketCap = 16
	ghostSpreadMin = 248 // share mode spreads over 248..256
)

func (l ghostLayout) total() (n int) {
	for _, c := range l {
		n += c
	}
	return n
}

// ghostQuantiles is the measured per-node dead share of mainnet discv5 tables
// (AWS crawl 2026-10-23, 22,937 tables) as (quantile, share) points.
var ghostQuantiles = [][2]float64{{0, 0}, {0.10, 0.021}, {0.25, 0.035}, {0.50, 0.053}, {0.75, 0.130}, {0.90, 0.30}, {1.0, 0.57}}

func ghostQuantile(u float64) float64 {
	q := ghostQuantiles
	for i := 1; i < len(q); i++ {
		if u <= q[i][0] {
			f := (u - q[i-1][0]) / (q[i][0] - q[i-1][0])
			return q[i-1][1] + f*(q[i][1]-q[i-1][1])
		}
	}
	return q[len(q)-1][1]
}

// clippedMean is the mean of min(1, k x curve), exactly: the curve is
// piecewise linear and nondecreasing.
func clippedMean(k float64) (m float64) {
	q := ghostQuantiles
	for i := 1; i < len(q); i++ {
		w, a, b := q[i][0]-q[i-1][0], k*q[i-1][1], k*q[i][1]
		switch {
		case b <= 1:
			m += w * (a + b) / 2
		case a >= 1:
			m += w
		default:
			t := (1 - a) / (b - a)
			m += w * (t*(a+1)/2 + 1 - t)
		}
	}
	return m
}

// ghostScale is the factor on the curve whose clipped mean is share.
func ghostScale(share float64) float64 {
	lo, hi := 0.0, 1.0
	for clippedMean(hi) < share {
		hi *= 2
	}
	for range 60 {
		if mid := (lo + hi) / 2; clippedMean(mid) < share {
			lo = mid
		} else {
			hi = mid
		}
	}
	return (lo + hi) / 2
}

// ghostShare draws one node's share of unreachable table entries.
func ghostShare(rng *rand.Rand, scale float64) float64 {
	return math.Min(1, ghostQuantile(rng.Float64())*scale)
}

// expectedTableSize is the table a node fills in a network of this size:
// bucket d holds min(16, nodes / 2^(257-d)) entries.
func expectedTableSize(nodes int) float64 {
	t := 0.0
	for d := ghostMinDist; d <= 256; d++ {
		t += math.Min(ghostBucketCap, float64(nodes)*math.Pow(2, float64(d-257)))
	}
	return t
}

// sampleGhostLayout draws one node's share from the quantile curve scaled to
// mean share (scale = ghostScale(share)), and spreads that share of the
// expected table uniformly over buckets 248..256.
func sampleGhostLayout(rng *rand.Rand, scale float64, nodes int) ghostLayout {
	var l ghostLayout
	count := int(math.Round(ghostShare(rng, scale) * expectedTableSize(nodes)))
	first := ghostSpreadMin - ghostMinDist
	for ; count > 0; count-- {
		var open []int
		for k := first; k < len(l); k++ {
			if l[k] < ghostBucketCap {
				open = append(open, k)
			}
		}
		if len(open) == 0 {
			break
		}
		l[open[rng.Intn(len(open))]]++
	}
	return l
}

// loadGhostLayouts reads the dead entries per bucket of every node in a crawl's
// tables.json (or .json.gz), streaming: the file is too large to hold. Nodes
// come out sorted by id.
func loadGhostLayouts(path string) ([]ghostLayout, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var r io.Reader = bufio.NewReaderSize(f, 1<<20)
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(r)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		defer gz.Close()
		r = gz
	}
	dec := json.NewDecoder(r)
	fail := func(where string, err error) error { return fmt.Errorf("%s: %s: %v", path, where, err) }
	expect := func(where string, want json.Delim) error {
		tok, err := dec.Token()
		if err != nil {
			return fail(where, err)
		}
		if tok != want {
			return fail(where, fmt.Errorf("got %v, want %v", tok, want))
		}
		return nil
	}
	type entry struct {
		id string
		l  ghostLayout
	}
	var nodes []entry
	seen := false
	if err := expect("top level", '{'); err != nil {
		return nil, err
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, fail("top level", err)
		}
		if tok != "nodes" {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return nil, fail(fmt.Sprint(tok), err)
			}
			continue
		}
		seen = true
		if err := expect("nodes", '{'); err != nil {
			return nil, err
		}
		for dec.More() {
			tok, err := dec.Token()
			if err != nil {
				return nil, fail("nodes", err)
			}
			id, _ := tok.(string)
			var v struct {
				Buckets map[string][]struct {
					Alive *bool `json:"alive"`
				} `json:"buckets"`
			}
			if err := dec.Decode(&v); err != nil {
				return nil, fail("nodes."+id, err)
			}
			if v.Buckets == nil {
				return nil, fail("nodes."+id+".buckets", fmt.Errorf("missing"))
			}
			e := entry{id: id}
			for k, es := range v.Buckets {
				d, err := strconv.Atoi(k)
				if err != nil || d < ghostMinDist || d > 256 {
					return nil, fail("nodes."+id+".buckets."+k, fmt.Errorf("want a distance 240..256"))
				}
				for i, x := range es {
					if x.Alive == nil {
						return nil, fail(fmt.Sprintf("nodes.%s.buckets.%s[%d].alive", id, k, i), fmt.Errorf("missing"))
					}
					if !*x.Alive && e.l[d-ghostMinDist] < ghostBucketCap {
						e.l[d-ghostMinDist]++
					}
				}
			}
			nodes = append(nodes, e)
		}
		if err := expect("nodes", '}'); err != nil {
			return nil, err
		}
	}
	if err := expect("top level", '}'); err != nil {
		return nil, err
	}
	// reading to EOF also makes gzip verify its checksum
	if tok, err := dec.Token(); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("trailing data %v", tok)
		}
		return nil, fail("after top level", err)
	}
	if !seen || len(nodes) == 0 {
		return nil, fail("nodes", fmt.Errorf("no nodes"))
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].id < nodes[j].id })
	out := make([]ghostLayout, len(nodes))
	for i, e := range nodes {
		out[i] = e.l
	}
	return out, nil
}

// ghostPool holds the ghost identities; records are signed on first use.
type ghostPool struct {
	keys []*ecdsa.PrivateKey
	ids  []enode.ID
	ip   []netip.Addr
	enr  []string
}

func newGhostPool(seed int64, size, nodes int, hosts func(int) Host) (*ghostPool, error) {
	p := &ghostPool{keys: make([]*ecdsa.PrivateKey, size), ids: make([]enode.ID, size), ip: make([]netip.Addr, size), enr: make([]string, size)}
	for j := range p.keys {
		for ctr := 0; ; ctr++ {
			d := crypto.Keccak256([]byte(fmt.Sprintf("topdisc-testbed/ghost/%d/%d/%d", seed, j, ctr)))
			if k, err := crypto.ToECDSA(d); err == nil {
				p.keys[j] = k
				break
			}
		}
		p.ids[j] = enode.PubkeyToIDV4(&p.keys[j].PublicKey)
		h := hosts(j % nodes)
		ip := h.IP
		if h.ExtIP != "" {
			ip = h.ExtIP
		}
		addr, err := netip.ParseAddr(ip)
		if err != nil || !addr.Is4() {
			return nil, fmt.Errorf("ghost %d: node %d address %q is not IPv4", j, j%nodes, ip)
		}
		p.ip[j] = addr
	}
	return p, nil
}

func (p *ghostPool) record(j int) (string, error) {
	if p.enr[j] != "" {
		return p.enr[j], nil
	}
	var r enr.Record
	r.Set(enr.IPv4Addr(p.ip[j]))
	r.Set(enr.UDP(scenario.GhostPortBase + j%scenario.GhostPortSpan))
	r.Set(topicindex.TopicDiscoveryVersion)
	r.Set(discover.TestbedGhostMark)
	if err := enode.SignV4(&r, p.keys[j]); err != nil {
		return "", err
	}
	n, err := enode.New(enode.ValidSchemes, &r)
	if err != nil {
		return "", err
	}
	p.enr[j] = n.String()
	return p.enr[j], nil
}

func ghostBucket(self, id enode.ID) int {
	return max(enode.LogDist(self, id), ghostMinDist) - ghostMinDist
}

// assignGhosts fills every assignment's Ghosts: node i's layout (replayed from
// the crawl's tables, or sampled from share) is met with pool ghosts at the
// right distance from it, drawn without replacement. Requests a bucket has
// too few candidates for are counted in GhostsUnplaced.
func assignGhosts(as []Assignment, keys []*ecdsa.PrivateKey, cfg scenario.Config, hosts func(int) Host, modelDir string) error {
	g, n := cfg.Scenario.Ghosts, len(as)
	if !g.On() || n == 0 {
		return nil
	}
	seed := cfg.Scenario.Population.Seed
	rng := rand.New(rand.NewSource(seed + 7177))
	var replay []ghostLayout
	if g.Tables != "" {
		var err error
		if replay, err = loadGhostLayouts(g.TablesPath(modelDir)); err != nil {
			return err
		}
	}
	size := g.Pool
	if size == 0 {
		size = max(2*n, 1024)
	}
	pool, err := newGhostPool(seed, size, n, hosts)
	if err != nil {
		return err
	}
	scale := ghostScale(g.Share)
	var cands [17][]int
	for i := range as {
		var layout ghostLayout
		if replay != nil {
			layout = replay[i%len(replay)]
		} else {
			layout = sampleGhostLayout(rng, scale, n)
		}
		if layout.total() == 0 {
			continue
		}
		self := enode.PubkeyToIDV4(&keys[i].PublicKey)
		for k := range cands {
			cands[k] = cands[k][:0]
		}
		for j, id := range pool.ids {
			k := ghostBucket(self, id)
			cands[k] = append(cands[k], j)
		}
		for k, want := range layout {
			c := cands[k]
			take := min(want, len(c))
			as[i].GhostsUnplaced += want - take
			for t := 0; t < take; t++ {
				x := t + rng.Intn(len(c)-t)
				c[t], c[x] = c[x], c[t]
				s, err := pool.record(c[t])
				if err != nil {
					return fmt.Errorf("ghost %d: %w", c[t], err)
				}
				as[i].Ghosts = append(as[i].Ghosts, s)
			}
		}
	}
	return nil
}

// GhostSummary is the run log's line on the pinned ghosts, or "" when the
// scenario has none. The mean share is of the expected table size.
func GhostSummary(as []Assignment) string {
	pinned, unplaced, share := 0, 0, 0.0
	for _, a := range as {
		pinned += len(a.Ghosts)
		unplaced += a.GhostsUnplaced
		share += float64(len(a.Ghosts))
	}
	if pinned == 0 && unplaced == 0 {
		return ""
	}
	share /= float64(len(as)) * expectedTableSize(len(as))
	return fmt.Sprintf("ghosts: %d nodes, %d pinned (mean share %.1f %%), %d unplaced", len(as), pinned, 100*share, unplaced)
}
