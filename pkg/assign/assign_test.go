package assign

import (
	"testing"
	"time"

	"github.com/datahop/topdisc-testbed/pkg/scenario"
)

func TestDeterministic(t *testing.T) {
	cfg := scenario.Default()
	cfg.Scenario.Population.Nodes = 5
	cfg.Scenario.Population.Seed = 7
	cfg.Scenario.Phases.RegisterStagger = 30 * time.Millisecond
	hosts := func(i int) Host { return Host{IP: "127.0.0.1", BasePort: 30300 + i, StatusOff: 10000} }
	a, err := Generate(cfg, hosts, 1_000_000, ".")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Generate(cfg, hosts, 1_000_000, ".")
	for i := range a {
		if a[i].Key != b[i].Key || a[i].Key == "" {
			t.Fatalf("node %d key not deterministic", i)
		}
	}
	if len(a[0].Bootnodes) != 0 || len(a[1].Bootnodes) != 1 {
		t.Fatalf("bootnode wiring wrong: %v %v", a[0].Bootnodes, a[1].Bootnodes)
	}
	if a[4].Phases.RegisterAt <= a[0].Phases.RegisterAt {
		t.Fatal("register stagger not applied")
	}
}
