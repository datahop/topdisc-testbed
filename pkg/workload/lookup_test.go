package workload

import (
	"encoding/json"
	"sync"
	"sync/atomic"
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
	if f := final.Found[0]; f.Src != "" || f.Both || final.WalkQueries != 0 {
		t.Fatalf("untagged iterator: found %+v, walk queries %d", f, final.WalkQueries)
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

// oneIter yields one node, then nothing until closed.
type oneIter struct {
	nd     *enode.Node
	n      int
	closed chan struct{}
}

func (it *oneIter) Next() bool {
	if it.n++; it.n == 1 {
		return true
	}
	<-it.closed
	return false
}
func (it *oneIter) Node() *enode.Node { return it.nd }
func (it *oneIter) Close()            { close(it.closed) }

// A lookup that runs into the deadline is recorded, also when the stop that
// comes at the deadline wins the race with it. A run the scheduler held past
// the deadline before the lookup got busy tests nothing and is skipped.
func TestScheduledStopAtDeadline(t *testing.T) {
	tested := 0
	for i := 0; i < 10; i++ {
		deadline := time.Now().Add(50 * time.Millisecond)
		stop := make(chan struct{})
		open := func() enode.Iterator { return &oneIter{nd: testNode(1), closed: make(chan struct{})} }
		var busy time.Time
		// Still busy at the deadline, then stopped: both end the lookup at once.
		isRegistrant := func(enode.ID) bool {
			busy = time.Now()
			time.Sleep(time.Until(deadline) + time.Millisecond)
			close(stop)
			return true
		}
		var recorded []Lookup
		Scheduled(open, isRegistrant, 2, time.Minute, []time.Time{time.Now()}, deadline, stop, func(l Lookup) { recorded = append(recorded, l) })
		if busy.IsZero() || !busy.Before(deadline) {
			continue
		}
		tested++
		if len(recorded) != 1 || recorded[0].Results != 1 {
			t.Fatalf("run %d: recorded %+v, want the lookup cut by the deadline", i, recorded)
		}
	}
	if tested == 0 {
		t.Skip("no lookup got busy before its deadline")
	}
}

// A lookup that overruns the deadline without a stop ends the schedule: the
// next lookup, due before the deadline, is not started.
func TestScheduledOverrunEnds(t *testing.T) {
	tested := 0
	for i := 0; i < 10; i++ {
		start := time.Now()
		deadline := start.Add(50 * time.Millisecond)
		open := func() enode.Iterator { return &oneIter{nd: testNode(1), closed: make(chan struct{})} }
		var busy time.Time
		isRegistrant := func(enode.ID) bool {
			if busy.IsZero() {
				busy = time.Now()
				time.Sleep(time.Until(deadline) + 20*time.Millisecond)
			}
			return true
		}
		var recorded []Lookup
		Scheduled(open, isRegistrant, 2, time.Minute, []time.Time{start, start.Add(10 * time.Millisecond)}, deadline, make(chan struct{}), func(l Lookup) { recorded = append(recorded, l) })
		if busy.IsZero() || !busy.Before(deadline) {
			continue
		}
		tested++
		if len(recorded) != 1 || recorded[0].Results != 1 {
			t.Fatalf("run %d: recorded %+v, want only the overrunning lookup", i, recorded)
		}
	}
	if tested == 0 {
		t.Skip("no lookup got busy before its deadline")
	}
}

// srcIter yields nodes with a per-node source and reports walk queries.
type srcIter struct {
	nodes []*enode.Node
	srcs  []string
	i     int
}

func (it *srcIter) Next() bool         { it.i++; return it.i <= len(it.nodes) }
func (it *srcIter) Node() *enode.Node  { return it.nodes[it.i-1] }
func (it *srcIter) NodeSource() string { return it.srcs[it.i-1] }
func (it *srcIter) Close()             {}
func (it *srcIter) WalkQueries() int   { return 7 }

// Each registrant carries the source that returned it first, and both when
// the other source returned it again; a repeat is not a new result.
func TestContinuousSources(t *testing.T) {
	it := &srcIter{
		nodes: []*enode.Node{testNode(1), testNode(2), testNode(1), testNode(3)},
		srcs:  []string{"topic", "walk", "walk", "topic"},
	}
	var final Lookup
	Continuous(func() enode.Iterator { return it }, nil, 0, 0, time.Now().Add(time.Minute), nil, func(l Lookup) { final = l })
	if final.Results != 3 || len(final.Found) != 3 {
		t.Fatalf("final lookup %+v, want 3 results", final)
	}
	want := []Found{{Src: "topic", Both: true}, {Src: "walk"}, {Src: "topic"}}
	for i, f := range final.Found {
		if f.Src != want[i].Src || f.Both != want[i].Both {
			t.Errorf("found[%d] src %q both %v, want %q %v", i, f.Src, f.Both, want[i].Src, want[i].Both)
		}
	}
	if final.WalkQueries != 7 {
		t.Errorf("walk queries %d, want 7", final.WalkQueries)
	}
}

// slowIter is srcIter with a pause before each node after the first.
type slowIter struct{ srcIter }

func (it *slowIter) Next() bool {
	if it.i > 0 {
		time.Sleep(20 * time.Millisecond)
	}
	return it.srcIter.Next()
}

// A published lookup is a snapshot: marshalling it while the search goes on
// and marks a repeat as both must not race, and the flag reaches the reader.
func TestContinuousPublishesSnapshots(t *testing.T) {
	it := &slowIter{srcIter{nodes: []*enode.Node{testNode(1), testNode(1), testNode(2)}, srcs: []string{"topic", "walk", "topic"}}}
	var (
		mu      sync.Mutex
		last    Lookup
		stopped atomic.Bool
		reader  sync.WaitGroup
	)
	reader.Add(1)
	go func() {
		defer reader.Done()
		for !stopped.Load() {
			mu.Lock()
			l := last
			mu.Unlock()
			json.Marshal(l)
		}
	}()
	var bothSeen bool
	Continuous(func() enode.Iterator { return it }, nil, 0, 0, time.Now().Add(time.Minute), nil, func(l Lookup) {
		mu.Lock()
		last = l
		if l.Results == 1 && l.Found[0].Both {
			bothSeen = true
		}
		mu.Unlock()
	})
	stopped.Store(true)
	reader.Wait()
	if !bothSeen {
		t.Error("no update carried both before the next new registrant")
	}
}
