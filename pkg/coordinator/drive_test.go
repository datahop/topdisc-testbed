package coordinator

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/datahop/topdisc-testbed/pkg/assign"
	"github.com/datahop/topdisc-testbed/pkg/churn"
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

// planned counts what drive applies: no departure for an arrival, nothing at
// or past StopAt.
func TestPlannedSkipsArrivalsAndStop(t *testing.T) {
	as := make([]assign.Assignment, 3)
	as[0].Phases.SearchAt, as[0].Phases.StopAt = 1000, 101000 // 100 s window
	as[1].Churn = []churn.Event{{DownAt: 0, UpAt: 10}, {DownAt: 50, UpAt: 100}}
	as[2].Churn = []churn.Event{{DownAt: 20, UpAt: 30}, {DownAt: 100, UpAt: 100}}
	if d, r := planned(as); d != 2 || r != 2 {
		t.Errorf("planned departs=%d rejoins=%d, want 2 and 2", d, r)
	}
}
