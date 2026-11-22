package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInventoryValidate(t *testing.T) {
	inv := testInventory()
	if k := inv.kindOf("78de8ec159533c80"); k != kindNew {
		t.Errorf("kindOf(log prefix) = %s, want new", k)
	}
	if k := inv.kindOf(inv.Nodes[0].ID); k != kindOld {
		t.Errorf("kindOf(full id) = %s, want old", k)
	}
	if k := inv.kindOf("d167af3ab76d94a2"); k != kindUnknown {
		t.Errorf("kindOf(outsider) = %s, want unknown", k)
	}

	bad := testInventory()
	bad.Nodes[1].TD = nil
	bad.Nodes[2].ID = bad.Nodes[3].ID
	bad.Nodes[3].IP = bad.Nodes[0].IP
	bad.Nodes[0].TD = strp("01")
	errs := strings.Join(bad.validate(), "\n")
	for _, want := range []string{"new without td", "duplicate id", "duplicate ip", "old with td"} {
		if !strings.Contains(errs, want) {
			t.Errorf("validate: missing %q in\n%s", want, errs)
		}
	}
}

func TestInventoryCommand(t *testing.T) {
	inv := testInventory()
	dir := writeRunDir(t, inv)
	if err := runInventory([]string{"-run", dir}); err != nil {
		t.Fatal(err)
	}
	var res inventoryResult
	if err := readJSON(filepath.Join(dir, "inventory-check.json"), &res); err != nil {
		t.Fatal(err)
	}
	if res.Old != 2 || res.New != 2 || res.Boot != "el-1-geth-lighthouse" {
		t.Errorf("summary = %+v", res)
	}
}

func TestPeers(t *testing.T) {
	inv := testInventory()
	f := newFakeNet(t, inv)
	n := inv.Nodes
	f.setPeers(n[0].Name, peerEntry(n[1], true), peerEntry(n[3], false))
	f.setPeers(n[1].Name, peerEntry(n[0], false), peerEntry(n[2], true))
	f.setPeers(n[2].Name, peerEntry(n[1], false), peerEntry(n[3], true))
	f.setPeers(n[3].Name, peerEntry(n[2], false), peerEntry(n[0], true))

	res := collectPeers(context.Background(), inv)
	if res.Check.Status != statusPass {
		t.Fatalf("E3a = %s %s", res.Check.Status, res.Check.Detail)
	}
	if got := res.Nodes[0].ByKind[kindNew]; got.In != 1 || got.Out != 0 {
		t.Errorf("el-1 new peers = %+v", got)
	}
	if res.Matrix[kindOld][kindNew] != 2 || res.Matrix[kindNew][kindNew] != 2 {
		t.Errorf("matrix = %v", res.Matrix)
	}

	// el-4 loses its NEW peer; el-2 reports el-3 under an OLD name.
	f.setPeers(n[3].Name, peerEntry(n[0], true))
	wrong := peerEntry(n[2], true)
	wrong["name"] = "Geth/v1.17.10-stable-1df30b18/linux-amd64/go1.27.3"
	f.setPeers(n[1].Name, peerEntry(n[0], false), wrong)
	res = collectPeers(context.Background(), inv)
	if res.Check.Status != statusFail {
		t.Fatalf("E3a = %s, want FAIL", res.Check.Status)
	}
	for _, want := range []string{"el-4-geth-lighthouse (old) has no new peer", "name mismatch"} {
		if !strings.Contains(res.Check.Detail, want) {
			t.Errorf("detail %q lacks %q", res.Check.Detail, want)
		}
	}

	// A node that does not answer leaves E3(a) incomplete, not PASS.
	f.setPeers(n[1].Name, peerEntry(n[0], false), peerEntry(n[2], true))
	f.setPeers(n[3].Name, peerEntry(n[2], false))
	old := elURL
	elURL = func(x node) string {
		if x.Name == n[2].Name {
			return "http://127.0.0.1:1"
		}
		return old(x)
	}
	defer func() { elURL = old }()
	if res = collectPeers(context.Background(), inv); res.Check.Status != statusMissing {
		t.Errorf("E3a with el-3 down = %s %s", res.Check.Status, res.Check.Detail)
	}
}

func TestHeadsSample(t *testing.T) {
	inv := testInventory()
	f := newFakeNet(t, inv)
	f.head["el-3-geth-lighthouse"] = 99
	f.finalized["el-4-geth-lighthouse"] = 0 // before the first finalization
	for i := range 4 {
		f.slots[clNodeName(i)] = 300
	}
	cls, err := clNodes("", f.clList())
	if err != nil {
		t.Fatal(err)
	}
	genesis := time.Now().Add(-3600 * time.Second)
	s := sampleHeads(context.Background(), inv, cls, genesis)
	if s.ELs[2].Latest.Number != 99 || s.ELs[0].Finalized.Number != 64 || *s.ELs[0].Syncing {
		t.Errorf("EL sample = %+v", s.ELs)
	}
	if e := s.ELs[3]; !e.NotFinalized || e.Error != "" || e.Finalized != nil {
		t.Errorf("el-4 before finality = %+v", e)
	}
	if *s.CLs[1].HeadSlot != 300 || *s.CLs[1].FinalizedEpoch != 0 {
		t.Errorf("CL sample = %+v", s.CLs[1])
	}
	c, cov := evalHeads([]headSample{s}, 4)
	if st := subStatus(c, "E3c-errors"); st != statusPass {
		t.Errorf("not-yet-finalized counted as an error: %s", renderTable([]check{c}))
	}
	if st := subStatus(c, "E3c-finality"); st != statusFail || cov.FirstFinalizedS != nil {
		t.Errorf("never finalized: E3c-finality = %s", st)
	}

	// After the first finalization, "not found" on an EL that had a finalized block is a loss.
	f.set(func() {
		f.finalized["el-4-geth-lighthouse"] = 32
		for i := range 4 {
			f.finEpoch[clNodeName(i)] = 7
		}
	})
	s2 := sampleHeads(context.Background(), inv, cls, genesis.Add(-12*time.Second))
	f.set(func() { f.finalized["el-4-geth-lighthouse"] = 0 })
	s3 := sampleHeads(context.Background(), inv, cls, genesis.Add(-24*time.Second))
	c, cov = evalHeads([]headSample{s, s2, s3}, 4)
	if subStatus(c, "E3c-finalized") != statusFail || cov.FirstFinalizedS == nil {
		t.Errorf("el-4 lost its finalized block: %s", renderTable([]check{c}))
	}
}

func TestEvalHeadsBudget(t *testing.T) {
	sample := func(errName string) headSample {
		s := headSample{Elapsed: 1200}
		for i, name := range []string{"el-1", "el-2"} {
			e := elSample{Name: name, Latest: &blockRef{100, blockHash(100)}, Finalized: &blockRef{64, blockHash(64)}, Syncing: boolp(false)}
			c := clSample{Name: clNodeName(i), HeadSlot: u64(100), FinalizedEpoch: u64(1)}
			if name == errName {
				e = elSample{Name: name, Error: "context deadline exceeded"}
			}
			s.ELs, s.CLs = append(s.ELs, e), append(s.CLs, c)
		}
		return s
	}
	var samples []headSample
	for i := range 200 {
		name := ""
		if i == 5 || i == 50 {
			name = "el-1"
		}
		samples = append(samples, sample(name))
	}
	c, _ := evalHeads(samples, 2)
	if c.Status != statusPass || subStatus(c, "E3c-errors") != statusInfo {
		t.Errorf("2 errors in 200 samples: %s", renderTable([]check{c}))
	}
	samples[7] = sample("el-1")
	if c, _ = evalHeads(samples, 2); subStatus(c, "E3c-errors") != statusFail {
		t.Errorf("3 errors in 200 samples: %s", renderTable([]check{c}))
	}
	if c, _ = evalHeads(samples[:10], 3); subStatus(c, "E3c-nodes") != statusMissing || c.Status == statusPass {
		t.Errorf("3 ELs expected, 2 sampled: %s", renderTable([]check{c}))
	}
	// One sample with every EL down: within the error budget, but nothing was observed.
	if c, _ = evalHeads([]headSample{sample("el-1")}, 2); c.Status != statusMissing || subStatus(c, "E3c-coverage") != statusMissing {
		t.Errorf("el-1 never answered: %s", renderTable([]check{c}))
	}
}

func clNodeName(i int) string { return "cl-" + string(rune('1'+i)) + "-lighthouse-geth" }

func subStatus(c check, id string) string {
	for _, s := range c.Sub {
		if s.ID == id {
			return s.Status
		}
	}
	return ""
}

func u64(v uint64) *uint64 { return &v }
func boolp(v bool) *bool   { return &v }

func TestEvalHeads(t *testing.T) {
	el := func(name string, n uint64, hash string, fin uint64, syncing bool) elSample {
		e := elSample{Name: name, Latest: &blockRef{n, hash}, Syncing: boolp(syncing)}
		if fin == 0 {
			e.NotFinalized = true
		} else {
			e.Finalized = &blockRef{fin, blockHash(fin)}
		}
		return e
	}
	cl := func(i int, slot, fin uint64) clSample {
		return clSample{Name: clNodeName(i), HeadSlot: u64(slot), FinalizedEpoch: u64(fin)}
	}
	// Epoch 0 until +1500 s, first finalized epoch at +1512 s (epoch 3), then epoch 5 at +2700 s (epoch 7).
	good := []headSample{
		{Elapsed: 1500, ELs: []elSample{el("a", 125, blockHash(125), 0, false), el("b", 124, blockHash(124), 0, false)},
			CLs: []clSample{cl(0, 125, 0), cl(1, 124, 0)}},
		{Elapsed: 1512, ELs: []elSample{el("a", 126, blockHash(126), 32, false), el("b", 126, blockHash(126), 0, false)},
			CLs: []clSample{cl(0, 126, 1), cl(1, 126, 0)}},
		{Elapsed: 2700, ELs: []elSample{el("a", 225, blockHash(225), 160, false), el("b", 225, blockHash(225), 128, false)},
			CLs: []clSample{cl(0, 225, 5), cl(1, 225, 4)}},
	}
	c, cov := evalHeads(good, 2)
	if c.Status != statusPass || *cov.FirstFinalizedS != 1512 || *cov.SyncingFromS != 1500 {
		t.Fatalf("good = %s %+v: %s", c.Status, cov, renderTable([]check{c}))
	}
	if c, _ = evalHeads(good[:1], 2); subStatus(c, "E3c-finality") != statusFail {
		t.Errorf("never finalized: %s", renderTable([]check{c}))
	}
	if c, _ = evalHeads([]headSample{{Elapsed: 300, ELs: good[0].ELs, CLs: good[1].CLs}}, 2); subStatus(c, "E3c-syncing") != statusMissing {
		t.Errorf("no sample from +600 s: %s", renderTable([]check{c}))
	}

	bad := []headSample{
		{Elapsed: 1200, ELs: []elSample{el("a", 100, blockHash(100), 96, false), el("b", 100, "0xfork", 64, false), el("c", 98, blockHash(98), 64, true)},
			CLs: []clSample{cl(0, 100, 2), cl(1, 97, 1)}},
		{Elapsed: 1212, ELs: []elSample{el("a", 101, blockHash(101), 64, false)}, CLs: []clSample{cl(0, 101, 1)}},
		{Elapsed: 3000, ELs: []elSample{el("a", 250, blockHash(250), 64, false)}, CLs: []clSample{cl(0, 250, 2)}},
	}
	c, _ = evalHeads(bad, 3)
	for id, want := range map[string]string{
		"E3c-distinct": statusFail, "E3c-hash": statusFail, "E3c-finalized": statusFail,
		"E3c-syncing": statusFail, "E3c-slots": statusFail, "E3c-finality": statusFail, "E3c-errors": statusPass,
		"E3c-nodes": statusMissing,
	} {
		if got := subStatus(c, id); got != want {
			t.Errorf("%s = %s, want %s", id, got, want)
		}
	}
	if d := c.Sub[5].Detail; !strings.Contains(d, "epoch 2→1") || !strings.Contains(d, "head epoch 7") {
		t.Errorf("finality detail %q", d)
	}

	// el "b" still answers "not found" in the first finality sample: one sample of grace.
	lag := []headSample{good[0], good[1],
		{Elapsed: 1524, ELs: []elSample{el("a", 127, blockHash(127), 32, false), el("b", 127, blockHash(127), 0, false)},
			CLs: []clSample{cl(0, 127, 1), cl(1, 127, 1)}},
		good[2]}
	if c, _ = evalHeads(lag[:2], 2); subStatus(c, "E3c-errors") != statusPass {
		t.Errorf("graced sample counted: %s", renderTable([]check{c}))
	}
	if c, _ = evalHeads(lag, 2); subStatus(c, "E3c-errors") != statusInfo {
		t.Errorf("second not-found sample after finality: %s", renderTable([]check{c}))
	}

	// R0 at first finality: wall-clock epoch 4, but cl-2's head is still in epoch 3.
	r0 := []headSample{
		{Elapsed: 1524, ELs: []elSample{el("a", 127, blockHash(127), 0, false), el("b", 127, blockHash(127), 0, false)},
			CLs: []clSample{cl(0, 127, 0), cl(1, 127, 0)}},
		{Elapsed: 1536, ELs: []elSample{el("a", 128, blockHash(128), 64, false), el("b", 127, blockHash(127), 0, false)},
			CLs: []clSample{cl(0, 128, 2), cl(1, 127, 0)}},
		{Elapsed: 1548, ELs: []elSample{el("a", 129, blockHash(129), 64, false), el("b", 129, blockHash(129), 64, false)},
			CLs: []clSample{cl(0, 129, 2), cl(1, 129, 2)}},
	}
	if c, _ = evalHeads(r0, 2); subStatus(c, "E3c-finality") != statusPass {
		t.Errorf("R0 first finality: %s", renderTable([]check{c}))
	}
	// Frozen after first finality: heads stay at slot 129, finalized epoch 2, until +4500 s.
	stall := append([]headSample{}, r0...)
	for e := 1560.0; e <= 4500; e += 12 {
		s := r0[2]
		s.Elapsed = e
		stall = append(stall, s)
	}
	if c, _ = evalHeads(stall, 2); subStatus(c, "E3c-finality") != statusFail ||
		!strings.Contains(c.Sub[5].Detail, "head slot 129, wall-clock slot 162") {
		t.Errorf("chain frozen at slot 129: %s", renderTable([]check{c}))
	}
	if c, _ = evalHeads(stall[:len(r0)+32], 2); subStatus(c, "E3c-finality") != statusPass {
		t.Errorf("32 slots behind is within the bound: %s", renderTable([]check{c}))
	}
	r0[2].CLs[1] = cl(1, 160, 1)
	if c, _ = evalHeads(r0, 2); subStatus(c, "E3c-finality") != statusFail || !strings.Contains(c.Sub[5].Detail, "head epoch 5") {
		t.Errorf("cl-2 four epochs behind its head: %s", renderTable([]check{c}))
	}
}

func TestTxs(t *testing.T) {
	inv := testInventory()
	f := newFakeNet(t, inv)
	dir := writeRunDir(t, inv)
	if err := runTxs([]string{"-run", dir, "-n", "3", "-seen", "300ms"}); err != nil {
		t.Fatal(err)
	}
	var res txsResult
	if err := readJSON(filepath.Join(dir, "txs.json"), &res); err != nil {
		t.Fatal(err)
	}
	if res.Check.Status != statusPass || len(res.Txs) != 6 {
		t.Fatalf("E3b = %s, %d txs: %s", res.Check.Status, len(res.Txs), renderTable([]check{res.Check}))
	}
	if res.Txs[0].Via != "el-4-geth-lighthouse" || res.Txs[3].Via != "el-2-geth-lighthouse" {
		t.Errorf("vias %s, %s; want el-4 (old, not participant 0) then el-2", res.Txs[0].Via, res.Txs[3].Via)
	}
	for i, s := range f.senders {
		if s != "0xafF0CA253b97e54440965855cec0A8a2E2399896" || f.nonces[i] != uint64(5+i) {
			t.Errorf("tx %d: sender %s nonce %d", i, s, f.nonces[i])
		}
	}
	if res.ChainID != testChainID || len(res.Txs[0].SeenMs) != 3 {
		t.Errorf("chain %d, seen on %d nodes", res.ChainID, len(res.Txs[0].SeenMs))
	}

	for i, r := range res.Txs {
		for name, ms := range r.SeenMs {
			if ms > 300 {
				t.Errorf("tx %d on %s: %.0f ms above the limit", i, name, ms)
			}
		}
	}

	f.set(func() { f.deaf["el-3-geth-lighthouse"] = true })
	if err := runTxs([]string{"-run", dir, "-n", "2", "-seen", "200ms", "-old", "el-1-geth-lighthouse"}); err != nil {
		t.Fatal(err)
	}
	readJSON(filepath.Join(dir, "txs.json"), &res)
	if res.Check.Status != statusFail || res.Txs[0].Unseen[0] != "el-3-geth-lighthouse" {
		t.Errorf("deaf el-3: %s, unseen %v", res.Check.Status, res.Txs[0].Unseen)
	}

	// Seen only as mined, or answering after the limit, does not count as pending in time.
	f.set(func() {
		f.deaf["el-3-geth-lighthouse"] = false
		f.minedOnly["el-1-geth-lighthouse"] = true
		f.delay["el-3-geth-lighthouse"] = 400 * time.Millisecond
	})
	if err := runTxs([]string{"-run", dir, "-n", "2", "-seen", "300ms"}); err != nil {
		t.Fatal(err)
	}
	res = txsResult{}
	readJSON(filepath.Join(dir, "txs.json"), &res)
	r := res.Txs[0]
	if res.Check.Status != statusFail || r.MinedFirst[0] != "el-1-geth-lighthouse" || r.Unseen[0] != "el-3-geth-lighthouse" {
		t.Errorf("mined-only el-1, slow el-3: %s, mined first %v, unseen %v", res.Check.Status, r.MinedFirst, r.Unseen)
	}
	if _, ok := r.SeenMs["el-2-geth-lighthouse"]; !ok {
		t.Errorf("el-2 not seen: %v", r.SeenMs)
	}
}

func TestMetrics(t *testing.T) {
	inv := testInventory()
	f := newFakeNet(t, inv)
	for _, n := range inv.Nodes {
		m := map[string]any{"system/cpu/procload": 3.0, "system/cpu/goroutines": 210.0, "p2p/ingress.count": 1000.0,
			"discover/ingress/FINDNODE/v5.count": 5.0, "discover/v5/bad.count": 0.0, "p2p/ingress.one-minute": 1.5}
		if n.Kind == kindNew {
			m["discover/ingress/REGTOPIC/v5.count"] = 6.0
			m["discover/regtopic/ticket/late"] = 0.0
		}
		f.metrics[n.Name] = m
	}
	res := collectMetrics(context.Background(), inv)
	s := res.Nodes[1].Series
	if s["discover/ingress/REGTOPIC/v5.count"] != 6 || s["system/cpu/goroutines"] != 210 {
		t.Errorf("series = %v", s)
	}
	if _, ok := s["discover/regtopic/ticket/late"]; !ok {
		t.Error("regtopic prefix not kept")
	}
	for _, k := range []string{"discover/ingress/FINDNODE/v5.count", "p2p/ingress.one-minute"} {
		if _, ok := s[k]; ok {
			t.Errorf("%s kept", k)
		}
	}
	if _, ok := res.Nodes[0].Series["discover/ingress/REGTOPIC/v5.count"]; ok {
		t.Error("old node has fork meter")
	}

	stats := parseDockerStats("el-1-geth-lighthouse--076c4f59dff44d4191deb88e29569bff\t87.02MiB / 251.6GiB\t0.52%\n" +
		"vc-1-geth-lighthouse--1f11e1db6d1b47a18fe8f846a15dfa8f\t1.5GiB / 251.6GiB\t1.40%\n")
	if got := stats["el-1-geth-lighthouse"]; got[0] != 87.02*(1<<20) || got[1] != 0.52 {
		t.Errorf("el-1 stats = %v", got)
	}
	if got := stats["vc-1-geth-lighthouse"]; got[0] != 1.5*(1<<30) {
		t.Errorf("vc-1 stats = %v", got)
	}
}

func TestLogTemplate(t *testing.T) {
	for in, want := range map[string]string{
		"Sanitizing invalid node buffer size      provided=1.00GiB updated=256.00MiB": "Sanitizing invalid node buffer size",
		"Head block is not reachable":                                                               "Head block is not reachable",
		"The flag '--vmodule' is deprecated, please use '--log.vmodule' instead":                    "The flag '--vmodule' is deprecated, please use '--log.vmodule' instead",
		`Bad discv5 packet                        id=48 addr=1.2.3.4:5 err="unknown packet type 7"`: "Bad discv5 packet",
	} {
		if got := logTemplate(in); got != want {
			t.Errorf("logTemplate(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLogs(t *testing.T) {
	inv := testInventory()
	dir := writeRunDir(t, inv)
	// Without probes.json the probe's two registration lines on el-2 count.
	if err := runLogs([]string{"-run", dir}); err != nil {
		t.Fatal(err)
	}
	var res logsResult
	readJSON(filepath.Join(dir, "logs.json"), &res)
	if el2 := res.Nodes[1]; el2.TopicRegOK != 2 || el2.TopicRegFailed != 2 || el2.TopicRegProbe != 0 {
		t.Errorf("el-2 without probes.json = %+v", el2)
	}
	probes, err := os.ReadFile("testdata/probes.json")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "probes.json"), probes, 0o644)
	if err := runLogs([]string{"-run", dir}); err != nil {
		t.Fatal(err)
	}
	res = logsResult{}
	if err := readJSON(filepath.Join(dir, "logs.json"), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Probes) != 2 || res.Nodes[1].TopicRegProbe != 2 {
		t.Errorf("probes %v, el-2 ignored %d", res.Probes, res.Nodes[1].TopicRegProbe)
	}
	if len(res.Missing) != 1 || res.Missing[0] != "el-3-geth-lighthouse" {
		t.Errorf("missing = %v", res.Missing)
	}
	byName := map[string]logsNode{}
	for _, n := range res.Nodes {
		byName[n.Name] = n
	}
	el1, el2, el4 := byName["el-1-geth-lighthouse"], byName["el-2-geth-lighthouse"], byName["el-4-geth-lighthouse"]
	if el1.Lines != 12 || el1.Levels["WARN"] != 4 || el1.Levels["DEBUG"] != 4 {
		t.Errorf("el-1 lines %d levels %v", el1.Lines, el1.Levels)
	}
	if el1.Removed[kindNew] != 1 || el1.Removed[kindOld] != 1 || el1.Removed[kindUnknown] != 1 {
		t.Errorf("el-1 removed = %v", el1.Removed)
	}
	if bp := el1.RemovedByPeer; len(bp) != 2 || bp["el-3-geth-lighthouse"] != 1 || bp["el-4-geth-lighthouse"] != 1 {
		t.Errorf("el-1 removed by peer = %v", bp)
	}
	if el1.Reorgs != 2 || el1.ReorgsAfterWarmup != 1 {
		t.Errorf("el-1 reorgs %d, after warm-up %d", el1.Reorgs, el1.ReorgsAfterWarmup)
	}
	for _, tmpl := range []string{"WARN Sanitizing invalid node buffer size", "WARN Nil finalized block cannot evict old blobs", "WARN Large chain reorg detected"} {
		if el1.Templates[tmpl] != 1 {
			t.Errorf("el-1 template %q = %d", tmpl, el1.Templates[tmpl])
		}
	}
	if el2.TopicRegOK != 1 || el2.TopicRegFailed != 1 || el2.TopicQuery != 2 || el2.TopicQueryNonzero != 1 {
		t.Errorf("el-2 topic = %+v", el2)
	}
	if el4.BadPacket != 1 || el4.BadPacketErrs["unknown packet type 7"] != 1 || el4.Templates["ERROR Something broke"] != 1 {
		t.Errorf("el-4 = %+v", el4)
	}
}
