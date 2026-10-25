//go:build fork

package main

import (
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

func parseGhosts(recs []string) ([]*enode.Node, error) {
	var out []*enode.Node
	for _, s := range recs {
		n, err := enode.Parse(enode.ValidSchemes, s)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

func pinGhosts(cfg *p2p.Config, ghosts []*enode.Node) { cfg.DiscoveryV5Pinned = ghosts }

// ghostsInTable counts the table's entries and the ghosts among them.
func ghostsInTable(srv *p2p.Server, ids map[enode.ID]struct{}) (in, total int) {
	all := srv.DiscoveryV5().AllNodes()
	for _, n := range all {
		if _, ok := ids[n.ID()]; ok {
			in++
		}
	}
	return in, len(all)
}
