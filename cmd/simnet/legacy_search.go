package main

import (
	crand "crypto/rand"
	"time"

	"github.com/ethereum/go-ethereum/p2p/enode"
)

// runLegacyWalker is the search of a node without TopDisc, as in
// legacy/cmd/node-legacy on the real backends: check the nodes it already
// knows, then run random-target Kademlia lookups until the deadline, keeping
// every node that provides the topic. A provider is any node of the topic,
// TopDisc or not; on the real backends this is the node's "svc" record entry.
// TargetCount, if set, ends the walk early.
func runLegacyWalker(n nodeRec, topicIdx int, deadlineAt time.Time, pacing searchPacing,
	providers map[enode.ID]struct{}, stats *liveStats) searchResult {
	self := n.ln.ID()
	target := len(providers)
	if _, ok := providers[self]; ok {
		target--
	}
	res := searchResult{NodeIdx: n.idx, NodeID: self.String(), Topic: topicIdx, Target: target, Legacy: true, FLookup: -1}
	start := time.Now()
	if !searchEpoch.IsZero() {
		res.SearchStartMs = start.Sub(searchEpoch).Milliseconds()
	}
	disc := liveDisc(n)
	connected := make(map[enode.ID]struct{})
	for _, cn := range disc.AllNodes() {
		connected[cn.ID()] = struct{}{}
	}
	res.ConnectedAtStart = len(connected)
	seen := make(map[enode.ID]struct{})
	add := func(nodes []*enode.Node) {
		for _, nd := range nodes {
			id := nd.ID()
			if id == self {
				continue
			}
			if _, ok := providers[id]; !ok {
				continue
			}
			res.FoundRegistrant++
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			at := time.Since(start)
			if len(seen) == 1 {
				res.TimeToFirst = at
			}
			res.UniqueFoundAtMs = append(res.UniqueFoundAtMs, at.Milliseconds())
			res.UniqueFoundIDs = append(res.UniqueFoundIDs, id.String())
			res.FoundRegistrantIDs = append(res.FoundRegistrantIDs, id.String())
			if _, ok := connected[id]; ok {
				res.AlreadyConnectedReg++
			} else {
				res.NewRegistrant++
				res.NewFoundAtMs = append(res.NewFoundAtMs, at.Milliseconds())
			}
			if stats != nil {
				stats.recordUniqueFind(topicIdx, id)
			}
		}
	}
	done := func() bool { return pacing.TargetCount > 0 && len(seen) >= pacing.TargetCount }

	add(disc.AllNodes())
	rounds := 0
	for !done() && time.Now().Before(deadlineAt) {
		select {
		case <-stopSearches:
			deadlineAt = time.Now()
			continue
		default:
		}
		d := currentDisc(n.idx)
		if d == nil { // away under churn: no walk until it returns
			time.Sleep(time.Second)
			continue
		}
		var tgt enode.ID
		crand.Read(tgt[:])
		out := make(chan []*enode.Node, 1)
		go func() { out <- d.Lookup(tgt) }()
		select {
		case nodes := <-out:
			rounds++
			add(nodes)
		case <-time.After(time.Until(deadlineAt)):
		}
	}
	res.UniqueRegistrant = len(seen)
	res.Found = res.FoundRegistrant
	res.TimeToCompletion = time.Since(start)
	res.HitTimeoutBefore = !done()
	res.Lookups = 1
	if done() {
		res.LookupsHitTarget = 1
	}
	res.LookupLatencyMs = []int64{res.TimeToCompletion.Milliseconds()}
	res.LookupResults = []int{len(seen)}
	res.LookupQueries = []int{rounds}
	res.SearchQueries = rounds
	return res
}
