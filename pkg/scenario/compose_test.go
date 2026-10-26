package scenario

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	legacySnapshot = "testdata/legacy.sha256"
	// gitignored and only stat'ed by Load: the *-ghosts scenarios need it to exist
	ghostTables = "models/crawl-2026-10-23-tables.json.gz"
)

// copyScenarios copies the stubs, workloads, profiles and models into a temp
// dir and returns it with the stub names.
func copyScenarios(t *testing.T) (string, []string) {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "scenarios")
	var stubs []string
	err := filepath.WalkDir("../../scenarios", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel("../../scenarios", path)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		if strings.HasSuffix(rel, ".gz") || strings.HasPrefix(d.Name(), ".") || rel == "reference.yaml" {
			return nil
		}
		if filepath.Dir(rel) == "." && strings.HasSuffix(rel, ".yaml") {
			stubs = append(stubs, strings.TrimSuffix(rel, ".yaml"))
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	touch(t, filepath.Join(dst, ghostTables))
	sort.Strings(stubs)
	return dst, stubs
}

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func marshalLoad(t *testing.T, path string) []byte {
	t.Helper()
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := yaml.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// extractLegacy writes the scenarios/ tree of ref into dir.
func extractLegacy(t *testing.T, ref, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", "../..", "archive", ref, "scenarios").Output()
	if err != nil {
		t.Fatalf("git archive %s: %v", ref, err)
	}
	tr := tar.NewReader(bytes.NewReader(out))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, h.Name)
		if h.Typeflag == tar.TypeDir {
			os.MkdirAll(p, 0o755)
			continue
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		os.MkdirAll(filepath.Dir(p), 0o755)
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sc := filepath.Join(dir, "scenarios")
	touch(t, filepath.Join(sc, ghostTables))
	return sc
}

// TestStubsMatchLegacyScenarios checks that every stub resolves to the run.yaml
// its full file did before the split. SCENARIO_LEGACY_REF=<commit> compares
// against that commit's files and rewrites the snapshot.
func TestStubsMatchLegacyScenarios(t *testing.T) {
	dir, stubs := copyScenarios(t)
	got := map[string][]byte{}
	for _, s := range stubs {
		got[s] = marshalLoad(t, filepath.Join(dir, s+".yaml"))
	}
	if ref := os.Getenv("SCENARIO_LEGACY_REF"); ref != "" {
		legacy := extractLegacy(t, ref, t.TempDir())
		var lines []string
		n := 0
		for _, s := range stubs {
			want := marshalLoad(t, filepath.Join(legacy, s+".yaml"))
			if !bytes.Equal(got[s], want) {
				t.Errorf("%s: resolves differently from %s", s, ref)
				continue
			}
			n++
			sum := sha256.Sum256(want)
			lines = append(lines, s+" "+hex.EncodeToString(sum[:]))
		}
		files, _ := filepath.Glob(filepath.Join(legacy, "*.yaml"))
		if len(files)-1 != len(stubs) { // minus reference.yaml
			t.Fatalf("%d legacy scenarios, %d stubs", len(files)-1, len(stubs))
		}
		if t.Failed() {
			return
		}
		if err := os.WriteFile(legacySnapshot, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("%d byte-identical against %s, wrote %s", n, ref, legacySnapshot)
		return
	}
	b, err := os.ReadFile(legacySnapshot)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		name, sum, _ := strings.Cut(l, " ")
		want[name] = sum
	}
	if len(want) != len(stubs) {
		t.Errorf("%d stubs, %d in %s", len(stubs), len(want), legacySnapshot)
	}
	n := 0
	for _, s := range stubs {
		sum := sha256.Sum256(got[s])
		switch w, ok := want[s]; {
		case !ok:
			t.Errorf("%s: not in %s", s, legacySnapshot)
		case w != hex.EncodeToString(sum[:]):
			t.Errorf("%s: resolves differently from the snapshot", s)
		default:
			n++
		}
	}
	t.Logf("%d identical to %s", n, legacySnapshot)
}

// TestComposeRoundTrip checks that the composed file the drivers ship loads
// back to the same run.
func TestComposeRoundTrip(t *testing.T) {
	dir, stubs := copyScenarios(t)
	for _, s := range stubs {
		c, err := Load(filepath.Join(dir, s+".yaml"))
		if err != nil {
			t.Fatal(err)
		}
		want, _ := yaml.Marshal(c)
		b, err := c.ComposedYAML()
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, ".composed-"+s+".yaml")
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
		if got := marshalLoad(t, path); !bytes.Equal(got, want) {
			t.Errorf("%s: composed file loads differently:\n%s", s, got)
		}
	}
	t.Logf("%d round-trip", len(stubs))
}

func TestScanSpec(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		want    Spec
		n       int
		wantErr string
	}{
		{args: []string{"scenarios/x.yaml", "-var", "a=b"}, want: Spec{File: "scenarios/x.yaml"}, n: 1},
		{args: []string{"--workload", "w", "--profile", "p"}, want: Spec{Workload: "w", Profile: "p"}, n: 4},
		{args: []string{"--workload=w", "--profile=p", "--name=n"}, want: Spec{Workload: "w", Profile: "p", Name: "n"}, n: 3},
		{args: []string{"--workload", "w", "--set", "a=1", "--profile", "p", "--set=b.c={x: 1}", "--set", "a=2", "-var", "host_disk_gb=20"},
			want: Spec{Workload: "w", Profile: "p", Set: []string{"a=1", "b.c={x: 1}", "a=2"}}, n: 9},
		{args: []string{"--workload", "w", "--profile", "p", "--other", "x"}, want: Spec{Workload: "w", Profile: "p"}, n: 4},
		{args: []string{"--workload", "w"}, wantErr: "both required"},
		{args: []string{"--workload", "w", "--profile"}, wantErr: "needs a value"},
		{args: nil, wantErr: "missing scenario"},
	} {
		s, n, err := ScanSpec(tc.args)
		switch {
		case tc.wantErr != "":
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%q: error %v, want %q", tc.args, err, tc.wantErr)
			}
		case err != nil:
			t.Errorf("%q: %v", tc.args, err)
		case n != tc.n || !reflect.DeepEqual(s, tc.want):
			t.Errorf("%q: got %+v, %d; want %+v, %d", tc.args, s, n, tc.want, tc.n)
		}
	}
	if _, err := ParseArgs([]string{"--workload", "w", "--profile", "p", "-var", "x"}); err == nil || !strings.Contains(err.Error(), "unexpected argument") {
		t.Errorf("ParseArgs with a trailing argument: %v", err)
	}
}

func TestSet(t *testing.T) {
	dir, _ := copyScenarios(t)
	set := []string{"topic.search_yield_floor=4", "phases.search_timeout=30s", "network.regions={us-east-1: 1}"}
	c, err := Compose(dir, "25k-topdisc-cont", "aws-packed", set, "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "aws-packed-25k-topdisc-cont" {
		t.Errorf("name %q", c.Name)
	}
	if c.Scenario.Topic.SearchYieldFloor != 4 || c.Scenario.Phases.SearchTimeout.String() != "30s" || len(c.Scenario.Network.Regions) != 1 {
		t.Errorf("set not applied: %+v %+v %v", c.Scenario.Topic, c.Scenario.Phases, c.Scenario.Network.Regions)
	}
	if !reflect.DeepEqual(c.Overrides, set) {
		t.Errorf("overrides %v", c.Overrides)
	}
	b, _ := yaml.Marshal(c)
	if !strings.Contains(string(b), "overrides:\n    - topic.search_yield_floor=4\n") {
		t.Errorf("overrides not recorded:\n%s", b)
	}
	if strings.Contains(c.ParamsLine(), "overrides") {
		t.Errorf("overrides in PARAMS")
	}
	for kv, want := range map[string]string{
		"topic.nope=1":                     "unknown key",
		"topic=1":                          "a section",
		"topic.search_yield_floor.x=1":     "unknown key",
		"testbed.g5k.walltime=01:00:00":    "the profile owns testbed",
		"name=x":                           "the profile owns testbed",
		"topic.search_yield_floor=abc":     "cannot unmarshal",
		"phases.search_timeout=soon":       "cannot unmarshal",
		"topic.search_yield_floor=":        "want key=value",
		"search.model=sometimes":           "want conn, scheduled or continuous",
		"population.nodes":                 "want key=value",
		"population.topic_model=x/y/z.txt": "",
	} {
		_, err := Compose(dir, "25k-topdisc-cont", "aws-packed", []string{kv}, "")
		switch {
		case want == "" && err != nil:
			t.Errorf("%s: %v", kv, err)
		case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
			t.Errorf("%s: error %v, want %q", kv, err, want)
		}
	}
	if _, err := Compose(dir, "25k-topdisc-cont", "aws-packed", nil, "a/b"); err == nil {
		t.Errorf("name with / accepted")
	}
}

func TestComposeDisjoint(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("workloads/w.yaml", "scenario:\n  population:\n    nodes: 7\n")
	write("profiles/p.yaml", "testbed:\n  backend: local\n")
	write("workloads/with-testbed.yaml", "scenario:\n  population:\n    nodes: 7\ntestbed:\n  backend: local\n")
	write("workloads/with-name.yaml", "name: x\nscenario:\n  population:\n    nodes: 7\n")
	write("workloads/empty.yaml", "# nothing\n")
	write("profiles/with-scenario.yaml", "testbed:\n  backend: local\nscenario:\n  population:\n    nodes: 7\n")
	write("ok.yaml", "# header\nname: run\nworkload: w\nprofile: p\n")
	write("noname.yaml", "workload: w\nprofile: p\n")
	write("with-set.yaml", "workload: w\nprofile: p\nset: [population.nodes=9]\n")
	write("with-scenario.yaml", "workload: w\nprofile: p\nscenario:\n  population:\n    nodes: 9\n")
	write("no-profile.yaml", "workload: w\n")

	c, err := Load(filepath.Join(dir, "ok.yaml"))
	if err != nil || c.Name != "run" || c.Scenario.Population.Nodes != 7 || c.Testbed.Backend != "local" {
		t.Errorf("stub: %+v, %v", c.Name, err)
	}
	if c.SourcePath != filepath.Join(dir, "run.yaml") {
		t.Errorf("source path %s", c.SourcePath)
	}
	if c, err := Load(filepath.Join(dir, "noname.yaml")); err != nil || c.Name != "noname" {
		t.Errorf("stub without name: %q, %v", c.Name, err)
	}
	for _, tc := range []struct{ w, p, want string }{
		{"with-testbed", "p", "want exactly one top-level key scenario:"},
		{"with-name", "p", "want exactly one top-level key scenario:"},
		{"empty", "p", "want exactly one top-level key scenario:"},
		{"w", "with-scenario", "want exactly one top-level key testbed:"},
		{"w", "missing", "no such file"},
	} {
		if _, err := Compose(dir, tc.w, tc.p, nil, ""); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s + %s: error %v, want %q", tc.w, tc.p, err, tc.want)
		}
	}
	for f, want := range map[string]string{
		"with-set.yaml":      "not set:",
		"with-scenario.yaml": "not scenario:",
		"no-profile.yaml":    "needs both workload and profile",
	} {
		if _, err := Load(filepath.Join(dir, f)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error %v, want %q", f, err, want)
		}
	}
	if _, err := ParseArgs([]string{"--workload", "w"}); err == nil {
		t.Errorf("missing --profile accepted")
	}
}
