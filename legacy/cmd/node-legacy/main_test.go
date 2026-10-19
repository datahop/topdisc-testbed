package main

import (
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

// fakeDisc hands out one fresh provider per Lookup.
type fakeDisc struct {
	providers map[enode.ID]bool
	rounds    int
}

func (d *fakeDisc) AllNodes() []*enode.Node { return nil }

func (d *fakeDisc) Lookup(enode.ID) []*enode.Node {
	d.rounds++
	key, _ := crypto.GenerateKey()
	n := enode.NewV4(&key.PublicKey, nil, 0, 0)
	d.providers[n.ID()] = true
	return []*enode.Node{n}
}

func TestRandomWalkCountsRounds(t *testing.T) {
	disc := &fakeDisc{providers: map[enode.ID]bool{}}
	isProvider := func(n *enode.Node) bool { return disc.providers[n.ID()] }

	l := randomWalk(disc, nil, isProvider, 3, time.Minute, time.Now().Add(time.Minute))

	if !l.HitTarget || l.Results != 3 || len(l.Found) != 3 {
		t.Fatalf("walk: hit=%v results=%d found=%d, want 3 providers", l.HitTarget, l.Results, len(l.Found))
	}
	if l.Queries != disc.rounds || l.Queries != 3 {
		t.Fatalf("queries=%d, disc ran %d lookups, want 3", l.Queries, disc.rounds)
	}
	if l.Contacted != -1 {
		t.Fatalf("contacted=%d, want -1 (not reported)", l.Contacted)
	}
}

// fakeIter yields its nodes one by one, then blocks until closed, as the
// discovery walk does between lookups.
type fakeIter struct {
	nodes  []*enode.Node
	cur    *enode.Node
	closed chan struct{}
	once   sync.Once
}

func (it *fakeIter) Next() bool {
	if len(it.nodes) > 0 {
		it.cur, it.nodes = it.nodes[0], it.nodes[1:]
		return true
	}
	<-it.closed
	return false
}
func (it *fakeIter) Node() *enode.Node { return it.cur }
func (it *fakeIter) Close()            { it.once.Do(func() { close(it.closed) }) }

type fakeWalk struct {
	table []*enode.Node
	it    *fakeIter
}

func (d *fakeWalk) AllNodes() []*enode.Node     { return d.table }
func (d *fakeWalk) RandomNodes() enode.Iterator { return d.it }

func newNode() *enode.Node {
	key, _ := crypto.GenerateKey()
	return enode.NewV4(&key.PublicKey, nil, 0, 0)
}

func TestWalkIteratorReadsEveryNode(t *testing.T) {
	// One provider in the table, two among the five nodes the walk yields;
	// the walk then blocks until the deadline closes it.
	inTable, p1, p2 := newNode(), newNode(), newNode()
	providers := map[enode.ID]bool{inTable.ID(): true, p1.ID(): true, p2.ID(): true}
	isProvider := func(n *enode.Node) bool { return providers[n.ID()] }
	disc := &fakeWalk{table: []*enode.Node{inTable, newNode()},
		it: &fakeIter{nodes: []*enode.Node{newNode(), p1, newNode(), p2, p1}, closed: make(chan struct{})}}
	var snaps []lookup
	progress := func(l lookup) { snaps = append(snaps, l) }
	calls := 0
	cost := func() int64 { calls++; return 100 + int64(calls) }

	start := time.Now()
	l := walkIterator(disc, cost, isProvider, start.Add(200*time.Millisecond), progress, 0, 0)

	if time.Since(start) < 200*time.Millisecond {
		t.Fatalf("walk returned after %v, before the deadline", time.Since(start))
	}
	if l.Results != 3 || len(l.Found) != 3 || l.HitTarget || l.Contacted != -1 {
		t.Fatalf("walk: results=%d found=%d hit=%v contacted=%d, want 3 providers once each", l.Results, len(l.Found), l.HitTarget, l.Contacted)
	}
	if l.Found[0].ID != inTable.ID().String() || l.Found[1].ID != p1.ID().String() || l.Found[2].ID != p2.ID().String() {
		t.Fatalf("found order %v, want table hit, then p1, then p2", l.Found)
	}
	if l.Queries <= 0 {
		t.Fatalf("queries=%d, want the FINDNODE counter's growth", l.Queries)
	}
	// One snapshot for the table scan and one per new provider, each frozen
	// at its own length.
	if len(snaps) != 3 {
		t.Fatalf("progress called %d times, want 3", len(snaps))
	}
	for i, s := range snaps {
		if len(s.Found) != i+1 {
			t.Fatalf("snapshot %d holds %d found, want %d", i, len(s.Found), i+1)
		}
	}
}

func TestRandomWalkCountsRequests(t *testing.T) {
	disc := &fakeDisc{providers: map[enode.ID]bool{}}
	isProvider := func(n *enode.Node) bool { return disc.providers[n.ID()] }
	// A counter that already holds traffic from before the walk, and grows
	// by 12 FINDNODE requests per lookup.
	cost := func() int64 { return 100 + 12*int64(disc.rounds) }

	l := randomWalk(disc, cost, isProvider, 3, time.Minute, time.Now().Add(time.Minute))

	if l.Queries != 36 {
		t.Fatalf("queries=%d, want 36 requests sent by the walk's 3 lookups", l.Queries)
	}
}

func TestWalkIteratorPacesAfterInitial(t *testing.T) {
	// Four providers at once; after the first, the walk takes one more per
	// 50 ms, so the fourth is recorded at least 100 ms in.
	ps := []*enode.Node{newNode(), newNode(), newNode(), newNode()}
	providers := map[enode.ID]bool{}
	for _, p := range ps {
		providers[p.ID()] = true
	}
	isProvider := func(n *enode.Node) bool { return providers[n.ID()] }
	disc := &fakeWalk{it: &fakeIter{nodes: ps, closed: make(chan struct{})}}

	l := walkIterator(disc, nil, isProvider, time.Now().Add(400*time.Millisecond), nil, 1, 50*time.Millisecond)

	if len(l.Found) != 4 || l.Queries != -1 {
		t.Fatalf("found=%d queries=%d, want 4 providers and no request count", len(l.Found), l.Queries)
	}
	if l.Found[0].AtMs > 20 || l.Found[3].AtMs < 100 {
		t.Fatalf("first at %d ms, fourth at %d ms: want the first at once and the fourth paced past 100 ms", l.Found[0].AtMs, l.Found[3].AtMs)
	}
}
