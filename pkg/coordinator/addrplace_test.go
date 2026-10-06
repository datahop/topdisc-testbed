package coordinator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/datahop/topdisc-testbed/pkg/addr"
	"github.com/datahop/topdisc-testbed/pkg/assign"
	"github.com/datahop/topdisc-testbed/pkg/scenario"
)

// TestCrawlAddressPlacement checks the three properties a run on crawl
// addresses depends on and that nothing else enforces: every node gets a
// distinct address, a /24 is never split across machines (or the per-/24
// routes between them would be wrong), and no machine is given more nodes
// than it has room for.
func TestCrawlAddressPlacement(t *testing.T) {
	path := filepath.Join("..", "..", "scenarios", "g5k-crawl-25k-6h-realip.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Skip("scenario not present")
	}
	cfg, err := scenario.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	topicIdx, model, err := assign.Topics(cfg, filepath.Join("..", "..", "scenarios"))
	if err != nil {
		t.Fatal(err)
	}
	sc := cfg.Scenario
	pool, err := addr.Draw(sc.Population.Nodes, model, sc.Population.Topics, sc.Population.Seed, func(i int) int {
		if i < len(topicIdx) && len(topicIdx[i]) > 0 {
			return topicIdx[i][len(topicIdx[i])-1]
		}
		return -1
	})
	if err != nil {
		t.Fatal(err)
	}
	nodes, prefixes, largest := pool.Summary()
	t.Logf("%d nodes over %d /24s, largest /24 holds %d", nodes, prefixes, largest)

	var b []byte
	b = append(b, `{"coordinator":"c","hosts":[`...)
	for h := 0; h < 50; h++ {
		if h > 0 {
			b = append(b, ',')
		}
		b = append(b, fmt.Sprintf(`{"index":%d,"ip":"172.16.66.%d","nodes":500}`, h, h+10)...)
	}
	b = append(b, `]}`...)
	var inv Inventory
	if err := json.Unmarshal(b, &inv); err != nil {
		t.Fatal(err)
	}
	hostOf, local, err := place(cfg, inv, pool)
	if err != nil {
		t.Fatal(err)
	}

	seen := map[string]int{}
	for i := 0; i < sc.Population.Nodes; i++ {
		ip := pool.IP(i).String()
		if prev, dup := seen[ip]; dup {
			t.Fatalf("address %s given to nodes %d and %d", ip, prev, i)
		}
		seen[ip] = i
	}
	for pre, idxs := range pool.Groups() {
		h := hostOf[idxs[0]]
		for _, i := range idxs {
			if hostOf[i] != h {
				t.Fatalf("/24 %s split across machines %d and %d", addr.CIDR(pre), h, hostOf[i])
			}
		}
	}
	used := make([]int, len(inv.Hosts))
	for i := 0; i < sc.Population.Nodes; i++ {
		used[hostOf[i]]++
		if local[i] < 0 || local[i] >= inv.Hosts[hostOf[i]].Nodes {
			t.Fatalf("node %d has local slot %d on a machine with %d", i, local[i], inv.Hosts[hostOf[i]].Nodes)
		}
	}
	for h, u := range used {
		if u > inv.Hosts[h].Nodes {
			t.Fatalf("machine %d given %d nodes, room for %d", h, u, inv.Hosts[h].Nodes)
		}
	}
	routes := prefixRoutes(pool, hostOf, inv)
	min, max := len(routes[0]), len(routes[0])
	for _, r := range routes {
		if len(r) < min {
			min = len(r)
		}
		if len(r) > max {
			max = len(r)
		}
	}
	t.Logf("nodes per machine: min %d max %d; routes per machine: min %d max %d",
		minOf(used), maxOf(used), min, max)
	if max != prefixes-minOwned(pool, hostOf, len(inv.Hosts)) && min == 0 {
		t.Fatalf("no routes built")
	}
}

func minOf(v []int) int {
	m := v[0]
	for _, x := range v {
		if x < m {
			m = x
		}
	}
	return m
}
func maxOf(v []int) int {
	m := v[0]
	for _, x := range v {
		if x > m {
			m = x
		}
	}
	return m
}
func minOwned(p *addr.Pool, hostOf []int, hosts int) int {
	own := make([]int, hosts)
	for _, idxs := range p.Groups() {
		own[hostOf[idxs[0]]]++
	}
	return maxOf(own)
}
