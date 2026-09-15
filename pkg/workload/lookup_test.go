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
	Continuous(func() enode.Iterator { return enode.IterNodes(nodes) }, nil, 0, 0, time.Now().Add(time.Minute), func(l Lookup) {
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
	Continuous(func() enode.Iterator { return enode.IterNodes(nodes) }, nil, 2, interval, time.Now().Add(time.Minute), func(l Lookup) {
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
