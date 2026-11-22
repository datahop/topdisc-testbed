package main

import (
	"context"
	"flag"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

type peerInfo struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	NameKind string `json:"name_kind"`
	Inbound  bool   `json:"inbound"`
}

type dirCount struct {
	In  int `json:"in"`
	Out int `json:"out"`
}

type peersNode struct {
	Name         string              `json:"name"`
	Kind         string              `json:"kind"`
	Error        string              `json:"error,omitempty"`
	Total        int                 `json:"total"`
	ByKind       map[string]dirCount `json:"by_kind"`
	NameMismatch []string            `json:"name_mismatch,omitempty"`
	Peers        []peerInfo          `json:"peers"`
}

type peersResult struct {
	Time     time.Time                 `json:"time"`
	Nodes    []peersNode               `json:"nodes"`
	Matrix   map[string]map[string]int `json:"matrix"`
	Check    check                     `json:"check"`
	Failures []string                  `json:"failures,omitempty"`
}

// nameKind classifies an admin_peers name; NEW nodes run with --identity=topdisc.
func nameKind(name string) string {
	switch {
	case strings.HasPrefix(name, "Geth/topdisc/"):
		return kindNew
	case strings.HasPrefix(name, "Geth/"):
		return kindOld
	}
	return kindUnknown
}

func init() {
	register("peers", "admin_peers on every EL: old/new peer matrix, E3(a)", runPeers)
}

func runPeers(args []string) error {
	fs := flag.NewFlagSet("peers", flag.ExitOnError)
	rf := addRunFlags(fs)
	fs.Parse(args)
	if err := rf.check(); err != nil {
		return err
	}
	inv, err := rf.inventory()
	if err != nil {
		return err
	}
	res := collectPeers(context.Background(), inv)
	for _, n := range res.Nodes {
		fmt.Printf("%-24s %-3s peers=%-3d old in/out=%d/%d new in/out=%d/%d unknown=%d %s\n", n.Name, n.Kind, n.Total,
			n.ByKind[kindOld].In, n.ByKind[kindOld].Out, n.ByKind[kindNew].In, n.ByKind[kindNew].Out,
			n.ByKind[kindUnknown].In+n.ByKind[kindUnknown].Out, n.Error)
	}
	fmt.Printf("E3(a) %s %s\n", res.Check.Status, res.Check.Detail)
	return writeJSON(filepath.Join(rf.out, "peers.json"), res)
}

func collectPeers(ctx context.Context, inv *inventory) peersResult {
	res := peersResult{Time: time.Now().UTC(), Nodes: make([]peersNode, len(inv.Nodes))}
	forEach(len(inv.Nodes), func(i int) {
		n := inv.Nodes[i]
		pn := peersNode{Name: n.Name, Kind: n.Kind, ByKind: make(map[string]dirCount)}
		var peers []struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Network struct {
				Inbound bool `json:"inbound"`
			} `json:"network"`
		}
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if err := rpcCall(cctx, elURL(n), "admin_peers", &peers); err != nil {
			pn.Error = err.Error()
		}
		for _, p := range peers {
			pi := peerInfo{ID: p.ID, Name: p.Name, Kind: inv.kindOf(p.ID), NameKind: nameKind(p.Name), Inbound: p.Network.Inbound}
			if pi.Kind != kindUnknown && pi.NameKind != pi.Kind {
				pn.NameMismatch = append(pn.NameMismatch, fmt.Sprintf("%s: %s but name %q", short(p.ID), pi.Kind, p.Name))
			}
			dc := pn.ByKind[pi.Kind]
			if pi.Inbound {
				dc.In++
			} else {
				dc.Out++
			}
			pn.ByKind[pi.Kind] = dc
			pn.Peers = append(pn.Peers, pi)
		}
		pn.Total = len(peers)
		res.Nodes[i] = pn
	})
	res.Matrix, res.Failures, res.Check = evalPeers(inv, res.Nodes, oldNew)
	return res
}

// evalPeers checks E3(a): every OLD node has a NEW peer and every NEW node an OLD peer,
// worded with g. A node that did not answer makes the check incomplete.
func evalPeers(inv *inventory, nodes []peersNode, g groupNames) (map[string]map[string]int, []string, check) {
	matrix := map[string]map[string]int{kindOld: {}, kindNew: {}}
	var fails, missing []string
	for _, n := range nodes {
		if matrix[n.Kind] == nil {
			matrix[n.Kind] = map[string]int{}
		}
		for k, dc := range n.ByKind {
			matrix[n.Kind][k] += dc.In + dc.Out
		}
		other := kindNew
		if n.Kind == kindNew {
			other = kindOld
		}
		switch {
		case n.Error != "":
			missing = append(missing, n.Name+": "+n.Error)
		case inv.count(other) > 0 && n.ByKind[other].In+n.ByKind[other].Out == 0:
			fails = append(fails, fmt.Sprintf("%s (%s) has no %s peer", n.Name, g.of(n.Kind), g.of(other)))
		}
		for _, m := range n.NameMismatch {
			fails = append(fails, n.Name+": name mismatch "+m)
		}
	}
	c := check{ID: "E3a", Desc: fmt.Sprintf("every %s node has a %s peer and vice versa", upper(g.old), upper(g.new))}
	judge(&c, fails, missing, fmt.Sprintf("%[1]s→%[1]s %[3]d %[1]s→%[2]s %[4]d %[2]s→%[1]s %[5]d %[2]s→%[2]s %[6]d", g.old, g.new, matrix[kindOld][kindOld],
		matrix[kindOld][kindNew], matrix[kindNew][kindOld], matrix[kindNew][kindNew]))
	if c.Status == statusPass && (inv.count(kindOld) == 0 || inv.count(kindNew) == 0) {
		c.Status, c.Detail = statusNA, "single-kind run"
	}
	return matrix, append(fails, missing...), c
}
