package coordinator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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

// Under score_all_providers a TopDisc searcher is scored against every
// provider of its topic, finds of other nodes dropped, each provider tagged
// with the sources that returned it across lookups and the dialer; the
// providers up at the end it never found are listed.
func TestMetricsScoreAll(t *testing.T) {
	var ids []string
	for i := 1; i <= 5; i++ {
		ids = append(ids, strings.Repeat(fmt.Sprint(i), 64))
	}
	other := strings.Repeat("9", 64) // in no trace: not a provider
	f := func(id, src string, both bool) string {
		return fmt.Sprintf(`{"id":%q,"at_ms":5,"src":%q,"both":%v}`, id, src, both)
	}
	searcher := fmt.Sprintf(`"lookups":[{"start_ms":1,"results":4,"walk_queries":4,"found":[%s,%s,%s,%s]},{"start_ms":2,"results":1,"found":[%s]}],"conn":{"start_ms":1,"found":[%s]}`,
		f(ids[1], "walk", false), f(other, "walk", false), f(ids[2], "topic", false), f(ids[4], "walk", true), f(ids[1], "topic", false), f(ids[2], "walk", false))
	for _, scoreAll := range []bool{false, true} {
		trDir, runDir := t.TempDir(), t.TempDir()
		var as []assign.Assignment
		for i, legacy := range []bool{false, true, false, true, false} {
			a := assign.Assignment{Idx: i, Topics: []string{"topic-1"}, Legacy: legacy}
			a.Search.ScoreAllProviders = scoreAll
			as = append(as, a)
			body := `"lookups":[{"start_ms":1,"results":0}]`
			if i == 0 {
				body = searcher
			}
			tr := fmt.Sprintf(`{"idx":%d,"id":%q,"legacy":%v,%s}`, i, ids[i], legacy, body)
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
			Results []searchResult `json:"results"`
		}
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		r := m.Results[0]
		// Without score_all_providers lookup finds stay unfiltered (the old
		// behaviour), dialer finds count registrants only.
		wantIDs := []string{short(ids[1]), short(other), short(ids[2]), short(ids[4])}
		wantSrc := []string{"walk+topic", "walk", "topic+walk", "walk+topic"}
		wantTarget, wantMissed := 2, []string(nil)
		if scoreAll {
			wantIDs = []string{short(ids[1]), short(ids[2]), short(ids[4])}
			wantSrc = []string{"walk+topic", "topic+walk", "walk+topic"}
			wantTarget, wantMissed = 4, []string{short(ids[3])}
		}
		if !reflect.DeepEqual(r.UniqueFoundIDs, wantIDs) || !reflect.DeepEqual(r.UniqueFoundSrc, wantSrc) {
			t.Errorf("score all %v: uniqueFoundIds %v uniqueFoundSrc %v, want %v %v", scoreAll, r.UniqueFoundIDs, r.UniqueFoundSrc, wantIDs, wantSrc)
		}
		if !reflect.DeepEqual(r.LookupWalkQueries, []int{4}) {
			t.Errorf("score all %v: lookupWalkQueries %v", scoreAll, r.LookupWalkQueries)
		}
		if r.Target != wantTarget || r.Missed != len(wantMissed) || !reflect.DeepEqual(r.MissedIDs, wantMissed) {
			t.Errorf("score all %v: target %d missed %d %v, want %d %d %v", scoreAll, r.Target, r.Missed, r.MissedIDs, wantTarget, len(wantMissed), wantMissed)
		}
	}
}
