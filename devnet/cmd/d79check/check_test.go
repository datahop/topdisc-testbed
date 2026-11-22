package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/p2p/enode"
)

var validStatus = map[string]bool{statusPass: true, statusFail: true, statusNA: true, statusMissing: true, statusInfo: true}

// TestCheckRoundTrip reads live d79-smoke outputs the way M3a's report.go does: a
// top-level "check" with valid statuses throughout.
func TestCheckRoundTrip(t *testing.T) {
	for _, name := range []string{"tables.json", "crawl-reach.json", "topic.json"} {
		data, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		var v struct {
			Check *check `json:"check"`
		}
		if err := json.Unmarshal(data, &v); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if v.Check == nil || len(v.Check.Sub) == 0 {
			t.Fatalf("%s: no top-level check with sub-checks", name)
		}
		var walk func(c check)
		walk = func(c check) {
			if !validStatus[c.Status] || c.ID == "" {
				t.Errorf("%s: bad check %+v", name, c)
			}
			for _, s := range c.Sub {
				walk(s)
			}
		}
		walk(*v.Check)
		want := overall(v.Check.Sub)
		if name == "tables.json" {
			want = v.Check.Sub[0].Status // judged on E1a only
		}
		if v.Check.Status != want {
			t.Errorf("%s: status %s, want %s", name, v.Check.Status, want)
		}
		out, err := json.Marshal(v.Check)
		if err != nil {
			t.Fatal(err)
		}
		var back check
		if err := json.Unmarshal(out, &back); err != nil || back.Status != v.Check.Status || len(back.Sub) != len(v.Check.Sub) {
			t.Errorf("%s: round trip changed the check: %+v", name, back)
		}
	}
}

func TestTablesCheck(t *testing.T) {
	full := &crawlTables{Nodes: map[string]crawlRead{
		"01": {Buckets: map[string][]crawlEntry{"256": {{"02", true}, {"03", true}, {"04", true}}}},
		"02": {Buckets: map[string][]crawlEntry{"256": {{"01", true}, {"03", true}, {"04", true}}}},
		"03": {Buckets: map[string][]crawlEntry{"256": {{"01", true}, {"02", true}, {"04", true}}}},
		"04": {Buckets: map[string][]crawlEntry{"256": {{"01", true}, {"02", true}, {"03", true}}}},
	}}
	res := classifyTables(fourNodes(), full)
	if c := tablesCheck(res, "/runs/x/snapshots/t30m", oldNew); c.Status != statusPass || c.Sub[0].Status != statusPass || c.Sub[1].Status != statusInfo {
		t.Errorf("full tables at t30m: %+v", c)
	}
	if c := tablesCheck(res, "/runs/x/snapshots/t10m", oldNew); c.Sub[0].Status != statusNA || c.Status != statusNA {
		t.Errorf("full tables at t10m: %+v", c)
	}
	// el-4 answered but holds only dead entries: E1(a) fails, E1(b) lacks data.
	full.Nodes["04"] = crawlRead{Buckets: map[string][]crawlEntry{"256": {{"01", false}}}}
	res = classifyTables(fourNodes(), full)
	if c := tablesCheck(res, "/runs/x/snapshots/t30m", oldNew); c.Status != statusFail || c.Sub[1].Status != statusMissing {
		t.Errorf("one empty table: %+v", c)
	}
	delete(full.Nodes, "04")
	res = classifyTables(fourNodes(), full)
	if c := tablesCheck(res, "/runs/x/snapshots/t60m", oldNew); c.Status != statusMissing || c.Sub[0].Status != statusMissing || c.Sub[1].Status != statusMissing {
		t.Errorf("one table unread: %+v", c)
	}
	if c := tablesCheck(classifyTables(fourNodes(), &crawlTables{}), "/runs/x", oldNew); c.Status == statusPass {
		t.Errorf("no tables read: %+v", c)
	}
}

func TestReachCheck(t *testing.T) {
	full := 3.0
	cases := []struct {
		r    *reachResult
		want string
	}{
		{nil, statusNA},
		{&reachResult{Err: "exit status 1"}, statusMissing},
		{&reachResult{Reached: 4, Of: 4, Pass: true, TFullS: &full}, statusPass},
		{&reachResult{Reached: 3, Of: 4, Missing: []string{"el-3"}}, statusFail},
	}
	for _, c := range cases {
		if got := reachCheck("old", c.r); got.Status != c.want {
			t.Errorf("%+v: %s, want %s", c.r, got.Status, c.want)
		}
	}
}

func TestTopicCheck(t *testing.T) {
	inv := fourNodes()
	pass := &topicResult{Pass: true, Expected: 2, FoundNew: 2}
	if c := topicCheck(inv, map[string]*topicResult{"new": pass}); c.Status != statusMissing || c.Sub[1].Status != statusMissing {
		t.Errorf("OLD seed not run: %+v", c)
	}
	if c := topicCheck(inv, map[string]*topicResult{"new": pass, "old": pass}); c.Status != statusPass {
		t.Errorf("both seeds pass: %+v", c)
	}
	if c := topicCheck(inv, map[string]*topicResult{"new": pass, "old": {Expected: 2, FoundNew: 1}}); c.Status != statusFail {
		t.Errorf("OLD seed fails: %+v", c)
	}
	control := shortInventory(node{Name: "el-1", Kind: "old", ID: "01"})
	if c := topicCheck(control, map[string]*topicResult{}); c.Status != statusNA {
		t.Errorf("control run: %+v", c)
	}
}

func TestRecordProbe(t *testing.T) {
	dir := t.TempDir()
	done := make(chan func(), 2)
	for _, role := range []string{"crawl-old", "crawl-new"} {
		go func() {
			stop, err := recordProbe(dir, role, fixedKey(role))
			if err != nil {
				t.Error(err)
			}
			done <- stop
		}()
	}
	for range 2 {
		if stop := <-done; stop != nil {
			stop()
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "probes.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ps []probeRecord
	if err := json.Unmarshal(data, &ps); err != nil {
		t.Fatal(err)
	}
	if len(ps) != 2 || ps[0].ID == ps[1].ID || ps[0].Stopped == nil || ps[1].Stopped == nil {
		t.Fatalf("probes.json: %s", data)
	}
	if want := enode.PubkeyToIDV4(&fixedKey("crawl-old").PublicKey).String(); ps[0].ID != want && ps[1].ID != want {
		t.Errorf("crawl-old id %s not in %s", want, data)
	}
}

// TestRecordProbeMerge keeps one entry per ID with its first start and last stop,
// folds duplicate IDs of an older file, and keeps entries of other probes.
func TestRecordProbeMerge(t *testing.T) {
	dir := t.TempDir()
	t0 := time.Date(2026, 11, 18, 16, 58, 0, 0, time.UTC)
	t1, t2 := t0.Add(time.Minute), t0.Add(2*time.Minute)
	topicID := enode.PubkeyToIDV4(&fixedKey("topic").PublicKey).String()
	old := []probeRecord{
		{Name: "earlier", ID: "aa", Started: t0, Stopped: &t1},
		{Name: "topic-new", ID: topicID, Started: t0, Stopped: &t1},
		{Name: "topic-old", ID: topicID, Started: t1, Stopped: &t2},
	}
	if err := writeJSON(filepath.Join(dir, "probes.json"), old); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		stop, err := recordProbe(dir, "topic", fixedKey("topic"))
		if err != nil {
			t.Fatal(err)
		}
		stop()
	}
	data, err := os.ReadFile(filepath.Join(dir, "probes.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ps []probeRecord
	if err := json.Unmarshal(data, &ps); err != nil {
		t.Fatal(err)
	}
	if len(ps) != 2 || ps[0].ID != "aa" || !ps[0].Stopped.Equal(t1) {
		t.Fatalf("probes.json: %s", data)
	}
	if p := ps[1]; p.ID != topicID || !p.Started.Equal(t0) || p.Stopped == nil || !p.Stopped.After(t2) {
		t.Errorf("topic entry: %+v", p)
	}
}

// TestProbesFileErrors refuses to rewrite an empty or unparsable probes.json.
func TestProbesFileErrors(t *testing.T) {
	for _, content := range []string{"", "  \n", "{", "null", `{"name":"x"}`} {
		dir := t.TempDir()
		path := filepath.Join(dir, "probes.json")
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := recordProbe(dir, "tables", fixedKey("tables")); err == nil {
			t.Errorf("%q: no error", content)
		}
		if data, _ := os.ReadFile(path); string(data) != content {
			t.Errorf("%q: file changed to %q", content, data)
		}
	}
}

// TestProbesWriterReader: what recordProbe writes, next to its lock file, is what the
// logs check reads, and a log-abbreviated probe ID matches.
func TestProbesWriterReader(t *testing.T) {
	dir := t.TempDir()
	stop, err := recordProbe(dir, "topic-new", fixedKey("topic-new"))
	if err != nil {
		t.Fatal(err)
	}
	stop()
	if _, err := os.Stat(filepath.Join(dir, "probes.json.lock")); err != nil {
		t.Fatal(err)
	}
	ids, err := readProbes(filepath.Join(dir, "probes.json"))
	if err != nil {
		t.Fatal(err)
	}
	id := enode.PubkeyToIDV4(&fixedKey("topic-new").PublicKey)
	if len(ids) != 1 || !isProbe(id.TerminalString(), ids) || isProbe("4822759436a4e679", ids) {
		t.Errorf("ids %v, log id %s", ids, id.TerminalString())
	}
	if err := os.WriteFile(filepath.Join(dir, "probes.json"), []byte("null"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readProbes(filepath.Join(dir, "probes.json")); err == nil {
		t.Error("null probes.json accepted")
	}
}
