package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// genInventory makes nOld OLD (participant 0 first) and nNew NEW nodes.
func genInventory(nOld, nNew int) *inventory {
	inv := new(inventory)
	for i := range nOld + nNew {
		n := node{Name: fmt.Sprintf("el-%d-geth-lighthouse", i+1), Kind: kindOld, IP: fmt.Sprintf("10.0.0.%d", i+1),
			ID: fmt.Sprintf("%064x", i+1), ENR: "enr:-x"}
		if i >= nOld {
			n.Kind, n.TD = kindNew, strp("01")
		}
		inv.Nodes = append(inv.Nodes, n)
	}
	if errs := inv.validate(); len(errs) > 0 {
		panic(strings.Join(errs, "; "))
	}
	return inv
}

func runFor(inv *inventory) *runData {
	return &runData{
		inv: &inventoryResult{Nodes: inv.Nodes, Old: inv.count(kindOld), New: inv.count(kindNew),
			Boot: inv.boot().Name, BootKind: inv.boot().Kind},
		subdirs: []string{"."}, peers: map[string]*peersResult{}, metrics: map[string]*metricsResult{}, m3b: map[string][]string{},
	}
}

func okLog(n node) logsNode {
	return logsNode{Name: n.Name, Kind: n.Kind, Lines: 10, Levels: map[string]int{"DEBUG": 5}, Removed: map[string]int{},
		RemovedByPeer: map[string]int{}, Templates: map[string]int{}}
}

func withLogs(rd *runData, edit func(*logsNode)) *runData {
	rd.logs = &logsResult{}
	for _, n := range rd.inv.Nodes {
		l := okLog(n)
		if edit != nil {
			edit(&l)
		}
		rd.logs.Nodes = append(rd.logs.Nodes, l)
	}
	return rd
}

func f64(v float64) *float64 { return &v }

// withMetrics adds a metrics.json at rel with every e2 series at val for each node.
func withMetrics(rd *runData, rel string, elapsed, val float64, edit func(*metricsNode)) *runData {
	m := &metricsResult{Elapsed: f64(elapsed)}
	for _, n := range rd.inv.Nodes {
		mn := metricsNode{Name: n.Name, Kind: n.Kind, RSSBytes: f64(val), Series: map[string]float64{"discover/v5/bad.count": 0}}
		for _, s := range e2Series {
			if s != "rss" {
				mn.Series[s] = val
			}
		}
		if edit != nil {
			edit(&mn)
		}
		m.Nodes = append(m.Nodes, mn)
	}
	_, had := rd.metrics[rel]
	rd.metrics[rel] = m
	if !had && rel != "." {
		rd.subdirs = append(rd.subdirs, rel)
	}
	return rd
}

func TestReport(t *testing.T) {
	inv := testInventory()
	f := newFakeNet(t, inv)
	n := inv.Nodes
	f.setPeers(n[0].Name, peerEntry(n[1], true))
	f.setPeers(n[1].Name, peerEntry(n[0], false))
	f.setPeers(n[2].Name, peerEntry(n[3], true))
	f.setPeers(n[3].Name, peerEntry(n[2], false))
	for _, nd := range inv.Nodes {
		f.metrics[nd.Name] = map[string]any{"system/cpu/procload/total": 10.0, "system/cpu/goroutines": 200.0,
			"p2p/ingress.count": 1.0, "p2p/egress.count": 1.0, "discover/ingress.count": 1.0, "discover/egress.count": 1.0,
			"discover/v5/bad.count": 0.0}
	}
	dir := writeRunDir(t, inv)
	for _, cmd := range [][]string{{"inventory"}, {"peers"}, {"logs"}, {"metrics", "-no-docker"}} {
		if err := commands[cmd[0]].run(append(cmd[1:], "-run", dir)); err != nil {
			t.Fatalf("%s: %v", cmd[0], err)
		}
	}
	snap := filepath.Join(dir, "snapshots", "t30m")
	if err := runPeers([]string{"-run", dir, "-out", snap}); err != nil {
		t.Fatal(err)
	}
	if err := runReport([]string{"-run", dir}); err != nil {
		t.Fatal(err)
	}
	var res reportResult
	if err := readJSON(filepath.Join(dir, "checks.json"), &res); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	flatten(res.Checks, 0, func(c check, _ int) { got[c.ID] = c.Status })
	for id, want := range map[string]string{
		"E1": statusNeedsM3b, "E2": statusFail, "E2a": statusFail, "E2b": statusNeedsControl, "E2c": statusInfo,
		"E2d": statusNeedsControl, "E3": statusFail, "E3a": statusPass, "E3a snapshots/t30m": statusPass,
		"E3b": statusMissing, "E3c": statusMissing, "E3d": statusFail, "E4": statusNeedsM3b, "E4c": statusMissing,
		"E4d": statusFail, "E4e": statusFail,
	} {
		if got[id] != want {
			t.Errorf("%s = %q, want %q", id, got[id], want)
		}
	}
	md, err := os.ReadFile(filepath.Join(dir, "summary.md"))
	if err != nil || !strings.Contains(string(md), "| **E4** | needs M3b | topic search and registration: FAIL so far: E4d, E4e |") {
		t.Errorf("summary.md:\n%s", md)
	}

	// M3b-shaped files: data plus a top-level check. E1(a) is judged from t30m on.
	early := filepath.Join(dir, "snapshots", "t10m")
	os.MkdirAll(early, 0o755)
	tables := func(status string) map[string]any {
		return map[string]any{
			"snapshot": "t30m",
			"nodes": []map[string]any{{"name": "el-1-geth-lighthouse", "kind": "old", "entries": 3,
				"by_kind": map[string]int{"old": 1, "new": 2}, "new_share": 0.67, "band": []float64{0.2, 0.8}}},
			"check": map[string]any{"id": "E1a", "status": status, "detail": "4/4 nodes hold the other kind",
				"sub": []map[string]any{{"id": "E1b", "status": "n/a", "detail": "binomial band, reported"}}},
		}
	}
	writeJSON(filepath.Join(early, "tables.json"), tables(statusFail))
	writeJSON(filepath.Join(dir, "crawl-reach.json"), map[string]any{"runs": []any{map[string]any{"seed": "old", "reached": 4}},
		"check": map[string]any{"id": "E1c", "status": "PASS", "detail": "4/4 from either seed"}})
	rd, _ := loadRun(dir)
	if c := buildChecks(rd, nil)[0]; c.Status != statusMissing {
		t.Errorf("E1 with only a t10m table = %s: %s", c.Status, renderTable([]check{c}))
	}
	writeJSON(filepath.Join(snap, "tables.json"), tables(statusPass))
	rd, _ = loadRun(dir)
	c := buildChecks(rd, nil)[0]
	if c.Status != statusPass || len(c.Sub) != 3 {
		t.Fatalf("E1 = %s: %s", c.Status, renderTable([]check{c}))
	}
	if c.Sub[0].Status != statusNA || c.Sub[1].Sub[0].Status != statusNA {
		t.Errorf("t10m table / nested E1b: %s", renderTable([]check{c}))
	}
	e1 := func(what string) {
		t.Helper()
		rd, _ := loadRun(dir)
		if c := buildChecks(rd, nil)[0]; c.Status != statusMissing {
			t.Errorf("E1 with %s = %s: %s", what, c.Status, renderTable([]check{c}))
		}
	}
	// No verdict at t30m: n/a does not count.
	writeJSON(filepath.Join(snap, "tables.json"), tables(statusNA))
	e1("an n/a t30m table")
	// An unlabelled run-dir table is not judged.
	writeJSON(filepath.Join(dir, "tables.json"), tables(statusPass))
	e1("only a run-dir table")
	// Every snapshot from t30m on needs a verdict.
	writeJSON(filepath.Join(snap, "tables.json"), tables(statusPass))
	os.MkdirAll(filepath.Join(dir, "snapshots", "t60m"), 0o755)
	e1("no table at t60m")
	os.Remove(filepath.Join(dir, "snapshots", "t60m"))
	writeJSON(filepath.Join(dir, "crawl-reach.json"), map[string]any{"pass": true})
	e1("a crawl-reach.json without check")
}

func TestBadPacketReadings(t *testing.T) {
	inv := testInventory()
	rd := withLogs(runFor(inv), nil)
	if c := badPacketCheck(rd, "E2a"); c.Status != statusMissing {
		t.Errorf("no metrics.json: %s %s", c.Status, c.Detail)
	}
	withMetrics(rd, ".", 1800, 1, nil)
	withMetrics(rd, "snapshots/t30m", 1800, 1, func(m *metricsNode) {
		if m.Name == "el-4-geth-lighthouse" {
			delete(m.Series, "discover/v5/bad.count")
		}
	})
	if c := badPacketCheck(rd, "E2a"); c.Status != statusMissing || !strings.Contains(c.Detail, "snapshots/t30m el-4") {
		t.Errorf("el-4 without the meter at t30m: %s %s", c.Status, c.Detail)
	}
	withMetrics(rd, "snapshots/t30m", 1800, 1, nil)
	if c := badPacketCheck(rd, "E2a"); c.Status != statusPass {
		t.Errorf("complete: %s %s", c.Status, c.Detail)
	}
	rd.logs.Nodes[3].Error = "open logs/el-4-geth-lighthouse.log: no such file"
	if c := badPacketCheck(rd, "E2a"); c.Status != statusMissing {
		t.Errorf("el-4 log missing: %s %s", c.Status, c.Detail)
	}
	rd.metrics["."].Nodes[0].Series["discover/v5/bad.count"] = 2
	if c := badPacketCheck(rd, "E2a"); c.Status != statusFail {
		t.Errorf("el-1 meter 2: %s %s", c.Status, c.Detail)
	}
}

func TestE2cPerNode(t *testing.T) {
	inv := genInventory(5, 4)     // el-1 boot, el-2..el-5 OLD, el-6..el-9 NEW
	removed := map[string][2]int{ // old, new
		"el-1-geth-lighthouse": {0, 50}, // participant 0, not judged
		"el-2-geth-lighthouse": {4, 4},
		"el-3-geth-lighthouse": {1, 6},
		"el-4-geth-lighthouse": {0, 2},
		"el-5-geth-lighthouse": {4, 5},
	}
	mk := func(over map[string][2]int) *runData {
		return withLogs(runFor(inv), func(l *logsNode) {
			r := removed[l.Name]
			if o, ok := over[l.Name]; ok {
				r = o
			}
			l.Removed[kindOld], l.Removed[kindNew] = r[0], r[1]
		})
	}
	c := removalCheck(mk(nil))
	if c.Status != statusInfo || !strings.Contains(c.Detail, "above 2×: el-3-geth-lighthouse old 1 new 6") || strings.Contains(c.Detail, "el-1-geth") {
		t.Errorf("el-3 over the rate: %s %s", c.Status, c.Detail)
	}
	c = removalCheck(mk(map[string][2]int{"el-3-geth-lighthouse": {4, 3}}))
	if c.Status != statusInfo || !strings.Contains(c.Detail, "3 nodes compared, none above 2×") || !strings.Contains(c.Detail, "el-4-geth-lighthouse old 0 new 2") {
		t.Errorf("all within: %s %s", c.Status, c.Detail)
	}
	small := map[string][2]int{"el-2-geth-lighthouse": {0, 1}, "el-3-geth-lighthouse": {0, 0}, "el-5-geth-lighthouse": {1, 1}}
	if c = removalCheck(mk(small)); c.Status != statusInfo {
		t.Errorf("all small: %s %s", c.Status, c.Detail)
	}
	rd := mk(map[string][2]int{"el-3-geth-lighthouse": {4, 3}})
	rd.logs.Nodes[4].Levels = map[string]int{"INFO": 10}
	if c = removalCheck(rd); c.Status != statusMissing || !strings.Contains(c.Detail, "el-5-geth-lighthouse: no DEBUG lines") {
		t.Errorf("el-5 without debug: %s %s", c.Status, c.Detail)
	}
	rd = mk(nil)
	rd.logs.Nodes[4].Levels = map[string]int{"INFO": 10}
	if c = removalCheck(rd); c.Status != statusMissing || !strings.Contains(c.Detail, "above 2×: el-3-geth-lighthouse") {
		t.Errorf("el-5 without debug, el-3 above: %s %s", c.Status, c.Detail)
	}
	rd.logs.Nodes = rd.logs.Nodes[:3]
	if c = removalCheck(rd); c.Status != statusMissing || !strings.Contains(c.Detail, "el-4-geth-lighthouse: not in logs.json") {
		t.Errorf("el-4 absent: %s %s", c.Status, c.Detail)
	}
}

func TestE2Control(t *testing.T) {
	mixed, control := genInventory(5, 4), genInventory(9, 0)
	run := func() *runData { return withMetrics(withLogs(runFor(mixed), nil), "snapshots/t30m", 1805, 120, nil) }
	ctl := func() *runData { return withMetrics(withLogs(runFor(control), nil), "snapshots/t30m", 1790, 100, nil) }

	if c := mediansCheck(run(), ctl()); c.Status != statusPass {
		t.Errorf("E2d = %s %s", c.Status, renderTable(c.Sub))
	}
	if c := mediansCheck(withMetrics(withLogs(runFor(mixed), nil), "snapshots/t30m", 1805, 130, nil), ctl()); c.Status != statusFail {
		t.Errorf("30 %% above: %s %s", c.Status, renderTable(c.Sub))
	}
	r := withMetrics(withLogs(runFor(mixed), nil), "snapshots/t30m", 1805, 120, func(m *metricsNode) {
		if m.Name == "el-3-geth-lighthouse" {
			m.Error, m.Series, m.RSSBytes = "connection refused", nil, nil
		}
	})
	if c := mediansCheck(r, ctl()); c.Status != statusMissing || !strings.Contains(c.Sub[0].Detail, "el-3-geth-lighthouse") {
		t.Errorf("el-3 unreachable: %s %s", c.Status, renderTable(c.Sub))
	}
	if c := mediansCheck(withMetrics(run(), "snapshots/t60m", 3600, 120, nil), ctl()); c.Status != statusMissing || c.Sub[1].Detail != "metrics.json only in the run" {
		t.Errorf("unmatched t60m: %s %s", c.Status, renderTable(c.Sub))
	}
	if c := mediansCheck(run(), withMetrics(withLogs(runFor(control), nil), "snapshots/t30m", 2400, 100, nil)); c.Status != statusMissing || !strings.Contains(c.Sub[0].Detail, "not comparable") {
		t.Errorf("control t30m taken at +2400s: %s %s", c.Status, renderTable(c.Sub))
	}
	if c := mediansCheck(withMetrics(run(), ".", 4000, 120, nil), withMetrics(ctl(), ".", 4500, 100, nil)); c.Status != statusMissing {
		t.Errorf("root snapshots 500 s apart: %s %s", c.Status, renderTable(c.Sub))
	}

	set := func(tmpl map[string]int) func(*logsNode) {
		return func(l *logsNode) { l.Templates = tmpl }
	}
	ctlT := withLogs(runFor(control), set(map[string]int{"WARN a": 1, "WARN b": 2}))
	if c := templatesCheck(withLogs(runFor(mixed), set(map[string]int{"WARN a": 3})), ctlT); c.Status != statusPass {
		t.Errorf("E2b = %s %s", c.Status, c.Detail)
	}
	if c := templatesCheck(withLogs(runFor(mixed), set(map[string]int{"ERROR new": 1})), ctlT); c.Status != statusFail || !strings.Contains(c.Detail, "ERROR new") {
		t.Errorf("E2b extra = %s %s", c.Status, c.Detail)
	}
	broken := withLogs(runFor(mixed), func(l *logsNode) {
		if l.Name == "el-2-geth-lighthouse" {
			l.Error = "read error"
		}
	})
	if c := templatesCheck(broken, ctlT); c.Status != statusMissing {
		t.Errorf("E2b with an unreadable log = %s %s", c.Status, c.Detail)
	}
	ctlT.logs.Nodes = ctlT.logs.Nodes[1:]
	if c := templatesCheck(withLogs(runFor(mixed), nil), ctlT); c.Status != statusMissing || !strings.Contains(c.Detail, "control el-1") {
		t.Errorf("E2b with a control node absent = %s %s", c.Status, c.Detail)
	}
}

// TestE2SingleKindControl judges a stock-only run against the control like a mixed run.
func TestE2SingleKindControl(t *testing.T) {
	stock := func() *runData {
		return withMetrics(withLogs(runFor(genInventory(9, 0)), nil), "snapshots/t30m", 1795, 100, nil)
	}
	c := buildChecks(stock(), stock())[1]
	got := map[string]check{}
	flatten([]check{c}, 0, func(c check, _ int) { got[c.ID] = c })
	if c.Status != statusPass {
		t.Errorf("E2 = %s: %s", c.Status, renderTable([]check{c}))
	}
	for id, want := range map[string]string{
		"E2a": "9 old nodes", "E2b": "on 9 old nodes, control 0 on 9", "E2d snapshots/t30m": "8 vs 8 old nodes",
	} {
		if !strings.Contains(got[id].Detail, want) {
			t.Errorf("%s detail %q, want %q", id, got[id].Detail, want)
		}
	}
	if s := got["E2c"].Status; s != statusInfo {
		t.Errorf("E2c = %s %s", s, got["E2c"].Detail)
	}
	above := withLogs(runFor(genInventory(9, 0)), func(l *logsNode) {
		if l.Name == "el-3-geth-lighthouse" {
			l.RemovedByPeer = map[string]int{"el-2-geth-lighthouse": 6, "el-5-geth-lighthouse": 1}
		}
	})
	withMetrics(above, "snapshots/t30m", 1795, 100, nil)
	if c := buildChecks(above, stock())[1]; c.Status != statusPass || c.Sub[2].Status != statusInfo ||
		!strings.Contains(c.Sub[2].Detail, "above 2×: el-3-geth-lighthouse") {
		t.Errorf("E2 with el-3 above 2× = %s: %s", c.Status, renderTable([]check{c}))
	}
	if c := buildChecks(stock(), nil)[1]; c.Status != statusNeedsControl {
		t.Errorf("E2 without control = %s: %s", c.Status, renderTable([]check{c}))
	}
}

// TestE3cFromSamples: report re-judges E3c from heads.jsonl and falls back to the saved
// heads.json check when the samples cannot be read.
func TestE3cFromSamples(t *testing.T) {
	inv := genInventory(1, 1)
	sample := func(e float64, slot, block, fin uint64) headSample {
		s := headSample{Elapsed: e}
		for i, n := range inv.Nodes {
			el := elSample{Name: n.Name, Latest: &blockRef{block, blockHash(block)}, Syncing: boolp(false), NotFinalized: fin == 0}
			if fin > 0 {
				el.Finalized = &blockRef{fin * 32, blockHash(fin * 32)}
			}
			s.ELs = append(s.ELs, el)
			s.CLs = append(s.CLs, clSample{Name: clNodeName(i), HeadSlot: u64(slot), FinalizedEpoch: u64(fin)})
		}
		return s
	}
	dir := t.TempDir()
	rd := runFor(inv)
	if err := writeJSON(filepath.Join(dir, "inventory-check.json"), rd.inv); err != nil {
		t.Fatal(err)
	}
	saved := headsResult{Check: check{ID: "E3c", Desc: "heads", Status: statusFail, Detail: "saved verdict"}}
	if err := writeJSON(filepath.Join(dir, "heads.json"), saved); err != nil {
		t.Fatal(err)
	}
	var lines []byte
	for _, s := range []headSample{sample(1500, 125, 125, 0), sample(1512, 126, 126, 1), sample(2700, 225, 225, 5)} {
		b, _ := json.Marshal(s)
		lines = append(append(lines, b...), '\n')
	}
	jsonl := filepath.Join(dir, "heads.jsonl")
	e3c := func() check {
		t.Helper()
		rd, err := loadRun(dir)
		if err != nil {
			t.Fatal(err)
		}
		return buildChecks(rd, nil)[2].Sub[2]
	}
	os.WriteFile(jsonl, lines, 0o644)
	if c := e3c(); c.Status != statusPass {
		t.Errorf("from heads.jsonl: %s", renderTable([]check{c}))
	}
	os.WriteFile(jsonl, lines[:len(lines)-20], 0o644)
	if c := e3c(); c.Status != statusFail || c.Detail != "saved verdict" {
		t.Errorf("truncated heads.jsonl: %s", renderTable([]check{c}))
	}
	os.Remove(jsonl)
	if c := e3c(); c.Status != statusFail || c.Detail != "saved verdict" {
		t.Errorf("no heads.jsonl: %s", renderTable([]check{c}))
	}
}

func TestE3aSnapshots(t *testing.T) {
	rd := runFor(testInventory())
	rd.subdirs = []string{".", "snapshots/t10m", "snapshots/t30m"}
	rd.peers["snapshots/t10m"] = &peersResult{Check: check{Status: statusPass}}
	if c := e3Check(rd).Sub[0]; c.Status != statusMissing || c.Sub[1].ID != "E3a snapshots/t30m" {
		t.Errorf("t30m without peers.json: %s", renderTable([]check{c}))
	}
	rd.peers["snapshots/t30m"] = &peersResult{Check: check{Status: statusPass}}
	if c := e3Check(rd).Sub[0]; c.Status != statusPass || len(c.Sub) != 2 {
		t.Errorf("both snapshots: %s", renderTable([]check{c}))
	}
}

func TestControlRun(t *testing.T) {
	mixed := genInventory(5, 4)
	notControl := withMetrics(withLogs(runFor(genInventory(8, 1)), nil), "snapshots/t30m", 1790, 100, nil)
	run := withMetrics(withLogs(runFor(mixed), nil), "snapshots/t30m", 1805, 100, nil)
	if c := templatesCheck(run, notControl); c.Status != statusMissing || !strings.Contains(c.Detail, "1 NEW") {
		t.Errorf("E2b with a mixed control = %s %s", c.Status, c.Detail)
	}
	if c := mediansCheck(run, notControl); c.Status != statusMissing {
		t.Errorf("E2d with a mixed control = %s %s", c.Status, c.Detail)
	}

	// A control with another node count: E2 is missing, naming both counts.
	small := withMetrics(withLogs(runFor(genInventory(4, 0)), nil), "snapshots/t30m", 1790, 100, nil)
	same := withMetrics(withLogs(runFor(genInventory(9, 0)), nil), "snapshots/t30m", 1790, 100, nil)
	withMetrics(run, ".", 1800, 100, nil)
	if c := e2Check(run, small); c.Status != statusMissing || !strings.Contains(c.Detail, "control has 4 nodes, run 9") {
		t.Errorf("E2 with a 4-node control = %s %s", c.Status, c.Detail)
	}
	both := withMetrics(withLogs(runFor(genInventory(3, 1)), nil), "snapshots/t30m", 1790, 100, nil)
	if c := e2Check(run, both); c.Status != statusMissing || !strings.HasPrefix(c.Detail, "control has 4 nodes, run 9; control has 1 NEW nodes") {
		t.Errorf("E2 with a 4-node mixed control = %s %s", c.Status, c.Detail)
	}
	withMetrics(same, ".", 1800, 100, nil)
	if c := e2Check(run, same); c.Status != statusPass {
		t.Errorf("E2 with a 9-node control = %s %s", c.Status, renderTable([]check{c}))
	}

	// R2: the run's participant 0 is NEW, so the control's (OLD) participant 0 is no baseline.
	newBoot := genInventory(5, 4)
	newBoot.Nodes[0].Kind, newBoot.Nodes[0].TD = kindNew, strp("01")
	newBoot.Nodes[8].Kind, newBoot.Nodes[8].TD = kindOld, nil
	r2 := withLogs(runFor(newBoot), func(l *logsNode) { l.Templates = map[string]int{"WARN boot only": 1} })
	ctl := withLogs(runFor(genInventory(9, 0)), func(l *logsNode) {
		if l.Name == "el-1-geth-lighthouse" {
			l.Templates = map[string]int{"WARN boot only": 1}
		}
	})
	if c := templatesCheck(r2, ctl); c.Status != statusFail {
		t.Errorf("R2 vs control participant 0 template = %s %s", c.Status, c.Detail)
	}
	if c := templatesCheck(withLogs(runFor(mixed), func(l *logsNode) { l.Templates = map[string]int{"WARN boot only": 1} }), ctl); c.Status != statusPass {
		t.Errorf("R1 (participant 0 OLD) = %s %s", c.Status, c.Detail)
	}
}

func TestE4Missing(t *testing.T) {
	inv := testInventory()
	rd := withLogs(runFor(inv), func(l *logsNode) { l.TopicRegOK, l.TopicQuery, l.TopicQueryNonzero = 1, 3, 1 })
	withMetrics(rd, ".", 1800, 1, func(m *metricsNode) {
		for _, s := range e4Meters {
			m.Series[s] = 4
		}
		m.Series["discover/regtopic/ticket/late"] = 0
	})
	got := map[string]string{}
	flatten([]check{e4Check(rd, false)}, 0, func(c check, _ int) { got[c.ID] = c.Status })
	if got["E4c"] != statusPass || got["E4d"] != statusPass {
		t.Errorf("complete: %v", got)
	}
	delete(rd.metrics["."].Nodes[2].Series, "discover/ingress/TOPICNODES/v5.count")
	rd.logs.Nodes = rd.logs.Nodes[:2]
	flatten([]check{e4Check(rd, false)}, 0, func(c check, _ int) { got[c.ID] = c.Status })
	if got["E4c"] != statusMissing || got["E4d"] != statusMissing {
		t.Errorf("el-3 meter and log missing: %v", got)
	}
}

func TestOverall(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want string
	}{
		{nil, statusNA},
		{[]string{statusPass, statusNA, statusInfo}, statusPass},
		{[]string{statusInfo}, statusNA},
		{[]string{statusPass, statusNeedsControl, statusMissing}, statusNeedsControl},
		{[]string{statusNeedsM3b, statusFail}, statusFail},
	} {
		var cs []check
		for _, s := range tc.in {
			cs = append(cs, check{Status: s})
		}
		if got := overall(cs); got != tc.want {
			t.Errorf("overall(%v) = %s, want %s", tc.in, got, tc.want)
		}
	}
}
