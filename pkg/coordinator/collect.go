package coordinator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/datahop/topdisc-testbed/pkg/assign"
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
