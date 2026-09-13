package coordinator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/datahop/topdisc-testbed/pkg/assign"
	"github.com/datahop/topdisc-testbed/pkg/host"
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
	AdsHeld        int                         `json:"ads_held"`
	Wire           map[string]map[string]int64 `json:"wire"`
	Legacy         bool                        `json:"legacy"`
	FirstCapableMs int64                       `json:"first_capable_ms"`
}

// Collect reads the per-node traces, prints the run summary and writes
// nodes.json plus oh.json in the format figures_overhead.py reads.
func Collect(trDir, runDir string, as []assign.Assignment) error {
	n := len(as)
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
	var legacyN, legacyLookups, legacyHits int
	var legacyLat, capable []int
	oh := make([]map[string]any, 0, len(traces))
	for _, t := range traces {
		if t.Legacy {
			legacyN++
			for _, l := range t.Lookups {
				legacyLookups++
				legacyLat = append(legacyLat, int(l.LatencyMs))
				if l.HitTarget {
					legacyHits++
				}
			}
			if t.FirstCapableMs >= 0 {
				capable = append(capable, int(t.FirstCapableMs))
			}
			continue
		}
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
	if legacyN > 0 {
		fmt.Printf("legacy nodes=%d (stock discv5)", legacyN)
		if legacyLookups > 0 {
			fmt.Printf("; random-walk lookups=%d hit-target=%d (%.1f%%) latency ms p50=%d p95=%d", legacyLookups, legacyHits, 100*float64(legacyHits)/float64(legacyLookups), pct(legacyLat, 50), pct(legacyLat, 95))
		}
		if len(capable) > 0 {
			fmt.Printf("; first TopDisc-capable peer ms p50=%d p95=%d (%d/%d saw one)", pct(capable, 50), pct(capable, 95), len(capable), legacyN)
		}
		fmt.Println()
	}
	b, _ := json.Marshal(traces)
	os.WriteFile(filepath.Join(runDir, "nodes.json"), b, 0o644)
	b, _ = json.Marshal(oh)
	if err := os.WriteFile(filepath.Join(runDir, "oh.json"), b, 0o644); err != nil {
		return err
	}
	if err := writeMetrics(trDir, runDir, as); err != nil {
		fmt.Println("metrics:", err)
	} else {
		fmt.Printf("metrics written to: %s/m.json, series.json, oh.json\n", runDir)
	}
	return nil
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

// hostSummary prints what the host monitor saw: node CPU and RSS at the busiest
// sample, and whether any host dropped UDP datagrams, which invalidates a run
// the way simnet's link drops do.
func hostSummary(hosts [][]host.Sample) {
	var cpu, rss []int
	drops, dropHosts, samples := int64(0), 0, 0
	for _, hs := range hosts {
		if len(hs) == 0 {
			continue
		}
		samples += len(hs)
		if d := hs[len(hs)-1].UDPRcvbufErrors - hs[0].UDPRcvbufErrors; d > 0 {
			drops += d
			dropHosts++
		}
		for _, s := range hs {
			for _, n := range s.Nodes {
				cpu, rss = append(cpu, int(n.CPUPct)), append(rss, int(n.RSSMB))
			}
		}
	}
	if samples == 0 {
		fmt.Println("hostmetrics: no samples")
		return
	}
	pct := func(v []int, p int) int { sort.Ints(v); return v[(p*(len(v)-1))/100] }
	fmt.Printf("hostmetrics: %d samples over %d hosts; node cpu%% p50=%d p95=%d max=%d; rss MB p50=%d p95=%d max=%d; ",
		samples, len(hosts), pct(cpu, 50), pct(cpu, 95), pct(cpu, 100), pct(rss, 50), pct(rss, 95), pct(rss, 100))
	if drops > 0 {
		fmt.Printf("UDP receive-buffer drops: %d on %d hosts (host saturation; treat timings with care)\n", drops, dropHosts)
	} else {
		fmt.Println("no UDP receive-buffer drops")
	}
}
