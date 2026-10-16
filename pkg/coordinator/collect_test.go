package coordinator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/datahop/topdisc-testbed/pkg/assign"
)

// An all-legacy run (legacy_bootnode, legacy_build fork) has no TopDisc node:
// Collect must neither panic nor leave the legacy nodes out of oh.json.
func TestCollectAllLegacy(t *testing.T) {
	dir := t.TempDir()
	trDir := filepath.Join(dir, "traces")
	os.MkdirAll(trDir, 0o755)
	var as []assign.Assignment
	for i := 0; i < 3; i++ {
		as = append(as, assign.Assignment{Idx: i, Legacy: true, LegacyFork: true, Topics: []string{"t"}})
		tr := map[string]any{"idx": i, "id": fmt.Sprintf("%064x", i+1), "legacy": true, "outbound": 4, "inbound": 3,
			"lookups": []any{}, "first_capable_ms": -1,
			"wire": map[string]any{"FINDNODE/v5": map[string]int64{"txMsgs": 10, "txBytes": 900, "rxMsgs": 8, "rxBytes": 700}}}
		b, _ := json.Marshal(tr)
		os.WriteFile(filepath.Join(trDir, fmt.Sprintf("node%d.json", i)), b, 0o644)
	}
	if err := Collect(trDir, dir, as); err != nil {
		t.Fatal(err)
	}
	var oh []map[string]any
	b, _ := os.ReadFile(filepath.Join(dir, "oh.json"))
	if err := json.Unmarshal(b, &oh); err != nil || len(oh) != 3 {
		t.Fatalf("oh.json: %d entries (err %v), want 3", len(oh), err)
	}
	if oh[0]["txPkts"].(float64) != 10 {
		t.Fatalf("oh.json[0] txPkts=%v, want 10", oh[0]["txPkts"])
	}
}
