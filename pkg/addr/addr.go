// Package addr draws node addresses from a crawl model's per-topic histogram
// of /24 prefixes, so a run sees the address diversity the crawled network
// actually had.
//
// This matters because three defences in discv5 are keyed on addresses and are
// dormant when nodes sit in private space: the per-/24 limits on the routing
// table (2 per bucket, 10 per table), the one-registrar-per-/24 limit on a
// registration bucket, and the ad cache's ipTree admission score. The ipTree
// is the reason the real addresses are used rather than a remapping onto some
// convenient block: it is a binary trie walked from the most significant bit
// that compares each subtree's population against a uniform one, so it reads
// the whole 32-bit path and the global distribution across it. Remapping every
// node into one /10 makes them share the first ten bits and pins the score at
// its maximum for every node; scattering the /24s uniformly instead destroys
// the clustering across /8s and /16s that the trie exists to detect, and
// understates the score. Only the crawled addresses give the right answer.
//
// The crawl holds only publicly routable addresses, so none of them collide
// with the testbed's own networks or with any range geth classifies as LAN.
// They never leave the network namespaces they are assigned to: the only
// routes installed are explicit per-/24 routes between the run's machines, and
// a namespace has no path to the outside.
package addr

import (
	"fmt"
	"math/rand"
	"net"

	"github.com/datahop/topdisc-testbed/pkg/churn"
)

// Pool holds one address per node, drawn once so that placement, assignment
// and routing all agree on which node is where in the address space.
type Pool struct {
	ip     []net.IP
	prefix []uint32 // the /24 of ip[i], as a.b.c
	used   map[uint32]int
}

// Draw assigns every node an address. A node's /24 comes from the histogram of
// its own topic, so nodes of a topic share prefixes the way that topic's
// crawled nodes did; within a /24 the host octets are handed out in order, and
// a prefix that runs out of them (254) is redrawn.
func Draw(n int, m *churn.Model, numTopics int, seed int64, topicOf func(i int) int) (*Pool, error) {
	if m == nil || !m.HasAddresses() {
		return nil, fmt.Errorf("population.addresses crawl: the topic model carries no /24 prefixes (regenerate it with model.py --nodes)")
	}
	p := &Pool{ip: make([]net.IP, n), prefix: make([]uint32, n), used: map[uint32]int{}}
	for i := 0; i < n; i++ {
		rng := rand.New(rand.NewSource(seed*7919 + int64(i)))
		for try := 0; ; try++ {
			if try == 64 {
				return nil, fmt.Errorf("population.addresses crawl: no free host address for node %d after 64 draws", i)
			}
			pre := m.DrawPrefix(rng, topicOf(i), numTopics)
			if pre == 0 {
				return nil, fmt.Errorf("population.addresses crawl: topic %d has no prefixes", topicOf(i))
			}
			if p.used[pre] >= 254 {
				continue
			}
			p.used[pre]++
			p.ip[i] = net.IP{byte(pre >> 16), byte(pre >> 8), byte(pre), byte(p.used[pre])}
			p.prefix[i] = pre
			break
		}
	}
	return p, nil
}

// IP is node i's address.
func (p *Pool) IP(i int) net.IP { return p.ip[i] }

// Prefix is node i's /24, packed as a<<16|b<<8|c.
func (p *Pool) Prefix(i int) uint32 { return p.prefix[i] }

// CIDR renders a /24 as a route destination.
func CIDR(prefix uint32) string {
	return fmt.Sprintf("%d.%d.%d.0/24", byte(prefix>>16), byte(prefix>>8), byte(prefix))
}

// Groups returns the node indices of each /24, keyed by prefix. Placement
// keeps a /24 on one machine so that routes between machines stay per-/24
// rather than per-node.
func (p *Pool) Groups() map[uint32][]int {
	g := make(map[uint32][]int, len(p.used))
	for i, pre := range p.prefix {
		g[pre] = append(g[pre], i)
	}
	return g
}

// Summary reports the address diversity the run will see.
func (p *Pool) Summary() (nodes, prefixes, largest int) {
	for _, n := range p.used {
		nodes += n
		if n > largest {
			largest = n
		}
	}
	return nodes, len(p.used), largest
}
