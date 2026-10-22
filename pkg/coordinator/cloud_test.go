package coordinator

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFetchLogsForNodesWithoutTrace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/log" && r.URL.Query().Get("idx") == "1" {
			fmt.Fprint(w, "node 1 died")
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	dir := t.TempDir()
	failed := map[int]error{1: errors.New("trace 1: 404"), 2: errors.New("trace 2: 404")}

	got, sample := fetchLogs([]agent{{srv.URL}}, []int{0, 0, 0}, failed, dir)
	if got != 1 {
		t.Errorf("fetched %d logs, want 1", got)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "node1.log")); err != nil || string(b) != "node 1 died" {
		t.Errorf("node1.log = %q, %v", b, err)
	}
	for _, f := range []string{"node0.log", "node2.log"} {
		if _, err := os.Stat(filepath.Join(dir, f)); !os.IsNotExist(err) {
			t.Errorf("%s exists (err %v)", f, err)
		}
	}
	if !strings.Contains(sample, "node 2") {
		t.Errorf("sample %q does not name node 2", sample)
	}
}
