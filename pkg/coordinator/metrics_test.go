package coordinator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/datahop/topdisc-testbed/pkg/assign"
)

// An empty sessions[] is a node that never came up: offline at every moment.
// A trace with no session data at all (older runs) stays always online.
func TestOnlineSessions(t *testing.T) {
	sessions := map[string][][2]int64{}
	for id, js := range map[string]string{
		"never":   `{"id":"never","sessions":[]}`,
		"old":     `{"id":"old"}`,
		"one":     `{"id":"one","session":{"start_ms":100,"end_ms":200}}`,
		"churned": `{"id":"churned","sessions":[{"start_ms":100,"end_ms":200},{"start_ms":300,"end_ms":400}]}`,
	} {
		var lt leanTrace
		if err := json.Unmarshal([]byte(js), &lt); err != nil {
			t.Fatal(err)
		}
		if ss, ok := lt.spans(); ok {
			sessions[id] = ss
		}
	}
	if _, ok := sessions["old"]; ok {
		t.Error("a trace without session data has spans")
	}
	online := onlineAt(sessions, 1000)
	for _, tc := range []struct {
		id   string
		at   int64
		want bool
	}{
		{"never", 0, false}, {"never", 150, false}, {"never", 1000, false},
		{"old", 0, true}, {"old", 1000, true},
		{"one", 150, true}, {"one", 250, false},
		{"churned", 150, true}, {"churned", 250, false}, {"churned", 350, true}, {"churned", 1000, false},
	} {
		if got := online(tc.id, tc.at); got != tc.want {
			t.Errorf("online(%s, %d) = %v, want %v", tc.id, tc.at, got, tc.want)
		}
	}
}

// A mixed topic: a TopDisc searcher is scored against the TopDisc members, a
// plain one against every member, and perTopic.target is members - 1
// whichever searcher comes first.
func TestMetricsMixedTopic(t *testing.T) {
	trDir, runDir := t.TempDir(), t.TempDir()
	var as []assign.Assignment
	for i, legacy := range []bool{false, true, false} {
		as = append(as, assign.Assignment{Idx: i, Topics: []string{"topic-1"}, Legacy: legacy})
		id := strings.Repeat(fmt.Sprint(i+1), 64)
		tr := fmt.Sprintf(`{"idx":%d,"id":%q,"legacy":%v,"lookups":[{"start_ms":1,"results":0}]}`, i, id, legacy)
		if err := os.WriteFile(filepath.Join(trDir, fmt.Sprintf("node%d.json", i)), []byte(tr), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeMetrics(trDir, runDir, as); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(runDir, "metrics.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Results  []searchResult `json:"results"`
		PerTopic []topicReport  `json:"perTopic"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Results) != 3 {
		t.Fatalf("%d results, want 3", len(m.Results))
	}
	for _, r := range m.Results {
		want := map[bool]int{true: 2, false: 1}[r.Legacy]
		if r.Legacy != (r.NodeIdx == 1) || r.Target != want {
			t.Errorf("node %d: legacy %v target %d, want legacy %v target %d", r.NodeIdx, r.Legacy, r.Target, r.NodeIdx == 1, want)
		}
	}
	if len(m.PerTopic) != 1 || m.PerTopic[0].Target != 2 || m.PerTopic[0].Registrants != 2 {
		t.Errorf("perTopic %+v, want target 2 registrants 2", m.PerTopic)
	}
}
