package main

import (
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"
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

	l := randomWalk(disc, nil, isProvider, 3, time.Minute, time.Now().Add(time.Minute), nil)

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

func randNode() *enode.Node {
	key, _ := crypto.GenerateKey()
	return enode.NewV4(&key.PublicKey, nil, 0, 0)
}

func capableNode(t *testing.T, entries ...enr.Entry) *enode.Node {
	key, _ := crypto.GenerateKey()
	var r enr.Record
	r.Set(enr.WithEntry("ng", []byte{1}))
	for _, e := range entries {
		r.Set(e)
	}
	if err := enode.SignV4(&r, key); err != nil {
		t.Fatal(err)
	}
	nd, err := enode.New(enode.ValidSchemes, &r)
	if err != nil {
		t.Fatal(err)
	}
	return nd
}

// Ghosts and bootnodes carry "ng" but do not count as a first capable peer,
// also a ghost another node pinned and passed on.
func TestHasCapable(t *testing.T) {
	key, _ := crypto.GenerateKey()
	n := newNode(assignment{Topics: []string{"t"}}, key)
	mark := enr.WithEntry("ghost", uint(1))
	own, foreign, boot, peer := capableNode(t, mark), capableNode(t, mark), capableNode(t), capableNode(t)
	n.ghostIDs[own.ID()] = struct{}{}
	n.bootIDs[boot.ID()] = struct{}{}
	for _, nd := range []*enode.Node{own, foreign, boot} {
		if n.hasCapable([]*enode.Node{randNode(), nd}) {
			t.Fatalf("%v counted as capable", nd.ID())
		}
	}
	if !n.hasCapable([]*enode.Node{randNode(), own, foreign, peer}) {
		t.Fatal("capable peer not counted")
	}
}

func TestWalkIteratorReadsEveryNode(t *testing.T) {
	// One provider in the table, two among the five nodes the walk yields;
	// the walk then blocks until the deadline closes it.
	inTable, p1, p2 := randNode(), randNode(), randNode()
	providers := map[enode.ID]bool{inTable.ID(): true, p1.ID(): true, p2.ID(): true}
	isProvider := func(n *enode.Node) bool { return providers[n.ID()] }
	disc := &fakeWalk{table: []*enode.Node{inTable, randNode()},
		it: &fakeIter{nodes: []*enode.Node{randNode(), p1, randNode(), p2, p1}, closed: make(chan struct{})}}
	var snaps []lookup
	progress := func(l lookup) { snaps = append(snaps, l) }
	calls := 0
	cost := func() int64 { calls++; return 100 + int64(calls) }

	start := time.Now()
	l := walkIterator(disc, cost, isProvider, start.Add(200*time.Millisecond), nil, progress, 0, 0)

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

	l := randomWalk(disc, cost, isProvider, 3, time.Minute, time.Now().Add(time.Minute), nil)

	if l.Queries != 36 {
		t.Fatalf("queries=%d, want 36 requests sent by the walk's 3 lookups", l.Queries)
	}
}

func TestWalkIteratorPacesAfterInitial(t *testing.T) {
	// Four providers at once; after the first, the walk takes one more per
	// 50 ms, so the fourth is recorded at least 100 ms in.
	ps := []*enode.Node{randNode(), randNode(), randNode(), randNode()}
	providers := map[enode.ID]bool{}
	for _, p := range ps {
		providers[p.ID()] = true
	}
	isProvider := func(n *enode.Node) bool { return providers[n.ID()] }
	disc := &fakeWalk{it: &fakeIter{nodes: ps, closed: make(chan struct{})}}

	l := walkIterator(disc, nil, isProvider, time.Now().Add(400*time.Millisecond), nil, nil, 1, 50*time.Millisecond)

	if len(l.Found) != 4 || l.Queries != -1 {
		t.Fatalf("found=%d queries=%d, want 4 providers and no request count", len(l.Found), l.Queries)
	}
	if l.Found[0].AtMs > 20 || l.Found[3].AtMs < 100 {
		t.Fatalf("first at %d ms, fourth at %d ms: want the first at once and the fourth paced past 100 ms", l.Found[0].AtMs, l.Found[3].AtMs)
	}
}

// A departure ends a walk blocked on its iterator at once, with what it had.
func TestWalkIteratorStops(t *testing.T) {
	p := randNode()
	isProvider := func(n *enode.Node) bool { return n.ID() == p.ID() }
	disc := &fakeWalk{it: &fakeIter{nodes: []*enode.Node{p}, closed: make(chan struct{})}}
	stop := make(chan struct{})
	time.AfterFunc(50*time.Millisecond, func() { close(stop) })

	start := time.Now()
	l := walkIterator(disc, nil, isProvider, start.Add(time.Minute), stop, nil, 0, 0)

	if d := time.Since(start); d > 150*time.Millisecond {
		t.Fatalf("walk returned %v after start, want within 100 ms of the stop at 50 ms", d)
	}
	if len(l.Found) != 1 || l.Found[0].ID != p.ID().String() {
		t.Fatalf("found %v, want the provider seen before the stop", l.Found)
	}
}

func TestTraceAccumulatesSessions(t *testing.T) {
	key, _ := crypto.GenerateKey()
	n := newNode(assignment{Idx: 4, Topics: []string{"t"}}, key)
	w1, w2, sum := testWire()
	s1 := newSession(time.UnixMilli(1000))
	s1.wire, s1.lookups = w1, []lookup{{StartMs: 1100}}
	s1.connStart, s1.connFound = time.UnixMilli(1200), []found{{ID: "p1", AtMs: 5}}
	n.cur, n.drops, n.firstCapableMs = s1, 1, -1
	n.ghosts = []*enode.Node{randNode(), randNode()}
	s1.ghostsInTable, s1.tableSize = 1, 5
	if forkBuild {
		n.addSample(sample{AtMs: 1500, Wire: w1})
	}
	n.fold(s1, time.UnixMilli(2000))
	s2 := newSession(time.UnixMilli(3000))
	s2.wire, s2.lookups, s2.firstCapableMs = w2, []lookup{{StartMs: 3100}}, 70
	s2.connStart, s2.connFound = time.UnixMilli(3200), []found{{ID: "p2", AtMs: 7}}
	n.cur, n.drops = s2, n.drops+2
	s2.ghostsInTable, s2.tableSize = 2, 9
	if forkBuild {
		n.addSample(sample{AtMs: 3500, Wire: w2})
	}
	m := n.trace()

	if w := m["wire"].(map[string]wireCounter); !reflect.DeepEqual(w, sum) {
		t.Errorf("wire %v, want %v", w, sum)
	}
	smp := m["samples"].([]sample)
	if forkBuild && (len(smp) != 2 || !reflect.DeepEqual(smp[1].Wire, sum)) {
		t.Errorf("second session's sample does not carry the first's totals: %+v", smp)
	}
	if !forkBuild && len(smp) != 0 {
		t.Errorf("stock build has samples: %+v", smp)
	}
	if l := m["lookups"].([]lookup); len(l) != 2 || l[0].StartMs != 1100 || l[1].StartMs != 3100 {
		t.Errorf("lookups %+v", l)
	}
	if m["peer_drops"] != 3 || m["first_capable_ms"] != int64(70) || m["legacy"] != true {
		t.Errorf("peer_drops %v first_capable_ms %v legacy %v", m["peer_drops"], m["first_capable_ms"], m["legacy"])
	}
	conn := m["conn"].(map[string]any)
	if f := conn["found"].([]found); conn["start_ms"] != int64(3200) || len(f) != 2 || f[0].AtMs != 1205-3200 || f[1].AtMs != 7 {
		t.Errorf("conn %v", conn)
	}
	if ss := m["sessions"].([]span); len(ss) != 2 || ss[0] != (span{1000, 2000}) || ss[1].StartMs != 3000 {
		t.Errorf("sessions %v", ss)
	}
	if _, ok := m["session"]; ok {
		t.Error("trace has a session key")
	}
	if m["ghosts_assigned"] != 2 || m["ghosts_in_table"] != 2 || m["table_size"] != 9 {
		t.Errorf("ghosts_assigned %v ghosts_in_table %v table_size %v, want the last session's 2 2 9", m["ghosts_assigned"], m["ghosts_in_table"], m["table_size"])
	}
	n.fold(s2, time.UnixMilli(4000))
	if m := n.trace(); m["ghosts_in_table"] != 2 || m["table_size"] != 9 {
		t.Errorf("after the last session closed: ghosts_in_table %v table_size %v, want 2 9", m["ghosts_in_table"], m["table_size"])
	}
}
