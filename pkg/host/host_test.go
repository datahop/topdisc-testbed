package host

import (
	"fmt"
	"os"
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

func TestKillLogsKill(t *testing.T) {
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
	if !r.Kill(2) {
		t.Fatal("Kill(2) = false for a running node")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Kill took %s", d)
	}
	if got := nodeLog(t, r, 2); !strings.Contains(got, "[host] exited: signal: terminated at ") {
		t.Errorf("node log %q", got)
	}
	if r.Kill(2) {
		t.Error("second Kill(2) = true")
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
