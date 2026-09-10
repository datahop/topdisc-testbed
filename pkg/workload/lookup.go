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
	StartMs   int64 `json:"start_ms"`
	LatencyMs int64 `json:"latency_ms"`
	Results   int   `json:"results"`
	HitTarget bool  `json:"hit_target"`
}

// Continuous runs lookups back to back until the deadline: each ends at target
// distinct registrants or after timeout, then the next starts after delay.
// isRegistrant filters what counts as a result; nil counts every node.
func Continuous(open func() enode.Iterator, isRegistrant func(enode.ID) bool, target int, delay, timeout time.Duration, deadline time.Time, record func(Lookup)) {
	for time.Now().Before(deadline) {
		l := runOne(open, isRegistrant, target, timeout, deadline)
		record(l)
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-time.After(time.Until(deadline)):
				return
			}
		}
	}
}

func runOne(open func() enode.Iterator, isRegistrant func(enode.ID) bool, target int, timeout time.Duration, deadline time.Time) Lookup {
	start := time.Now()
	it := open()
	defer func() { go it.Close() }() // Close can block on a busy search; do not hold the loop
	seen := map[enode.ID]struct{}{}
	stop := deadline
	if timeout > 0 && start.Add(timeout).Before(stop) {
		stop = start.Add(timeout)
	}
	next := make(chan bool, 1)
	for {
		go func() { next <- it.Next() }()
		select {
		case ok := <-next:
			if !ok {
				return Lookup{StartMs: start.UnixMilli(), LatencyMs: time.Since(start).Milliseconds(), Results: len(seen)}
			}
		case <-time.After(time.Until(stop)):
			return Lookup{StartMs: start.UnixMilli(), LatencyMs: time.Since(start).Milliseconds(), Results: len(seen)}
		}
		id := it.Node().ID()
		if isRegistrant == nil || isRegistrant(id) {
			seen[id] = struct{}{}
		}
		if target > 0 && len(seen) >= target {
			return Lookup{StartMs: start.UnixMilli(), LatencyMs: time.Since(start).Milliseconds(), Results: len(seen), HitTarget: true}
		}
	}
}
