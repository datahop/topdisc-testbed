// Package workload is the per-node behaviour shared by backends: what a node
// does with discovery once it is up. Peer slots are the p2p server's business;
// this package only drives lookups and records what they returned.
package workload

import (
	"time"

	"github.com/ethereum/go-ethereum/p2p/discover"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

// Lookup is one search: how long it took to reach the target, how many
// distinct registrants it returned, and whether it got there before timing out.
type Lookup struct {
	StartMs     int64                      `json:"start_ms"`
	LatencyMs   int64                      `json:"latency_ms"`
	FirstMs     int64                      `json:"first_ms"` // to the first result; -1 = none
	Results     int                        `json:"results"`
	HitTarget   bool                       `json:"hit_target"`
	Found       []Found                    `json:"found"`                  // distinct registrants in the order seen
	Queries     int                        `json:"queries"`                // TOPICQUERY requests sent; -1 = not reported
	Contacted   int                        `json:"contacted"`              // distinct nodes queried; -1 = not reported
	Search      *discover.TopicSearchStats `json:"search,omitempty"`       // search progress; nil = not reported
	WalkQueries int                        `json:"walk_queries,omitempty"` // FINDNODE requests sent during a search mixed with the random walk
}

// Found is one distinct registrant a lookup returned and when (ms since the
// lookup started). Src is the iterator source that returned it first; Both is
// set when another source returned it again.
type Found struct {
	ID   string `json:"id"`
	AtMs int64  `json:"at_ms"`
	Src  string `json:"src,omitempty"`
	Both bool   `json:"both,omitempty"`
}

// Continuous runs one search until the deadline and reports it as a single
// lookup, passed to update each time a new registrant is found and when the
// search ends. It takes initial new registrants at once, then one per interval
// (0 = as fast as they come). isRegistrant filters what counts as a result; nil
// counts every node. Closing stop ends the search early with what it found.
func Continuous(open func() enode.Iterator, isRegistrant func(enode.ID) bool, initial int, interval time.Duration, deadline time.Time, stop <-chan struct{}, update func(Lookup)) {
	l, _ := runOne(open, isRegistrant, 0, 0, deadline, stop, update, initial, interval)
	update(l)
}

// Scheduled runs one lookup at each of the given times (a lookup that overruns
// the next time starts the next one at once), until the deadline. Closing stop
// returns at once; a lookup it cuts short before the deadline is not recorded.
func Scheduled(open func() enode.Iterator, isRegistrant func(enode.ID) bool, target int, timeout time.Duration, at []time.Time, deadline time.Time, stop <-chan struct{}, record func(Lookup)) {
	for _, t := range at {
		if !t.Before(deadline) {
			return
		}
		select {
		case <-time.After(time.Until(t)):
		case <-time.After(time.Until(deadline)):
			return
		case <-stop:
			return
		}
		l, stopped := runOne(open, isRegistrant, target, timeout, deadline, stop, nil, 0, 0)
		if stopped {
			if !time.Now().Before(deadline) {
				record(l) // the stop at the deadline: cut by the deadline all the same
			}
			return
		}
		record(l)
	}
}

func runOne(open func() enode.Iterator, isRegistrant func(enode.ID) bool, target int, timeout time.Duration, deadline time.Time, stopCh <-chan struct{}, progress func(Lookup), initial int, interval time.Duration) (Lookup, bool) {
	start := time.Now()
	it := open()
	defer func() { go it.Close() }() // Close can block on a busy search; do not hold the loop
	pos := map[enode.ID]int{}
	found := []Found{}
	var paceStart time.Time
	stop := deadline
	if timeout > 0 && start.Add(timeout).Before(stop) {
		stop = start.Add(timeout)
	}
	done := func(hit bool) Lookup {
		l := Lookup{StartMs: start.UnixMilli(), LatencyMs: time.Since(start).Milliseconds(), FirstMs: -1, Results: len(found), HitTarget: hit, Found: append([]Found{}, found...), Queries: -1, Contacted: -1}
		if c, ok := it.(interface{ Contacts() (int, int) }); ok {
			l.Queries, l.Contacted = c.Contacts()
		}
		if s, ok := it.(interface {
			Stats() discover.TopicSearchStats
		}); ok {
			st := s.Stats()
			l.Search = &st
		}
		if w, ok := it.(interface{ WalkQueries() int }); ok {
			l.WalkQueries = w.WalkQueries()
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
				return done(false), false
			}
		case <-time.After(time.Until(stop)):
			return done(false), false
		case <-stopCh:
			return done(false), true
		}
		id := it.Node().ID()
		src := ""
		if si, ok := it.(enode.SourceIterator); ok {
			src = si.NodeSource()
		}
		if isRegistrant == nil || isRegistrant(id) {
			if k, dup := pos[id]; dup {
				if src != "" && src != found[k].Src && !found[k].Both {
					found[k].Both = true
					if progress != nil {
						progress(done(false))
					}
				}
			} else {
				pos[id] = len(found)
				found = append(found, Found{ID: id.String(), AtMs: time.Since(start).Milliseconds(), Src: src})
				if progress != nil {
					progress(done(false))
				}
				if over := len(found) - initial; interval > 0 && over >= 0 {
					if paceStart.IsZero() {
						paceStart = time.Now()
					}
					select {
					case <-time.After(time.Until(paceStart.Add(time.Duration(over) * interval))):
					case <-time.After(time.Until(stop)):
						return done(false), false
					case <-stopCh:
						return done(false), true
					}
				}
			}
		}
		if target > 0 && len(found) >= target {
			return done(true), false
		}
	}
}
