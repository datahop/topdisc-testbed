// Package coordinator turns a scenario into a running experiment on a backend
// and collects what the nodes recorded.
package coordinator

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/datahop/topdisc-testbed/pkg/assign"
	"github.com/datahop/topdisc-testbed/pkg/churn"
	"github.com/datahop/topdisc-testbed/pkg/scenario"
)

// NodeTrace is what cmd/node writes at StopAt.
type NodeTrace struct {
	Idx       int     `json:"idx"`
	ID        string  `json:"id"`
	Outbound  int     `json:"outbound"`
	Inbound   int     `json:"inbound"`
	PeerDrops int     `json:"peer_drops"`
	RefillMs  []int64 `json:"refill_ms"`
	Lookups   []struct {
		LatencyMs int64 `json:"latency_ms"`
		Results   int   `json:"results"`
		HitTarget bool  `json:"hit_target"`
	} `json:"lookups"`
	AdsHeld int                         `json:"ads_held"`
	Wire    map[string]map[string]int64 `json:"wire"`
}

// RunLocal executes the scenario as one node process per node on this host.
// runDir already exists (scenario.PrepareRun); assignments, logs and traces go
// under it.
func RunLocal(cfg scenario.Config, runDir string) error {
	sc, lc := cfg.Scenario, cfg.Testbed.Local
	n := sc.Population.Nodes
	asgDir, logDir, trDir := filepath.Join(runDir, "assignments"), filepath.Join(runDir, "logs"), filepath.Join(runDir, "traces")
	for _, d := range []string{asgDir, logDir, trDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	t0 := time.Now().Add(2 * time.Second).UnixMilli()
	as, err := assign.Generate(cfg, func(i int) assign.Host {
		return assign.Host{IP: "127.0.0.1", BasePort: lc.BasePort + i, StatusOff: 10000, TraceFile: filepath.Join(trDir, fmt.Sprintf("node%d.json", i))}
	}, t0, filepath.Dir(cfg.SourcePath))
	if err != nil {
		return err
	}
	if err := assign.Write(asgDir, as); err != nil {
		return err
	}
	fmt.Printf("local backend: %d nodes, bootnode %s, phases register@+%s search@+%s stop@+%s\n", n, as[1].Bootnodes[0][:24]+"…",
		time.Duration(as[0].Phases.RegisterAt-t0)*time.Millisecond, time.Duration(as[0].Phases.SearchAt-t0)*time.Millisecond, time.Duration(as[0].Phases.StopAt-t0)*time.Millisecond)

	procs := make([]*exec.Cmd, n)
	for i := range as {
		logf, err := os.Create(filepath.Join(logDir, fmt.Sprintf("node%d.log", i)))
		if err != nil {
			return err
		}
		c := exec.Command(lc.NodeBinary, "-assignment", filepath.Join(asgDir, fmt.Sprintf("node%d.json", i)), "-v", "2")
		c.Stdout, c.Stderr = logf, logf
		if err := c.Start(); err != nil {
			return fmt.Errorf("node %d: %w", i, err)
		}
		procs[i] = c
		if i == 0 {
			time.Sleep(time.Second) // bootnode first
		}
	}
	// Churn: kill at DownAt, restart the same identity at UpAt. A restarted
	// node re-registers and re-searches at once (its phase times are past).
	searchAt := time.UnixMilli(as[0].Phases.SearchAt)
	var churnMu sync.Mutex
	departs, rejoins := 0, 0
	for i, a := range as {
		if len(a.Churn) == 0 {
			continue
		}
		go func(i int, events []churn.Event) {
			for _, ev := range events {
				time.Sleep(time.Until(searchAt.Add(time.Duration(ev.DownAt * float64(time.Second)))))
				churnMu.Lock()
				if procs[i] != nil && procs[i].Process != nil {
					procs[i].Process.Kill()
					procs[i].Wait()
					departs++
				}
				churnMu.Unlock()
				time.Sleep(time.Until(searchAt.Add(time.Duration(ev.UpAt * float64(time.Second)))))
				logf, err := os.OpenFile(filepath.Join(logDir, fmt.Sprintf("node%d.log", i)), os.O_APPEND|os.O_WRONLY, 0o644)
				if err != nil {
					return
				}
				c := exec.Command(lc.NodeBinary, "-assignment", filepath.Join(asgDir, fmt.Sprintf("node%d.json", i)), "-v", "2")
				c.Stdout, c.Stderr = logf, logf
				churnMu.Lock()
				if c.Start() == nil {
					procs[i] = c
					rejoins++
				}
				churnMu.Unlock()
			}
		}(i, a.Churn)
	}
	stop := time.UnixMilli(as[0].Phases.StopAt).Add(lc.Grace)
	fmt.Printf("nodes started; collecting at %s\n", stop.Format(time.TimeOnly))
	for time.Now().Before(stop) {
		time.Sleep(30 * time.Second)
		churnMu.Lock()
		if departs > 0 {
			fmt.Printf("[churn] departs=%d rejoins=%d\n", departs, rejoins)
		}
		churnMu.Unlock()
	}
	churnMu.Lock()
	fmt.Printf("churn applied: departs=%d rejoins=%d (planned %d)\n", departs, rejoins, planned(as))
	churnMu.Unlock()
	churnMu.Lock()
	for _, c := range procs {
		if c != nil && c.ProcessState == nil && c.Process != nil {
			c.Process.Kill()
		}
	}
	churnMu.Unlock()
	return Collect(trDir, runDir, n)
}

// Collect reads the per-node traces, prints the run summary and writes
// nodes.json plus oh.json in the format figures_overhead.py reads.
func Collect(trDir, runDir string, n int) error {
	var traces []NodeTrace
	for i := 0; i < n; i++ {
		b, err := os.ReadFile(filepath.Join(trDir, fmt.Sprintf("node%d.json", i)))
		if err != nil {
			continue
		}
		var t NodeTrace
		if json.Unmarshal(b, &t) == nil {
			traces = append(traces, t)
		}
	}
	if len(traces) == 0 {
		return fmt.Errorf("no node traces collected")
	}
	pct := func(v []int, p int) int { sort.Ints(v); return v[(p*(len(v)-1))/100] }
	var out, in, ads, drops []int
	var refills []int
	var lookups, hits int
	var lat []int
	oh := make([]map[string]any, 0, len(traces))
	for _, t := range traces {
		out, in, ads, drops = append(out, t.Outbound), append(in, t.Inbound), append(ads, t.AdsHeld), append(drops, t.PeerDrops)
		for _, r := range t.RefillMs {
			refills = append(refills, int(r))
		}
		for _, l := range t.Lookups {
			lookups++
			lat = append(lat, int(l.LatencyMs))
			if l.HitTarget {
				hits++
			}
		}
		var tx, rx, txp, rxp int64
		bt := map[string]any{}
		for k, v := range t.Wire {
			tx, rx, txp, rxp = tx+v["txBytes"], rx+v["rxBytes"], txp+v["txMsgs"], rxp+v["rxMsgs"]
			bt[k] = v
		}
		oh = append(oh, map[string]any{"idx": t.Idx, "id": t.ID, "txBytes": tx, "rxBytes": rx, "txPkts": txp, "rxPkts": rxp, "byType": bt})
	}
	full := 0
	for _, o := range out {
		if o >= 16 {
			full++
		}
	}
	fmt.Printf("=== nodes (%d traces) ===\n", len(traces))
	fmt.Printf("outbound: p5=%d p50=%d p95=%d; full(>=16)=%d/%d\n", pct(out, 5), pct(out, 50), pct(out, 95), full, len(out))
	fmt.Printf("inbound:  p5=%d p50=%d p95=%d max=%d\n", pct(in, 5), pct(in, 50), pct(in, 95), pct(in, 100))
	fmt.Printf("ads held per node: p50=%d total=%d; peer drops total=%d; refills=%d", pct(ads, 50), sum(ads), sum(drops), len(refills))
	if len(refills) > 0 {
		fmt.Printf(" p50=%dms p95=%dms", pct(refills, 50), pct(refills, 95))
	}
	fmt.Println()
	if lookups > 0 {
		fmt.Printf("lookups=%d hit-target=%d (%.1f%%) latency ms p50=%d p95=%d\n", lookups, hits, 100*float64(hits)/float64(lookups), pct(lat, 50), pct(lat, 95))
	}
	b, _ := json.Marshal(traces)
	os.WriteFile(filepath.Join(runDir, "nodes.json"), b, 0o644)
	b, _ = json.Marshal(oh)
	return os.WriteFile(filepath.Join(runDir, "oh.json"), b, 0o644)
}

func planned(as []assign.Assignment) int {
	n := 0
	for _, a := range as {
		n += len(a.Churn)
	}
	return n
}

func sum(v []int) int {
	s := 0
	for _, x := range v {
		s += x
	}
	return s
}
