//go:build !fork

package main

import (
	"fmt"
	"os"

	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

// Upstream geth cannot pin table entries, so the stock build runs without ghosts.
func parseGhosts(recs []string) ([]*enode.Node, error) {
	if len(recs) > 0 {
		fmt.Fprintf(os.Stderr, "stock build ignores the assignment's %d ghosts\n", len(recs))
	}
	return nil, nil
}

func pinGhosts(*p2p.Config, []*enode.Node) {}

func ghostsInTable(srv *p2p.Server, _ map[enode.ID]struct{}) (in, total int) {
	return 0, len(srv.DiscoveryV5().AllNodes())
}
