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

func enableWireStats() { discover.EnableWireStats() }

func wireStats(srv *p2p.Server) any {
	if ws := srv.DiscoveryV5().WireStats(); len(ws) > 0 {
		return ws
	}
	return map[string]any{}
}

func findnodesSent(srv *p2p.Server) func() int64 {
	return func() int64 { return srv.DiscoveryV5().WireStats()["FINDNODE/v5"].TxMsgs }
}
