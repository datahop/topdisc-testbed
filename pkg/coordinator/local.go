// Package coordinator turns a scenario into a running experiment on a backend
// and collects what the nodes recorded.
package coordinator

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/datahop/topdisc-testbed/pkg/assign"
	"github.com/datahop/topdisc-testbed/pkg/host"
	"github.com/datahop/topdisc-testbed/pkg/scenario"
)

// RunLocal executes the scenario as one node process per node on this host.
// runDir already exists (scenario.PrepareRun); assignments, logs and traces go
// under it.
func RunLocal(cfg scenario.Config, runDir string) error {
	sc, lc := cfg.Scenario, cfg.Testbed.Local
	n := sc.Population.Nodes
	trDir := filepath.Join(runDir, "traces")
	if err := os.MkdirAll(trDir, 0o755); err != nil {
		return err
	}
	star := cfg.Testbed.Wan.Star()
	t0 := time.Now().Add(2 * time.Second).UnixMilli()
	as, err := assign.Generate(cfg, func(i int) assign.Host {
		ip := "127.0.0.1"
		if star != nil {
			ip = host.NodeIP(0, i)
		}
		return assign.Host{IP: ip, BasePort: lc.BasePort + i, StatusOff: 10000, TraceFile: filepath.Join(trDir, fmt.Sprintf("node%d.json", i))}
	}, t0, filepath.Dir(cfg.SourcePath))
	if err != nil {
		return err
	}
	r := &host.Runner{NodeBinary: lc.NodeBinary, Verbosity: lc.Verbosity, AsgDir: filepath.Join(runDir, "assignments"),
		LogDir: filepath.Join(runDir, "logs"), Wan: star, Seed: sc.Population.Seed}
	if err := r.Prepare(as, nil); err != nil {
		return err
	}
	defer r.StopAll()
	printPhases("local", as, t0)
	if star != nil {
		fmt.Printf("wan: star model, one-way %g..%g ms, %d kbit/s per node, netns per node\n", star.MinMs, star.MaxMs, star.RateKbps)
	}
	ctl := hostControl{
		start: func(idx int) error { return r.Start(idx) },
		kill:  func(idx int) bool { return r.Kill(idx) },
	}
	if err := drive(as, ctl, lc.Grace); err != nil {
		return err
	}
	return Collect(trDir, runDir, n)
}
