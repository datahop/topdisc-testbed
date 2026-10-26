package coordinator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

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
	Ops            []map[string]any            `json:"ops"`
	TopicLoad      map[string]any              `json:"topic_load"`
	Sessions       []struct {
		StartMs int64 `json:"start_ms"`
		EndMs   int64 `json:"end_ms"`
	} `json:"sessions"` // the node's online periods
	GhostsAssigned int    `json:"ghosts_assigned"`
	GhostsInTable  int    `json:"ghosts_in_table"` // last session's table
	TableSize      int    `json:"table_size"`
	Fork           string `json:"fork"` // go-ethereum module of a legacy node's binary
}

// Collect reads the per-node traces, prints the run summary and writes
// nodes.json plus oh.json in the format figures_overhead.py reads.
func Collect(trDir, runDir string, as []assign.Assignment) error {
	n := len(as)
	var traces []NodeTrace
	for i := 0; i < n; i++ {
		b, err := host.LoadTrace(filepath.Join(trDir, fmt.Sprintf("node%d.json", i)))
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
	pct := func(v []int, p int) int {
		if len(v) == 0 {
			return 0
		}
		sort.Ints(v)
		return v[(p*(len(v)-1))/100]
	}
	var out, in, ads, drops []int
	var refills []int
	var lookups, hits int
	var lat []int
	var legacyN, legacyLookups, legacyHits int
	var legacyLat, capable []int
	legacyForks := map[string]bool{}
	oh := make([]map[string]any, 0, len(traces))
	for _, t := range traces {
		if t.Legacy {
			legacyN++
			legacyForks[t.Fork] = true
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
		}
		// Peers and wire counters count for every node: leaving the legacy
		// ones out left an all-legacy run with peer stats and oh.json for
		// the bootnode alone.
		out, in, ads, drops = append(out, t.Outbound), append(in, t.Inbound), append(ads, t.AdsHeld), append(drops, t.PeerDrops)
		for _, r := range t.RefillMs {
			refills = append(refills, int(r))
		}
		for _, l := range t.Lookups {
			if t.Legacy {
				break
			}
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
		// The per-operation peer lists stay in the traces (checks.py reads them
		// there); at 25k nodes they made oh.json too large to load.
		ops := make([]map[string]any, 0, len(t.Ops))
		for _, op := range t.Ops {
			o := make(map[string]any, len(op))
			for k, v := range op {
				if k != "nodeIds" {
					o[k] = v
				}
			}
			ops = append(ops, o)
		}
		oh = append(oh, map[string]any{"idx": t.Idx, "id": t.ID, "txBytes": tx, "rxBytes": rx, "txPkts": txp, "rxPkts": rxp, "byType": bt, "ops": ops, "topicLoad": t.TopicLoad})
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
		forks := make([]string, 0, len(legacyForks))
		for f := range legacyForks {
			forks = append(forks, f)
		}
		sort.Strings(forks)
		fmt.Printf("legacy nodes=%d (discv5 %s)", legacyN, strings.Join(forks, ", "))
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
		fmt.Printf("metrics written to: %s/metrics.json, series.json, oh.json\n", runDir)
	}
	return nil
}

// planned counts the churn calls drive can apply: a departure before StopAt
// that is not an arrival, and a return before StopAt.
func planned(as []assign.Assignment) (departs, rejoins int) {
	searchAt, stopAt := time.UnixMilli(as[0].Phases.SearchAt), time.UnixMilli(as[0].Phases.StopAt)
	for _, a := range as {
		for _, ev := range a.Churn {
			down, up := churnTimes(searchAt, ev)
			if !down.Before(stopAt) {
				break
			}
			if ev.DownAt > 0 {
				departs++
			}
			if !up.Before(stopAt) {
				break
			}
			rejoins++
		}
	}
	return departs, rejoins
}

func sum(v []int) int {
	s := 0
	for _, x := range v {
		s += x
	}
	return s
}

// hostSummary prints what the host monitor saw: node CPU and RSS at the busiest
// sample, and where hosts dropped packets, which invalidates a run the way
// simnet's link drops do.
func hostSummary(hosts [][]host.Sample) {
	var cpu, rss []int
	samples := 0
	for _, hs := range hosts {
		samples += len(hs)
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
	pct := func(v []int, p int) int {
		if len(v) == 0 {
			return 0
		}
		sort.Ints(v)
		return v[(p*(len(v)-1))/100]
	}
	fmt.Printf("hostmetrics: %d samples over %d hosts; node cpu%% p50=%d p95=%d max=%d; rss MB p50=%d p95=%d max=%d; ",
		samples, len(hosts), pct(cpu, 50), pct(cpu, 95), pct(cpu, 100), pct(rss, 50), pct(rss, 95), pct(rss, 100))
	fmt.Println(dropLine(hostDrops(hosts)))
}

// dropCount is one category of host drops.
type dropCount struct {
	What  string
	N     int64
	Hosts int // with any
}

// hostDrops applies figures/checks.py's host_drops rules (H1), which
// figures/testdata/h1-cases.json pins for both: a counter's rise is summed
// between the samples that measured it, a fall being a reset after which the
// new value is all rise; a host whose first or last sample did not measure a
// source, that lacks a source other hosts have, or that sent no samples, is a
// coverage gap for it. Categories come in a fixed order; gaps by source.
func hostDrops(hosts [][]host.Sample) (cats []dropCount, gaps map[string]int) {
	var live [][]host.Sample
	empty := 0
	for _, hs := range hosts {
		if len(hs) == 0 {
			empty++
		} else {
			live = append(live, hs)
		}
	}
	any := func(hs []host.Sample, f func(host.Sample) bool) bool {
		for _, s := range hs {
			if f(s) {
				return true
			}
		}
		return false
	}
	measured := func(hs []host.Sample, f func(host.Sample) bool) []bool {
		ok := make([]bool, len(hs))
		for i, s := range hs {
			ok[i] = f(s)
		}
		return ok
	}
	covered := func(ok []bool) bool { return len(ok) >= 2 && ok[0] && ok[len(ok)-1] }
	increase := func(hs []host.Sample, ok []bool, v func(host.Sample) int64) int64 {
		var tot, prev int64
		seen := false
		for i, s := range hs {
			if !ok[i] {
				continue
			}
			x := v(s)
			if seen {
				if x >= prev {
					tot += x - prev
				} else {
					tot += x
				}
			}
			prev, seen = x, true
		}
		return tot
	}
	gaps = map[string]int{}
	gap := func(src string, n int) {
		if n > 0 {
			gaps[src] += n
		}
	}
	hasNetns := func(s host.Sample) bool { return s.Netns != nil || s.NetnsExpected > 0 }
	hasARP := func(s host.Sample) bool { return s.ARP != nil }
	enaHost := func(hs []host.Sample) bool {
		return any(hs, func(s host.Sample) bool { return s.ENAIface != "" }) || hs[len(hs)-1].ENAError != ""
	}
	var netnsHosts, arpHosts, enaHosts, noNetns, noARP int
	for _, hs := range live {
		if any(hs, hasNetns) {
			netnsHosts++
		} else {
			noNetns++
		}
		if any(hs, hasARP) {
			arpHosts++
		} else {
			noARP++
		}
		if enaHost(hs) {
			enaHosts++
		}
	}
	if netnsHosts > 0 {
		var qdisc, rcvbuf int64
		gap("netns", noNetns+empty)
		for _, hs := range live {
			if !any(hs, hasNetns) {
				continue
			}
			top := 0
			for _, s := range hs {
				top = max(top, s.NetnsSampled)
			}
			ok := measured(hs, func(s host.Sample) bool {
				want := s.NetnsExpected
				if want == 0 {
					want = top
				}
				return s.Netns != nil && s.NetnsSampled > 0 && s.NetnsSampled == want
			})
			if !covered(ok) {
				gap("netns", 1)
			}
			qdisc += increase(hs, ok, func(s host.Sample) int64 { return s.Netns.QdiscDrops })
			rcvbuf += increase(hs, ok, func(s host.Sample) int64 { return s.Netns.UDPRcvbufErrors })
		}
		cats = append(cats, dropCount{What: "netem", N: qdisc}, dropCount{What: "namespace UDP receive buffer", N: rcvbuf})
	}
	if arpHosts > 0 {
		var arp int64
		gap("ARP", noARP+empty)
		for _, hs := range live {
			if !any(hs, hasARP) {
				continue
			}
			ok := measured(hs, hasARP)
			if !covered(ok) {
				gap("ARP", 1)
			}
			arp += increase(hs, ok, func(s host.Sample) int64 { return s.ARP.UnresolvedDiscards })
			arp += increase(hs, ok, func(s host.Sample) int64 { return s.ARP.TableFulls })
		}
		cats = append(cats, dropCount{What: "ARP", N: arp})
	}
	if enaHosts > 0 {
		var ena int64
		gap("ENA", empty)
		for _, hs := range live {
			if !enaHost(hs) {
				continue
			}
			ok := measured(hs, func(s host.Sample) bool { return s.ENAIface != "" })
			if !covered(ok) {
				gap("ENA", 1)
			}
			for _, v := range []func(host.Sample) int64{
				func(s host.Sample) int64 { return s.PPSAllowanceExceeded },
				func(s host.Sample) int64 { return s.BWInAllowanceExceeded },
				func(s host.Sample) int64 { return s.BWOutAllowanceExceeded },
				func(s host.Sample) int64 { return s.ConntrackAllowanceExceeded },
				func(s host.Sample) int64 { return s.LinklocalAllowanceExceeded },
			} {
				ena += increase(hs, ok, v)
			}
		}
		cats = append(cats, dropCount{What: "ENA allowance", N: ena})
	}
	udp := dropCount{What: "host UDP receive buffer"}
	for _, hs := range live {
		if n := increase(hs, measured(hs, func(host.Sample) bool { return true }), func(s host.Sample) int64 { return s.UDPRcvbufErrors }); n > 0 {
			udp.N += n
			udp.Hosts++
		}
	}
	return append(cats, udp), gaps
}

// dropLine is hostSummary's verdict on drops: the root namespace's line for a
// run without the newer counters, otherwise every category and what went
// unmeasured.
func dropLine(cats []dropCount, gaps map[string]int) string {
	if len(cats) == 1 {
		if n := cats[0].N; n > 0 {
			return fmt.Sprintf("UDP receive-buffer drops: %d on %d hosts (host saturation; treat timings with care)", n, cats[0].Hosts)
		}
		return "no UDP receive-buffer drops"
	}
	var total int64
	parts := make([]string, len(cats))
	for i, c := range cats {
		total += c.N
		parts[i] = fmt.Sprintf("%s %d", c.What, c.N)
	}
	line := "no packet drops: "
	if total > 0 {
		line = fmt.Sprintf("%d packet drops (treat timings with care): ", total)
	}
	line += strings.Join(parts, ", ")
	if len(gaps) > 0 {
		srcs := make([]string, 0, len(gaps))
		for src := range gaps {
			srcs = append(srcs, src)
		}
		sort.Strings(srcs)
		for i, src := range srcs {
			srcs[i] = fmt.Sprintf("%s on %d hosts", src, gaps[src])
		}
		line += "; not measured throughout: " + strings.Join(srcs, ", ")
		if total == 0 {
			line = "insufficient coverage, " + line
		}
	}
	return line
}
