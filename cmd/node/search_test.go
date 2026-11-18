package main

import (
	"testing"
	"time"

	"github.com/datahop/topdisc-testbed/pkg/workload"
	"github.com/ethereum/go-ethereum/p2p/discover"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"
)

func svcNode(b byte, svc *svcEntry) *enode.Node {
	var id enode.ID
	id[0] = b
	var r enr.Record
	if svc != nil {
		r.Set(*svc)
	}
	return enode.SignNull(&r, id)
}

// statsIter is a topic search fake: IterNodes with search progress, recording Close.
type statsIter struct {
	enode.Iterator
	closed bool
}

func (it *statsIter) Stats() discover.TopicSearchStats { return discover.TopicSearchStats{Queries: 3} }
func (it *statsIter) Contacts() (int, int)             { return 4, 5 }
func (it *statsIter) Close()                           { it.closed = true; it.Iterator.Close() }

type closeIter struct {
	enode.Iterator
	closed bool
}

func (it *closeIter) Close() { it.closed = true; it.Iterator.Close() }

// The mixed search yields the providers of both sources, each tagged with its
// source, drops nodes of other services, and forwards the topic search's
// progress.
func TestMixedSearch(t *testing.T) {
	own, other := svcOf("a"), svcOf("b")
	topic := &statsIter{Iterator: enode.IterNodes([]*enode.Node{svcNode(1, &own), svcNode(2, &other), svcNode(3, &own)})}
	walk := &closeIter{Iterator: enode.IterNodes([]*enode.Node{svcNode(4, nil), svcNode(5, &own), svcNode(6, &own)})}
	n := &node{svc: own}
	sent := int64(10)
	m := newMixedSearch(topic, walk, n.isProvider, func() int64 { return sent })
	want := map[byte]string{1: "topic", 3: "topic", 5: "walk", 6: "walk"}
	got := map[byte]string{}
	for len(got) < len(want) {
		res := make(chan bool, 1)
		go func() { res <- m.Next() }()
		select {
		case ok := <-res:
			if !ok {
				t.Fatalf("mix ended after %v", got)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("mix blocked after %v", got)
		}
		if b := m.Node().ID()[0]; b == 2 || b == 4 {
			t.Fatalf("node %d of another or no service yielded", b)
		}
		got[m.Node().ID()[0]] = m.NodeSource()
	}
	for b, src := range want {
		if got[b] != src {
			t.Errorf("node %d: source %q, want %q", b, got[b], src)
		}
	}
	if st := m.Stats(); st.Queries != 3 {
		t.Errorf("stats not forwarded: %+v", st)
	}
	if q, c := m.Contacts(); q != 4 || c != 5 {
		t.Errorf("contacts %d %d, want 4 5", q, c)
	}
	sent = 17
	if q := m.WalkQueries(); q != 7 {
		t.Errorf("walk queries %d, want 7", q)
	}
	m.Close()
	if !topic.closed || !walk.closed {
		t.Errorf("close: topic %v walk %v, want both closed", topic.closed, walk.closed)
	}
}

// blockIter yields nothing until closed.
type blockIter struct{ closed chan struct{} }

func (it *blockIter) Next() bool        { <-it.closed; return false }
func (it *blockIter) Node() *enode.Node { return nil }
func (it *blockIter) Close()            { close(it.closed) }

// A search over sources that never yield still ends at its deadline, and
// closing it releases both sources.
func TestMixedSearchDeadline(t *testing.T) {
	topic, walk := &blockIter{make(chan struct{})}, &blockIter{make(chan struct{})}
	m := newMixedSearch(topic, walk, func(*enode.Node) bool { return true }, func() int64 { return 0 })
	done := make(chan struct{})
	go func() {
		workload.Continuous(func() enode.Iterator { return m }, nil, 0, 0, time.Now().Add(100*time.Millisecond), nil, func(workload.Lookup) {})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("search did not end at its deadline")
	}
	for name, c := range map[string]chan struct{}{"topic": topic.closed, "walk": walk.closed} {
		select {
		case <-c:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s source not closed", name)
		}
	}
}
