//go:build fork

package main

import (
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/discover"
)

// forkBuild is true when built against the TopDisc fork (go.fork.mod): the same
// discv5 code and per-message counters as cmd/node. The node never registers or
// searches a topic, so it sends only plain discv5 traffic, but its ENR carries
// the fork's "ng" entry. Use it only where every node is plain.
const forkBuild = true

type wireCounter = discover.WireCounter

func addCounter(a, b wireCounter) wireCounter {
	return wireCounter{TxMsgs: a.TxMsgs + b.TxMsgs, TxBytes: a.TxBytes + b.TxBytes, RxMsgs: a.RxMsgs + b.RxMsgs, RxBytes: a.RxBytes + b.RxBytes,
		Timeouts: a.Timeouts + b.Timeouts, GhostTimeouts: a.GhostTimeouts + b.GhostTimeouts, OwnGhostTimeouts: a.OwnGhostTimeouts + b.OwnGhostTimeouts}
}

func enableWireStats() { discover.EnableWireStats() }

// wireStats reads the server's counters, which start from zero on every new
// server.
func wireStats(srv *p2p.Server) map[string]wireCounter { return srv.DiscoveryV5().WireStats() }

func findnodesSent(srv *p2p.Server) func() int64 {
	return func() int64 { return srv.DiscoveryV5().WireStats()["FINDNODE/v5"].TxMsgs }
}
