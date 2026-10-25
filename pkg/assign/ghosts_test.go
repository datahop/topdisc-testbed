package assign

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/datahop/topdisc-testbed/pkg/scenario"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/p2p/discover"
	"github.com/ethereum/go-ethereum/p2p/discover/topicindex"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

func ghostCfg(nodes int, g scenario.GhostsConfig) scenario.Config {
	cfg := scenario.Default()
	cfg.Scenario.Population.Nodes, cfg.Scenario.Population.Seed = nodes, 3
	cfg.Scenario.Ghosts = g
	return cfg
}

func localHosts(i int) Host { return Host{IP: "127.0.0.1", BasePort: 30300 + i, StatusOff: 10000} }

// checkGhosts parses every ghost of a and returns its count per bucket.
func checkGhosts(t *testing.T, a Assignment) ghostLayout {
	t.Helper()
	key, err := crypto.HexToECDSA(a.Key)
	if err != nil {
		t.Fatal(err)
	}
	self := enode.PubkeyToIDV4(&key.PublicKey)
	var l ghostLayout
	for _, s := range a.Ghosts {
		n, err := enode.Parse(enode.ValidSchemes, s)
		if err != nil {
			t.Fatalf("node %d: bad ghost %q: %v", a.Idx, s, err)
		}
		var ng topicindex.TopicDiscovery
		keys := map[any]bool{}
		for i, e := range n.Record().AppendElements(nil) {
			if i%2 == 1 { // seq, then key, value pairs
				keys[e] = true
			}
		}
		if n.Load(&ng) != nil || !discover.IsTestbedGhost(n) || keys["svc"] || keys["tcp"] || !keys["ghost"] || !keys["udp"] {
			t.Fatalf("node %d: ghost %v: want ng and ghost, no svc and no tcp", a.Idx, n)
		}
		if p := n.UDP(); p < scenario.GhostPortBase || p >= scenario.GhostPortBase+scenario.GhostPortSpan || n.IPAddr().String() != "127.0.0.1" {
			t.Fatalf("node %d: ghost endpoint %v:%d", a.Idx, n.IPAddr(), p)
		}
		l[ghostBucket(self, n.ID())]++
	}
	return l
}

func TestGhostsShare(t *testing.T) {
	cfg := ghostCfg(40, scenario.GhostsConfig{Share: 0.11})
	as, err := Generate(cfg, localHosts, 0, ".")
	if err != nil {
		t.Fatal(err)
	}
	again, err := Generate(cfg, localHosts, 0, ".")
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for i, a := range as {
		if !reflect.DeepEqual(a.Ghosts, again[i].Ghosts) {
			t.Fatalf("node %d: ghosts differ between two Generate calls", i)
		}
		l := checkGhosts(t, a)
		for k := 0; k < ghostSpreadMin-ghostMinDist; k++ {
			if l[k] != 0 {
				t.Fatalf("node %d: %d ghosts at bucket %d, want only 248..256", i, l[k], k+ghostMinDist)
			}
		}
		total += len(a.Ghosts)
	}
	if total == 0 {
		t.Fatal("no ghosts assigned")
	}
	if s := GhostSummary(as); !strings.HasPrefix(s, "ghosts: 40 nodes, ") {
		t.Fatalf("summary %q", s)
	}
}

func TestGhostsOff(t *testing.T) {
	as, err := Generate(ghostCfg(5, scenario.GhostsConfig{}), localHosts, 0, ".")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(as)
	if strings.Contains(string(b), `"ghosts`) || GhostSummary(as) != "" {
		t.Fatalf("ghosts off, assignments carry %s", b)
	}
}

func TestGhostsTables(t *testing.T) {
	// Three crawled tables, out of id order. The first caps bucket 240 at 16
	// and has an alive entry; the second has no bucket 240.
	dead := func(n int, alive int) []map[string]any {
		var out []map[string]any
		for i := 0; i < n; i++ {
			out = append(out, map[string]any{"id": fmt.Sprint(i), "alive": false})
		}
		for i := 0; i < alive; i++ {
			out = append(out, map[string]any{"id": "a", "alive": true})
		}
		return out
	}
	tables := map[string]any{
		"t_end": 1, "peers_read": 3,
		"nodes": map[string]any{
			"cc": map[string]any{"t": 1, "buckets": map[string]any{"256": dead(1, 0), "250": dead(1, 0)}},
			"aa": map[string]any{"t": 1, "entries": 30, "buckets": map[string]any{"240": dead(20, 1), "256": dead(3, 5), "249": dead(1, 2)}},
			"bb": map[string]any{"t": 1, "buckets": map[string]any{"255": dead(2, 0), "252": dead(0, 4)}},
		},
		"summary": map[string]any{"x": 1},
	}
	want := []ghostLayout{{0: 16, 9: 1, 16: 3}, {15: 2}, {10: 1, 16: 1}}
	dir := t.TempDir()
	b, _ := json.Marshal(tables)
	plain := filepath.Join(dir, "tables.json")
	if err := os.WriteFile(plain, b, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(plain + ".gz")
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	gz.Write(b)
	gz.Close()
	f.Close()

	for _, name := range []string{"tables.json", "tables.json.gz"} {
		ls, err := loadGhostLayouts(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(ls, want) {
			t.Fatalf("%s: layouts %v, want %v", name, ls, want)
		}
		as, err := Generate(ghostCfg(6, scenario.GhostsConfig{Tables: name, Pool: 4096}), localHosts, 0, dir)
		if err != nil {
			t.Fatal(err)
		}
		for i, a := range as {
			got, w := checkGhosts(t, a), want[i%3]
			for k := ghostSpreadMin - ghostMinDist; k < len(w); k++ {
				if got[k] != w[k] {
					t.Fatalf("%s node %d: %d ghosts at %d, want %d", name, i, got[k], k+ghostMinDist, w[k])
				}
			}
			if got.total()+a.GhostsUnplaced != w.total() {
				t.Fatalf("%s node %d: %d placed + %d unplaced, want %d", name, i, got.total(), a.GhostsUnplaced, w.total())
			}
		}
	}

	bad := filepath.Join(dir, "bad.json")
	for _, tc := range []struct{ body, want string }{
		{`{"nodes": {"aa": {"buckets": {"300": []}}}}`, "nodes.aa.buckets.300"},
		{`{"nodes": {"aa": {"buckets": {"256": [{"alive": false}]}}}`, "top level"},
		{`{"nodes": {"aa": {"buckets": {"256": [{"alive": false}]}}`, "nodes"},
		{`{"nodes": {"aa": {"buckets": {"256": [{"alive": false}]}}}} xyz`, "after top level"},
		{`{"nodes": {"aa": {"buckets": {"256": [{"alive": false}]}}}} {}`, "after top level"},
	} {
		os.WriteFile(bad, []byte(tc.body), 0o644)
		if _, err := loadGhostLayouts(bad); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: error %v, want %q", tc.body, err, tc.want)
		}
	}
	os.WriteFile(bad, []byte(`{"nodes": {"aa": {"buckets": {"256": [{"alive": false}]}}}}`+"\n"), 0o644)
	if _, err := loadGhostLayouts(bad); err != nil {
		t.Fatalf("complete file: %v", err)
	}
	// a gzip stream cut short, and one whose checksum is wrong
	gzBytes, err := os.ReadFile(plain + ".gz")
	if err != nil {
		t.Fatal(err)
	}
	cut := filepath.Join(dir, "cut.json.gz")
	os.WriteFile(cut, gzBytes[:len(gzBytes)/2], 0o644)
	if _, err := loadGhostLayouts(cut); err == nil {
		t.Fatal("truncated gzip accepted")
	}
	corrupt := append([]byte{}, gzBytes...)
	corrupt[len(corrupt)-8] ^= 0xff // CRC-32 in the trailer
	os.WriteFile(cut, corrupt, 0o644)
	if _, err := loadGhostLayouts(cut); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("bad gzip checksum: error %v", err)
	}
}

// The per-node shares average to the requested share, also where the scaled
// curve is clipped at 1.
func TestGhostShareMean(t *testing.T) {
	for _, share := range []float64{0.11, 0.5} {
		scale := ghostScale(share)
		if m := clippedMean(scale); math.Abs(m-share) > 1e-9 {
			t.Fatalf("share %g: clipped mean %g", share, m)
		}
		rng := rand.New(rand.NewSource(1))
		sum, n := 0.0, 200000
		for i := 0; i < n; i++ {
			sum += ghostShare(rng, scale)
		}
		if m := sum / float64(n); math.Abs(m-share) > 0.02*share {
			t.Fatalf("share %g: sampled mean %g", share, m)
		}
	}
}
