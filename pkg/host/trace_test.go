package host

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadTraceMergesSessions(t *testing.T) {
	dir := t.TempDir()
	trace := filepath.Join(dir, "node3.json")
	s1 := `{"idx":3,"id":"a","peer_drops":1,"first_capable_ms":-1,"wire":{"PING":{"txBytes":10,"txMsgs":1}},
		"samples":[{"at_ms":100,"wire":{"PING":{"txBytes":10,"txMsgs":1}}}],
		"lookups":[{"start_ms":100,"found":[{"id":"p1","at_ms":5}]}],
		"conn":{"start_ms":100,"found":[{"id":"p1","at_ms":5}]},"session":{"start_ms":0,"end_ms":200}}`
	s2 := `{"idx":3,"id":"a","peer_drops":2,"first_capable_ms":50,"wire":{"PING":{"txBytes":5,"txMsgs":2},"PONG":{"txBytes":1}},
		"samples":[{"at_ms":400,"wire":{"PING":{"txBytes":5,"txMsgs":2}}}],
		"lookups":[{"start_ms":300,"found":[{"id":"p2","at_ms":7}]}],
		"conn":{"start_ms":300,"found":[{"id":"p2","at_ms":7}]},"session":{"start_ms":300,"end_ms":500}}`
	os.WriteFile(trace, []byte(s2), 0o644)
	os.WriteFile(sessionPath(trace, 1), []byte(s1), 0o644)
	b, err := LoadTrace(trace)
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		PeerDrops int                           `json:"peer_drops"`
		Capable   int                           `json:"first_capable_ms"`
		Wire      map[string]map[string]float64 `json:"wire"`
		Samples   []struct {
			Wire map[string]map[string]float64 `json:"wire"`
		} `json:"samples"`
		Lookups  []any `json:"lookups"`
		Sessions []any `json:"sessions"`
		Conn     struct {
			StartMs int64 `json:"start_ms"`
			Found   []struct {
				ID   string `json:"id"`
				AtMs int64  `json:"at_ms"`
			} `json:"found"`
		} `json:"conn"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m.PeerDrops != 3 || m.Capable != 50 || m.Wire["PING"]["txBytes"] != 15 || m.Wire["PONG"]["txBytes"] != 1 {
		t.Errorf("counters: %+v", m)
	}
	if len(m.Lookups) != 2 || len(m.Sessions) != 2 {
		t.Errorf("lists: lookups %d sessions %d", len(m.Lookups), len(m.Sessions))
	}
	if m.Samples[1].Wire["PING"]["txBytes"] != 15 {
		t.Errorf("second session's sample not carried on: %v", m.Samples[1].Wire)
	}
	if m.Conn.StartMs != 300 || len(m.Conn.Found) != 2 || m.Conn.Found[0].AtMs != -195 || m.Conn.Found[1].AtMs != 7 {
		t.Errorf("conn: %+v", m.Conn)
	}
	// One session: the file as written.
	os.Remove(sessionPath(trace, 1))
	if b, _ := LoadTrace(trace); string(b) != s2 {
		t.Error("single session altered")
	}
}

func TestRotateTrace(t *testing.T) {
	dir := t.TempDir()
	trace := filepath.Join(dir, "node0.json")
	if err := rotateTrace(trace); err != nil {
		t.Fatal(err)
	}
	for k := 1; k <= 2; k++ {
		os.WriteFile(trace, []byte("x"), 0o644)
		if err := rotateTrace(trace); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(sessionPath(trace, k)); err != nil {
			t.Fatalf("session %d not parked: %v", k, err)
		}
	}
}
