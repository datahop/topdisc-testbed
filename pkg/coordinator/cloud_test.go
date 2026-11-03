package coordinator

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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

func TestEachByHostCapsPerHost(t *testing.T) {
	const hosts, n = 20, 400
	var mu sync.Mutex
	inFlight, peak := map[int]int{}, map[int]int{}
	total, maxTotal, done := 0, 0, 0
	err := eachByHost(n, func(i int) int { return i / (n / hosts) }, func(i int) error {
		h := i / (n / hosts)
		mu.Lock()
		inFlight[h]++
		total++
		peak[h] = max(peak[h], inFlight[h])
		maxTotal = max(maxTotal, total)
		mu.Unlock()
		time.Sleep(time.Millisecond)
		mu.Lock()
		inFlight[h]--
		total--
		done++
		mu.Unlock()
		if i == 7 {
			return errors.New("seven")
		}
		return nil
	})
	if err == nil || err.Error() != "seven" {
		t.Errorf("err %v, want seven", err)
	}
	if done != n {
		t.Errorf("%d of %d done", done, n)
	}
	for h := 0; h < hosts; h++ {
		if peak[h] > perHostFetches {
			t.Errorf("host %d: %d in flight, cap %d", h, peak[h], perHostFetches)
		}
	}
	if maxTotal > 64 || maxTotal <= perHostFetches {
		t.Errorf("%d in flight overall: want hosts in parallel, at most 64", maxTotal)
	}
}
