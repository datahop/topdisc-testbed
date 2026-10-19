package assign

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/datahop/topdisc-testbed/pkg/scenario"
)

func TestLookupTimes(t *testing.T) {
	at := LookupTimes(rand.New(rand.NewSource(1)), 1000, 11000, 5)
	if len(at) != 5 {
		t.Fatalf("%d times", len(at))
	}
	for k, ms := range at {
		lo, hi := int64(1000+k*2000), int64(1000+(k+1)*2000)
		if ms < lo || ms >= hi {
			t.Fatalf("lookup %d at %d outside its interval [%d,%d)", k, ms, lo, hi)
		}
	}
}

func TestZipfAlphaOne(t *testing.T) {
	z := NewZipf(1.0, 300)
	rng := rand.New(rand.NewSource(1))
	counts := make([]int, 300)
	for i := 0; i < 100000; i++ {
		counts[z.Draw(rng)]++
	}
	// P(0)/P(1) = 2, P(0)/P(9) = 10 for s = 1.
	if r := float64(counts[0]) / float64(counts[1]); r < 1.8 || r > 2.2 {
		t.Fatalf("P0/P1 = %.2f, want 2", r)
	}
	if r := float64(counts[0]) / float64(counts[9]); r < 8.5 || r > 11.5 {
		t.Fatalf("P0/P9 = %.2f, want 10", r)
	}
}

func TestStartOffsets(t *testing.T) {
	o := StartOffsets(1, 10000, 15*time.Minute)
	if o[0] != 0 {
		t.Fatalf("bootnode offset %d, want 0", o[0])
	}
	counts := make([]int, 15)
	for i, v := range o {
		if v < 0 || v > 900000 || (i > 0 && v < o[i-1]) {
			t.Fatalf("offset %d = %d out of window or unsorted", i, v)
		}
		counts[min(14, int(v/60000))]++
	}
	for m, c := range counts {
		if c < 530 || c > 800 {
			t.Fatalf("minute %d holds %d starts, want about 667: not spread evenly", m, c)
		}
	}
	if again := StartOffsets(1, 10000, 15*time.Minute); again[5000] != o[5000] {
		t.Fatal("offsets not reproducible from the seed")
	}
	if z := StartOffsets(1, 3, 0); z[2] != 0 {
		t.Fatalf("zero window: %v", z)
	}
}

// The bootnode (node 0) never gets a churn schedule: a returning node has no
// other node to bootstrap from.
func TestBootnodeNeverChurns(t *testing.T) {
	cfg := scenario.Default()
	cfg.Scenario.Population.Nodes = 200
	cfg.Scenario.Population.Topics = 100
	cfg.Scenario.Population.TopicModel = "crawl-2026-09-18.json"
	cfg.Scenario.Phases.SearchTimeout = 4 * time.Hour
	cfg.Scenario.SessionChurn.Enabled = true
	cfg.Scenario.SessionChurn.Model = "crawl-2026-09-18.json"
	hosts := func(int) Host { return Host{IP: "10.0.0.1", BasePort: 30300} }
	for seed := int64(1); seed <= 20; seed++ {
		cfg.Scenario.Population.Seed = seed
		as, err := Generate(cfg, hosts, 0, "../../scenarios/models")
		if err != nil {
			t.Fatal(err)
		}
		if len(as[0].Churn) != 0 {
			t.Fatalf("seed %d: bootnode has a churn schedule: %v", seed, as[0].Churn)
		}
		churning := 0
		for _, a := range as[1:] {
			if len(a.Churn) > 0 {
				churning++
			}
		}
		if churning == 0 {
			t.Fatalf("seed %d: no node churns: the model was not applied", seed)
		}
	}
}

// legacy_build fork marks every legacy node, the bootnode included, for the
// fork-built binary; the stock build marks none.
func TestLegacyFork(t *testing.T) {
	cfg := scenario.Default()
	cfg.Scenario.Population.Nodes = 50
	cfg.Scenario.Population.LegacyFrac = 1
	cfg.Scenario.Population.LegacyBootnode = true
	hosts := func(int) Host { return Host{IP: "10.0.0.1", BasePort: 30300} }
	for _, build := range []string{"stock", "fork"} {
		cfg.Scenario.Population.LegacyBuild = build
		as, err := Generate(cfg, hosts, 0, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range as {
			if !a.Legacy || a.LegacyFork != (build == "fork") {
				t.Fatalf("%s: node %d legacy=%v legacy_fork=%v", build, a.Idx, a.Legacy, a.LegacyFork)
			}
		}
	}
}

// bootstrap_sample adds earlier starters to the bootnode: never node 0's
// own list, never a node that started less than bootstrapSettle before, the
// bootnode always first, and the same draw for the same seed.
func TestBootstrapSample(t *testing.T) {
	cfg := scenario.Default()
	cfg.Scenario.Population.Nodes = 400
	cfg.Scenario.Population.BootstrapSample = 5
	cfg.Scenario.Phases.StartWindow = 10 * time.Minute
	hosts := func(i int) Host { return Host{IP: "10.0.0.1", BasePort: 30300 + i} }
	as, err := Generate(cfg, hosts, 0, "../../scenarios/models")
	if err != nil {
		t.Fatal(err)
	}
	again, _ := Generate(cfg, hosts, 0, "../../scenarios/models")
	port := func(u string) int {
		var p int
		fmt.Sscanf(u[strings.LastIndex(u, ":")+1:], "%d", &p)
		return p - 30300
	}
	full := 0
	if len(as[0].Bootnodes) != 0 {
		t.Fatalf("bootnode has bootnodes: %v", as[0].Bootnodes)
	}
	for i, a := range as[1:] {
		i++
		if len(a.Bootnodes) == 0 || port(a.Bootnodes[0]) != 0 {
			t.Fatalf("node %d: bootnode not first: %v", i, a.Bootnodes)
		}
		if len(a.Bootnodes) > 6 {
			t.Fatalf("node %d: %d bootstrap records, want at most 6", i, len(a.Bootnodes))
		}
		if len(a.Bootnodes) == 6 {
			full++
		}
		seen := map[int]bool{}
		for _, u := range a.Bootnodes[1:] {
			j := port(u)
			if j <= 0 || j >= i || seen[j] {
				t.Fatalf("node %d: bad or repeated sample %d in %v", i, j, a.Bootnodes)
			}
			seen[j] = true
			if as[j].Phases.StartAt+bootstrapSettle.Milliseconds() > a.Phases.StartAt {
				t.Fatalf("node %d: sampled node %d started only %d ms earlier", i, j, a.Phases.StartAt-as[j].Phases.StartAt)
			}
		}
		if strings.Join(a.Bootnodes, ",") != strings.Join(again[i].Bootnodes, ",") {
			t.Fatalf("node %d: samples differ between two generations of the same seed", i)
		}
	}
	if full < 300 {
		t.Fatalf("%d of 399 nodes got the full 5 samples, want most of them", full)
	}
	cfg.Scenario.Population.BootstrapSample = 0
	off, _ := Generate(cfg, hosts, 0, "../../scenarios/models")
	if len(off[200].Bootnodes) != 1 {
		t.Fatalf("with bootstrap_sample 0 node 200 has %d bootnodes, want the bootnode alone", len(off[200].Bootnodes))
	}
}
