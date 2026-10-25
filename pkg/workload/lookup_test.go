package workload

import (
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"
)

func testNode(b byte) *enode.Node {
	var id enode.ID
	id[0] = b
	var r enr.Record
	return enode.SignNull(&r, id)
}

// A continuous search is one lookup covering the whole search: it does not stop
// at a target, counts each registrant once, and reports progress as it goes.
func TestContinuous(t *testing.T) {
	nodes := []*enode.Node{testNode(1), testNode(2), testNode(1), testNode(3), testNode(2)}
	var updates []Lookup
	Continuous(func() enode.Iterator { return enode.IterNodes(nodes) }, nil, 0, 0, time.Now().Add(time.Minute), nil, func(l Lookup) {
		updates = append(updates, l)
	})
	if len(updates) != 4 {
		t.Fatalf("got %d updates, want 3 on new registrants and 1 at the end", len(updates))
	}
	final := updates[len(updates)-1]
	if final.Results != 3 || len(final.Found) != 3 || final.HitTarget {
		t.Fatalf("final lookup %+v, want 3 distinct results and no target", final)
	}
	for i, l := range updates[:3] {
		if l.Results != i+1 {
			t.Fatalf("update %d has %d results, want %d", i, l.Results, i+1)
		}
	}
}

// A paced continuous search takes the initial registrants at once, then waits
// one interval before each further new registrant.
func TestContinuousPacing(t *testing.T) {
	nodes := []*enode.Node{testNode(1), testNode(2), testNode(3), testNode(4)}
	const interval = 40 * time.Millisecond
	start := time.Now()
	var final Lookup
	Continuous(func() enode.Iterator { return enode.IterNodes(nodes) }, nil, 2, interval, time.Now().Add(time.Minute), nil, func(l Lookup) {
		final = l
	})
	if final.Results != 4 {
		t.Fatalf("got %d results, want 4", final.Results)
	}
	if d := time.Since(start); d < 2*interval {
		t.Fatalf("paced search took %v, want at least %v", d, 2*interval)
	}
	if at := final.Found[1].AtMs; at > 20 {
		t.Fatalf("second registrant (within initial) found after %d ms", at)
	}
}

// blockIter yields nothing until closed.
type blockIter struct{ closed chan struct{} }

func (it *blockIter) Next() bool        { <-it.closed; return false }
func (it *blockIter) Node() *enode.Node { return nil }
func (it *blockIter) Close()            { close(it.closed) }

// A stop during the wait for the next lookup, or during a lookup, returns at
// once and records nothing.
func TestScheduledStops(t *testing.T) {
	open := func() enode.Iterator { return &blockIter{closed: make(chan struct{})} }
	for _, first := range []time.Duration{time.Minute, 0} {
		stop := make(chan struct{})
		var recorded []Lookup
		done := make(chan struct{})
		go func() {
			Scheduled(open, nil, 1, 0, []time.Time{time.Now().Add(first), time.Now().Add(2 * time.Minute)}, time.Now().Add(time.Hour), stop, func(l Lookup) { recorded = append(recorded, l) })
			close(done)
		}()
		time.Sleep(50 * time.Millisecond)
		close(stop)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("first lookup at +%v: Scheduled did not return after stop", first)
		}
		if len(recorded) != 0 {
			t.Fatalf("first lookup at +%v: recorded %d lookups after stop, want 0", first, len(recorded))
		}
	}
}
