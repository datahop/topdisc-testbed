package main

import (
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
	var snaps []lookup
	progress := func(l lookup) { snaps = append(snaps, l) }

	l := randomWalk(disc, nil, isProvider, 3, time.Minute, time.Now().Add(time.Minute), progress, 0, 0)

	if !l.HitTarget || l.Results != 3 || len(l.Found) != 3 {
		t.Fatalf("walk: hit=%v results=%d found=%d, want 3 providers", l.HitTarget, l.Results, len(l.Found))
	}
	if l.Queries != disc.rounds || l.Queries != 3 {
		t.Fatalf("queries=%d, disc ran %d lookups, want 3", l.Queries, disc.rounds)
	}
	if l.Contacted != -1 {
		t.Fatalf("contacted=%d, want -1 (not reported)", l.Contacted)
	}
	// One snapshot per round that did not hit the target; each keeps the
	// state of its own round after the walk moved on.
	if len(snaps) != 2 {
		t.Fatalf("progress called %d times, want 2", len(snaps))
	}
	for i, s := range snaps {
		if s.Queries != i+1 || len(s.Found) != i+1 || s.Found[i].ID != l.Found[i].ID {
			t.Fatalf("snapshot %d: queries=%d found=%d", i, s.Queries, len(s.Found))
		}
	}
}

func TestRandomWalkCountsRequests(t *testing.T) {
	disc := &fakeDisc{providers: map[enode.ID]bool{}}
	isProvider := func(n *enode.Node) bool { return disc.providers[n.ID()] }
	// A counter that already holds traffic from before the walk, and grows
	// by 12 FINDNODE requests per lookup.
	cost := func() int64 { return 100 + 12*int64(disc.rounds) }

	l := randomWalk(disc, cost, isProvider, 3, time.Minute, time.Now().Add(time.Minute), nil, 0, 0)

	if l.Queries != 36 {
		t.Fatalf("queries=%d, want 36 requests sent by the walk's 3 lookups", l.Queries)
	}
}
