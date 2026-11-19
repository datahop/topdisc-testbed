package main

import (
	"context"
	"flag"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// metricSeries are the DV7 series kept from geth's flat /debug/metrics JSON; prefixes
// keep every key below them (fork counters, absent on stock geth).
var (
	metricSeries = []string{
		"system/cpu/procload",
		"system/cpu/procload/total",
		"system/cpu/goroutines",
		"system/memory/used",
		"p2p/ingress.count",
		"p2p/egress.count",
		"discover/ingress.count",
		"discover/egress.count",
		"discover/v5/bad.count",
		"discover/ingress/UNKNOWN/v5.count",
		"discover/ingress/REGTOPIC/v5.count",
		"discover/ingress/REGCONFIRMATION/v5.count",
		"discover/ingress/TOPICQUERY/v5.count",
		"discover/ingress/TOPICNODES/v5.count",
	}
	metricPrefixes = []string{"discover/regtopic/", "discover/topicreg/"}
)

type metricsNode struct {
	Name     string             `json:"name"`
	Kind     string             `json:"kind"`
	Series   map[string]float64 `json:"series"`
	RSSBytes *float64           `json:"rss_bytes,omitempty"`
	CPUPerc  *float64           `json:"cpu_perc,omitempty"`
	Error    string             `json:"error,omitempty"`
}

type metricsResult struct {
	Time    time.Time     `json:"time"`
	Elapsed *float64      `json:"elapsed_s,omitempty"`
	Nodes   []metricsNode `json:"nodes"`
}

func init() {
	register("metrics", "record DV7 metric series and container RSS per EL", runMetrics)
}

func runMetrics(args []string) error {
	fs := flag.NewFlagSet("metrics", flag.ExitOnError)
	rf := addRunFlags(fs)
	enclave := fs.String("enclave", "", "enclave name for docker stats (default: from -run)")
	noDocker := fs.Bool("no-docker", false, "skip container RSS")
	fs.Parse(args)
	if err := rf.check(); err != nil {
		return err
	}
	inv, err := rf.inventory()
	if err != nil {
		return err
	}
	if *enclave == "" {
		*enclave = rf.enclave()
	}
	res := collectMetrics(context.Background(), inv)
	if g, err := rf.genesis(); err == nil {
		e := res.Time.Sub(g).Seconds()
		res.Elapsed = &e
	}
	if !*noDocker {
		if err := addDockerStats(*enclave, &res); err != nil {
			logf("metrics: no container memory: %v", err)
		}
	}
	for _, n := range res.Nodes {
		fmt.Printf("%-24s %-3s procload=%.0f goroutines=%.0f p2p in/out=%.0f/%.0f disc in/out=%.0f/%.0f rss=%s %s\n", n.Name, n.Kind,
			n.Series["system/cpu/procload"], n.Series["system/cpu/goroutines"], n.Series["p2p/ingress.count"],
			n.Series["p2p/egress.count"], n.Series["discover/ingress.count"], n.Series["discover/egress.count"],
			mib(n.RSSBytes), n.Error)
	}
	return writeJSON(filepath.Join(rf.out, "metrics.json"), res)
}

func mib(b *float64) string {
	if b == nil {
		return "-"
	}
	return fmt.Sprintf("%.1fMiB", *b/(1<<20))
}

func collectMetrics(ctx context.Context, inv *inventory) metricsResult {
	res := metricsResult{Time: time.Now().UTC(), Nodes: make([]metricsNode, len(inv.Nodes))}
	forEach(len(inv.Nodes), func(i int) {
		n := inv.Nodes[i]
		mn := metricsNode{Name: n.Name, Kind: n.Kind}
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		var all map[string]any
		if err := httpGetJSON(ctx, metricsURL(n), &all); err != nil {
			mn.Error = err.Error()
		} else {
			mn.Series = selectSeries(all)
		}
		res.Nodes[i] = mn
	})
	return res
}

func selectSeries(all map[string]any) map[string]float64 {
	out := make(map[string]float64)
	keep := func(k string) {
		if v, ok := all[k].(float64); ok {
			out[k] = v
		}
	}
	for _, k := range metricSeries {
		keep(k)
	}
	for k := range all {
		for _, p := range metricPrefixes {
			if strings.HasPrefix(k, p) {
				keep(k)
			}
		}
	}
	return out
}

func addDockerStats(enclave string, res *metricsResult) error {
	_, ids, err := enclaveServices(enclave)
	if err != nil {
		return err
	}
	out, err := docker(append([]string{"stats", "--no-stream", "--format", "{{.Name}}\t{{.MemUsage}}\t{{.CPUPerc}}"}, ids...)...)
	if err != nil {
		return err
	}
	stats := parseDockerStats(string(out))
	for i := range res.Nodes {
		if s, ok := stats[res.Nodes[i].Name]; ok {
			res.Nodes[i].RSSBytes, res.Nodes[i].CPUPerc = &s[0], &s[1]
		}
	}
	return nil
}

// parseDockerStats maps service name (container name before "--") → {memory bytes, CPU %}.
func parseDockerStats(out string) map[string][2]float64 {
	stats := make(map[string][2]float64)
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(l, "\t")
		if len(f) != 3 {
			continue
		}
		name, _, _ := strings.Cut(f[0], "--")
		used, _, _ := strings.Cut(f[1], "/")
		mem, err1 := parseSize(strings.TrimSpace(used))
		cpu, err2 := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(f[2]), "%"), 64)
		if err1 == nil && err2 == nil {
			stats[name] = [2]float64{mem, cpu}
		}
	}
	return stats
}

func parseSize(s string) (float64, error) {
	units := []struct {
		suffix string
		mult   float64
	}{{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"TiB", 1 << 40}, {"kB", 1e3}, {"MB", 1e6}, {"GB", 1e9}, {"B", 1}}
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			v, err := strconv.ParseFloat(strings.TrimSuffix(s, u.suffix), 64)
			return v * u.mult, err
		}
	}
	return 0, fmt.Errorf("size %q", s)
}
