package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func splitRun(nodes int) *runData {
	rd := runFor(genInventory(nodes, 0))
	rd.split = splitInventory(rd.inv.Nodes)
	return rd
}

func TestSplitInventory(t *testing.T) {
	inv := genInventory(9, 0)
	split := splitInventory(inv.Nodes)
	if split.count(kindOld) != 5 || split.count(kindNew) != 4 || split.boot().Kind != kindOld {
		t.Fatalf("A %d, B %d, boot %s", split.count(kindOld), split.count(kindNew), split.boot().Kind)
	}
	for i, n := range split.Nodes {
		want := kindOld
		if i%2 == 1 {
			want = kindNew
		}
		if n.Kind != want || split.kindOf(n.ID) != want || inv.Nodes[i].Kind != kindOld {
			t.Errorf("%s: split %s, kindOf %s, original %s", n.Name, n.Kind, split.kindOf(n.ID), inv.Nodes[i].Kind)
		}
	}
	g := (&runData{split: split}).splitGroups()
	if len(g.A) != 5 || g.A[0] != "el-1-geth-lighthouse" || len(g.B) != 4 || g.B[0] != "el-2-geth-lighthouse" {
		t.Errorf("groups %+v", g)
	}
}

// peersOf makes a saved peers.json node with real-kind (all old) counts.
func peersOf(inv *inventory, i int, peers ...int) peersNode {
	n := inv.Nodes[i]
	pn := peersNode{Name: n.Name, Kind: n.Kind, Total: len(peers), ByKind: map[string]dirCount{kindOld: {Out: len(peers)}}}
	for _, j := range peers {
		pn.Peers = append(pn.Peers, peerInfo{ID: inv.Nodes[j].ID, Kind: kindOld, NameKind: kindOld})
	}
	return pn
}

func TestSplitPeers(t *testing.T) {
	rd := splitRun(9)
	inv := genInventory(9, 0)
	sameParity := &peersResult{}
	for i := range 9 {
		sameParity.Nodes = append(sameParity.Nodes, peersOf(inv, i, (i+2)%9))
	}
	if c := rd.splitPeers(sameParity); c.Status != statusFail || !strings.Contains(c.Detail, "el-1-geth-lighthouse (A) has no B peer") {
		t.Errorf("A-only peering: %s %s", c.Status, c.Detail)
	}
	ring := func() *peersResult {
		p := &peersResult{}
		for i := range 9 {
			p.Nodes = append(p.Nodes, peersOf(inv, i, (i+1)%9, (i+8)%9))
		}
		return p
	}
	if c := rd.splitPeers(ring()); c.Status != statusPass || c.Desc != "every A node has a B peer and vice versa" ||
		!strings.Contains(c.Detail, "A→A 2 A→B 8") {
		t.Errorf("ring: %s %s: %s", c.Status, c.Desc, c.Detail)
	}
	p := ring()
	p.Nodes[1].Peers = []peerInfo{{ID: fmt.Sprintf("%064x", 99), Kind: kindUnknown}}
	if c := rd.splitPeers(p); c.Status != statusFail || !strings.Contains(c.Detail, "el-2-geth-lighthouse (B) has no A peer") {
		t.Errorf("el-2 with only an unknown peer: %s %s", c.Status, c.Detail)
	}
	p = ring()
	p.Nodes[3].NameMismatch = []string{"00000000…: old but name \"Geth/topdisc/x\""}
	if c := rd.splitPeers(p); c.Status != statusFail || !strings.Contains(c.Detail, "el-4-geth-lighthouse: name mismatch") {
		t.Errorf("saved name mismatch: %s %s", c.Status, c.Detail)
	}
	p = ring()
	p.Nodes[4].Error, p.Nodes[4].Peers = "connection refused", nil
	if c := rd.splitPeers(p); c.Status != statusMissing {
		t.Errorf("el-5 unreachable: %s %s", c.Status, c.Detail)
	}
	p = ring()
	p.Nodes = p.Nodes[:8]
	if c := rd.splitPeers(p); c.Status != statusMissing || !strings.Contains(c.Detail, "el-9-geth-lighthouse: not in peers.json") {
		t.Errorf("el-9 absent: %s %s", c.Status, c.Detail)
	}
	p = ring()
	p.Nodes[8] = p.Nodes[7]
	if c := rd.splitPeers(p); c.Status != statusMissing || !strings.Contains(c.Detail, "el-8-geth-lighthouse: listed twice in peers.json") ||
		!strings.Contains(c.Detail, "el-9-geth-lighthouse: not in peers.json") {
		t.Errorf("el-8 twice, el-9 absent: %s %s", c.Status, c.Detail)
	}
	p = ring()
	p.Nodes = append(p.Nodes, peersNode{Name: "el-10-geth-lighthouse"})
	if c := rd.splitPeers(p); c.Status != statusMissing || !strings.Contains(c.Detail, "el-10-geth-lighthouse: not in the inventory") {
		t.Errorf("stranger el-10: %s %s", c.Status, c.Detail)
	}
	if c := rd.splitPeers(&peersResult{Check: check{Status: statusNA}}); c.Status != statusMissing || c.Detail != "no nodes in peers.json" {
		t.Errorf("empty peers.json: %s %s", c.Status, c.Detail)
	}
}

func TestIndexOfAmbiguous(t *testing.T) {
	inv := newInventory([]node{
		{Name: "el-1", Kind: kindOld, ID: "abcdef01" + strings.Repeat("1", 56)},
		{Name: "el-2", Kind: kindNew, ID: "abcdef01" + strings.Repeat("2", 56)},
	})
	for id, want := range map[string]int{
		"abcdef01": -1, "abcdef011": 0, "abcdef012": 1, inv.Nodes[1].ID: 1, "ABCDEF012": 1, "abcdef0": -1, "ffffffff": -1,
	} {
		if got := inv.indexOf(id); got != want {
			t.Errorf("indexOf(%s) = %d, want %d", id, got, want)
		}
	}
	if k := inv.kindOf("abcdef01"); k != kindUnknown {
		t.Errorf("kindOf(ambiguous) = %s", k)
	}
}

func TestSplitTables(t *testing.T) {
	inv := genInventory(4, 0)
	id := func(i int) string { return inv.Nodes[i].ID }
	tables := func(el1 ...int) map[string]crawlRead {
		out := make(map[string]crawlRead)
		for i := range 4 {
			var entries []crawlEntry
			for j := range 4 {
				if j != i {
					entries = append(entries, crawlEntry{id(j), true})
				}
			}
			out[id(i)] = crawlRead{T: 1, Buckets: map[string][]crawlEntry{"256": entries}}
		}
		if el1 != nil {
			var entries []crawlEntry
			for _, j := range el1 {
				entries = append(entries, crawlEntry{id(j), true})
			}
			out[id(0)] = crawlRead{T: 1, Buckets: map[string][]crawlEntry{"255": entries}}
		}
		return out
	}
	dir := t.TempDir()
	snap := filepath.Join(dir, "snapshots", "t30m")
	os.MkdirAll(snap, 0o755)
	path := filepath.Join(snap, "tables.json")
	rd := runFor(inv)
	rd.dir, rd.split = dir, splitInventory(inv.Nodes)
	write := func(nodes map[string]crawlRead) {
		res := classifyTables(inv, &crawlTables{TEnd: 1795175076494, Nodes: nodes})
		res.Check = tablesCheck(res, snap, oldNew)
		if res.Check.Status != statusNA {
			t.Fatalf("all-old tables.json = %s", res.Check.Status)
		}
		if err := writeJSON(path, res); err != nil {
			t.Fatal(err)
		}
	}
	write(tables(2))
	if c := rd.splitTables(path, "snapshots/t30m"); c.Status != statusFail || c.Sub[0].Detail != "el-1-geth-lighthouse" {
		t.Errorf("el-1 without a B entry: %s", renderTable([]check{c}))
	}
	write(tables())
	c := rd.splitTables(path, "snapshots/t30m")
	if c.Status != statusPass || c.ID != "E1 snapshots/t30m/tables.json" || !strings.Contains(c.Sub[0].Desc, "every A table holds a B node") {
		t.Errorf("full tables: %s", renderTable([]check{c}))
	}
	if c := rd.splitTables(filepath.Join(dir, "none.json"), "."); c.Status != statusMissing {
		t.Errorf("no file: %s", renderTable([]check{c}))
	}
}

func TestSplitRemovals(t *testing.T) {
	mk := func(byPeer map[string]int) *runData {
		return withLogs(splitRun(9), func(l *logsNode) {
			if l.Name == "el-3-geth-lighthouse" {
				l.RemovedByPeer = byPeer
			}
		})
	}
	c := removalCheck(mk(map[string]int{"el-2-geth-lighthouse": 6, "el-5-geth-lighthouse": 1}))
	if c.Status != statusInfo || !strings.Contains(c.Detail, "above 2×: el-3-geth-lighthouse A 1 B 6 unknown 0 (1.50 vs 0.25 per node)") ||
		!strings.HasPrefix(c.Desc, "per A node: A↔B removal rate vs 2× A↔A, info only") {
		t.Errorf("el-3 over the rate: %s %s: %s", c.Status, c.Desc, c.Detail)
	}
	if c = removalCheck(mk(map[string]int{"el-2-geth-lighthouse": 3, "el-5-geth-lighthouse": 3})); c.Status != statusInfo || !strings.Contains(c.Detail, "1 nodes compared, none above 2×") {
		t.Errorf("balanced: %s %s", c.Status, c.Detail)
	}
	if c = removalCheck(mk(nil)); c.Status != statusMissing || !strings.Contains(c.Detail, "el-3-geth-lighthouse: no removed_by_peer in logs.json (re-run d79check logs)") {
		t.Errorf("old logs.json: %s %s", c.Status, c.Detail)
	}
}

func TestBuildChecksSingleKind(t *testing.T) {
	inv := genInventory(9, 0)
	rd := withMetrics(withLogs(runFor(inv), func(l *logsNode) {
		if l.Name == "el-3-geth-lighthouse" {
			l.RemovedByPeer = map[string]int{"el-2-geth-lighthouse": 3, "el-5-geth-lighthouse": 3}
		}
	}), "snapshots/t30m", 1805, 100, nil)
	peers := &peersResult{}
	for i := range 9 {
		peers.Nodes = append(peers.Nodes, peersOf(inv, i, (i+1)%9, (i+8)%9))
	}
	peers.Check = check{ID: "E3a", Status: statusNA, Detail: "single-kind run"}
	dir := t.TempDir()
	snap := filepath.Join(dir, "snapshots", "t30m")
	os.MkdirAll(snap, 0o755)
	for path, v := range map[string]any{
		filepath.Join(dir, "inventory-check.json"): rd.inv, filepath.Join(dir, "logs.json"): rd.logs,
		filepath.Join(snap, "metrics.json"): rd.metrics["snapshots/t30m"], filepath.Join(snap, "peers.json"): peers,
	} {
		if err := writeJSON(path, v); err != nil {
			t.Fatal(err)
		}
	}
	out := filepath.Join(t.TempDir(), "rereport")
	if err := runReport([]string{"-run", dir, "-out", out}); err != nil {
		t.Fatal(err)
	}
	var res reportResult
	if err := readJSON(filepath.Join(out, "checks.json"), &res); err != nil {
		t.Fatal(err)
	}
	got := map[string]check{}
	flatten(res.Checks, 0, func(c check, _ int) { got[c.ID] = c })
	for id, want := range map[string]string{
		"E1": statusNeedsM3b, "E2": statusNeedsControl, "E2a": statusPass, "E2c": statusInfo,
		"E3a": statusPass, "E3a snapshots/t30m": statusPass, "E4": statusNA,
	} {
		if got[id].Status != want {
			t.Errorf("%s = %q, want %q: %s", id, got[id].Status, want, got[id].Detail)
		}
	}
	if !strings.Contains(got["E1"].Desc, "mix A and B") || !strings.Contains(got["E3a"].Desc, "every A node has a B peer") {
		t.Errorf("E1 %q, E3a %q", got["E1"].Desc, got["E3a"].Desc)
	}
	if res.Split == nil || len(res.Split.A) != 5 || len(res.Split.B) != 4 || res.Split.A[0] != "el-1-geth-lighthouse" {
		t.Errorf("split = %+v", res.Split)
	}
	md, _ := os.ReadFile(filepath.Join(out, "summary.md"))
	if !strings.Contains(string(md), "Single-kind run: E1, E2c and E3a judge A (el-1-geth-lighthouse, el-3-geth-lighthouse, … 5 nodes, participant 0 included) against B (el-2-geth-lighthouse, el-4-geth-lighthouse, … 4 nodes)") {
		t.Errorf("summary.md:\n%s", md)
	}
}
