//go:build fork

package main

import (
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/discover"
)

// forkBuild is true when built against the fork's discv5 instrumentation branch
// (go.fork.mod): upstream master's discv5 plus the per-message counters and
// pinned entries cmd/node also has; the ENR carries no "ng". In a mixed run it
// is the plain half next to TopDisc nodes.
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
