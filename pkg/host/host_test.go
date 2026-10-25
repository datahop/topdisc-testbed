package host

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newRunner(t *testing.T, bin string) *Runner {
	return &Runner{NodeBinary: bin, AsgDir: t.TempDir(), LogDir: t.TempDir()}
}

func waitExit(t *testing.T, r *Runner, idx int) {
	t.Helper()
	r.mu.Lock()
	p := r.procs[idx]
	r.mu.Unlock()
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
		t.Fatalf("node %d did not exit", idx)
	}
}

func nodeLog(t *testing.T, r *Runner, idx int) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(r.LogDir, fmt.Sprintf("node%d.log", idx)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestReapLogsOwnExit(t *testing.T) {
	for _, tc := range []struct{ bin, want string }{
		{"/bin/true", "[host] exited on its own: exit status 0 at "},
		{"/bin/false", "[host] exited on its own: exit status 1 at "},
	} {
		r := newRunner(t, tc.bin)
		if err := r.Start(1); err != nil {
			t.Fatal(err)
		}
		waitExit(t, r, 1)
		if got := nodeLog(t, r, 1); !strings.Contains(got, tc.want) {
			t.Errorf("%s: node log %q, want %q", tc.bin, got, tc.want)
		}
	}
}

func TestTerminateLogsKill(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "node.sh")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := newRunner(t, bin)
	if err := r.Start(2); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if !r.terminate(2) {
		t.Fatal("terminate(2) = false for a running node")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("terminate took %s", d)
	}
	if got := nodeLog(t, r, 2); !strings.Contains(got, "[host] exited: signal: terminated at ") {
		t.Errorf("node log %q", got)
	}
	if r.terminate(2) {
		t.Error("second terminate(2) = true")
	}
}

func TestSampleSkipsExited(t *testing.T) {
	r := newRunner(t, "/bin/true")
	if err := r.Start(3); err != nil {
		t.Fatal(err)
	}
	waitExit(t, r, 3)
	r.mon = &monitor{lastCPU: map[int]float64{}}
	r.sample()
	if s := r.Samples(); len(s) != 1 || len(s[0].Nodes) != 0 {
		t.Errorf("samples %+v, want one with no nodes", s)
	}
}

// Down and a churn Start go to the node's own status port; the node's answer
// comes back as the error.
func TestRunnerDownUpAndStartDispatch(t *testing.T) {
	var calls []string
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls = append(calls, req.Method+" "+req.URL.Path)
		if len(calls) == 3 {
			http.Error(w, "already up", http.StatusConflict)
		}
	}))
	r := newRunner(t, "/nonexistent")
	r.init()
	r.status[3] = node.Listener.Addr().String()
	r.procs[3] = &proc{cmd: &exec.Cmd{}, done: make(chan struct{})}

	if err := r.Down(3); err != nil {
		t.Fatalf("Down: %v", err)
	}
	if err := r.Start(3); err != nil {
		t.Fatalf("Start of a started node: %v", err)
	}
	if err := r.Up(3); err == nil || !strings.Contains(err.Error(), "already up") {
		t.Errorf("Up on a 409: %v, want the node's answer", err)
	}
	if want := []string{"POST /down", "POST /up", "POST /up"}; fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Errorf("calls %v, want %v (no exec)", calls, want)
	}
	node.Close()
	if err := r.Down(3); err == nil {
		t.Error("Down to a closed node succeeded")
	}
}
