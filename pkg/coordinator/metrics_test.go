package coordinator

import (
	"encoding/json"
	"testing"
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
