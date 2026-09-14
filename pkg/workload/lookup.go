// Package workload is the per-node behaviour shared by backends: what a node
// does with discovery once it is up. Peer slots are the p2p server's business;
// this package only drives lookups and records what they returned.
package workload

import (
	"time"

	"github.com/ethereum/go-ethereum/p2p/enode"
)

// Lookup is one search: how long it took to reach the target, how many
// distinct registrants it returned, and whether it got there before timing out.
type Lookup struct {
	StartMs   int64   `json:"start_ms"`
	LatencyMs int64   `json:"latency_ms"`
	FirstMs   int64   `json:"first_ms"` // to the first result; -1 = none
	Results   int     `json:"results"`
	HitTarget bool    `json:"hit_target"`
	Found     []Found `json:"found"`     // distinct registrants in the order seen
	Queries   int     `json:"queries"`   // TOPICQUERY requests sent; -1 = not reported
	Contacted int     `json:"contacted"` // distinct nodes queried; -1 = not reported
}

// Found is one distinct registrant a lookup returned and when (ms since the
// lookup started).
type Found struct {
	ID   string `json:"id"`
	AtMs int64  `json:"at_ms"`
}

// Continuous runs one search until the deadline and reports it as a single
// lookup, passed to update each time a new registrant is found and when the
// search ends. isRegistrant filters what counts as a result; nil counts every
// node.
func Continuous(open func() enode.Iterator, isRegistrant func(enode.ID) bool, deadline time.Time, update func(Lookup)) {
	update(runOne(open, isRegistrant, 0, 0, deadline, update))
}

// Scheduled runs one lookup at each of the given times (a lookup that overruns
// the next time starts the next one at once), until the deadline.
func Scheduled(open func() enode.Iterator, isRegistrant func(enode.ID) bool, target int, timeout time.Duration, at []time.Time, deadline time.Time, record func(Lookup)) {
	for _, t := range at {
		if !t.Before(deadline) {
			return
		}
		select {
		case <-time.After(time.Until(t)):
		case <-time.After(time.Until(deadline)):
			return
		}
		record(runOne(open, isRegistrant, target, timeout, deadline, nil))
	}
}

func runOne(open func() enode.Iterator, isRegistrant func(enode.ID) bool, target int, timeout time.Duration, deadline time.Time, progress func(Lookup)) Lookup {
	start := time.Now()
	it := open()
	defer func() { go it.Close() }() // Close can block on a busy search; do not hold the loop
	seen := map[enode.ID]struct{}{}
	found := []Found{}
	stop := deadline
	if timeout > 0 && start.Add(timeout).Before(stop) {
		stop = start.Add(timeout)
	}
	done := func(hit bool) Lookup {
		l := Lookup{StartMs: start.UnixMilli(), LatencyMs: time.Since(start).Milliseconds(), FirstMs: -1, Results: len(seen), HitTarget: hit, Found: found, Queries: -1, Contacted: -1}
		if c, ok := it.(interface{ Contacts() (int, int) }); ok {
			l.Queries, l.Contacted = c.Contacts()
		}
		if len(found) > 0 {
			l.FirstMs = found[0].AtMs
		}
		return l
	}
	next := make(chan bool, 1)
	for {
		go func() { next <- it.Next() }()
		select {
		case ok := <-next:
			if !ok {
				return done(false)
			}
		case <-time.After(time.Until(stop)):
			return done(false)
		}
		id := it.Node().ID()
		if isRegistrant == nil || isRegistrant(id) {
			if _, dup := seen[id]; !dup {
				seen[id] = struct{}{}
				found = append(found, Found{ID: id.String(), AtMs: time.Since(start).Milliseconds()})
				if progress != nil {
					progress(done(false))
				}
			}
		}
		if target > 0 && len(seen) >= target {
			return done(true)
		}
	}
}
