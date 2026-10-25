package coordinator

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestChurnLogBounded(t *testing.T) {
	var buf bytes.Buffer
	cl := &churnLog{out: &buf}
	cl.kill(100, nil)
	cl.start(100, nil)
	for i := 0; i < churnFailShown+5; i++ {
		cl.start(i, errors.New("start refused"))
	}
	cl.kill(200, errors.New("node 200 not running"))

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != churnFailShown+1 {
		t.Fatalf("printed %d lines, want %d:\n%s", len(lines), churnFailShown+1, buf.String())
	}
	if !strings.Contains(lines[0], "start node 0:") || !strings.Contains(lines[0], "start refused") {
		t.Errorf("first line %q", lines[0])
	}
	if want := "departs=1 rejoins=1 failed kills=1 starts=25"; cl.summary() != want {
		t.Errorf("summary %q, want %q", cl.summary(), want)
	}
}
