package main

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// groupNames are the words a check uses for the kindOld and kindNew groups.
type groupNames struct{ old, new string }

var (
	oldNew  = groupNames{"old", "new"}
	splitAB = groupNames{"A", "B"}
)

func upper(g string) string { return strings.ToUpper(g) }

func (g groupNames) of(kind string) string {
	switch kind {
	case kindOld:
		return g.old
	case kindNew:
		return g.new
	}
	return kind
}

// splitInventory relabels a single-kind inventory: even positions (participant 0 among
// them) play the OLD role as group A, odd positions the NEW role as group B.
func splitInventory(nodes []node) *inventory {
	split := make([]node, len(nodes))
	for i, n := range nodes {
		n.Kind = kindOld
		if i%2 == 1 {
			n.Kind = kindNew
		}
		split[i] = n
	}
	return newInventory(split)
}

func (rd *runData) names() groupNames {
	if rd.split != nil {
		return splitAB
	}
	return oldNew
}

// groupNodes is population with the A/B labels in a split run.
func (rd *runData) groupNodes(kind string, noBoot bool) ([]node, bool) {
	if rd.split == nil {
		return rd.population(kind, noBoot)
	}
	var out []node
	for i, n := range rd.split.Nodes {
		if n.Kind == kind && !(noBoot && i == 0) {
			out = append(out, n)
		}
	}
	return out, true
}

func (rd *runData) splitKind(name string) string {
	for _, n := range rd.split.Nodes {
		if n.Name == name {
			return n.Kind
		}
	}
	return kindUnknown
}

// removed returns a node's removals by group, or why they are not available.
func (rd *runData) removed(l logsNode) (map[string]int, string) {
	if rd.split == nil {
		return l.Removed, ""
	}
	if l.RemovedByPeer == nil {
		return nil, "no removed_by_peer in logs.json (re-run d79check logs)"
	}
	out := map[string]int{kindUnknown: l.Removed[kindUnknown]}
	for name, k := range l.RemovedByPeer {
		out[rd.splitKind(name)] += k
	}
	return out, ""
}

// splitPeers re-judges E3(a) on A/B from the saved peer IDs. Inventory nodes absent from
// peers.json, listed twice or unknown to the inventory make it incomplete.
func (rd *runData) splitPeers(p *peersResult) check {
	if len(p.Nodes) == 0 {
		return check{ID: "E3a", Desc: "every A node has a B peer and vice versa", Status: statusMissing, Detail: "no nodes in peers.json"}
	}
	var nodes []peersNode
	seen := make(map[string]bool)
	for _, n := range p.Nodes {
		sn := peersNode{Name: n.Name, Kind: rd.splitKind(n.Name), Error: n.Error, Total: n.Total,
			ByKind: make(map[string]dirCount), NameMismatch: n.NameMismatch, Peers: n.Peers}
		switch {
		case seen[n.Name]:
			nodes = append(nodes, peersNode{Name: n.Name, Kind: sn.Kind, Error: "listed twice in peers.json"})
			continue
		case sn.Kind == kindUnknown:
			sn.Error = "not in the inventory"
		}
		seen[n.Name] = true
		for _, pi := range n.Peers {
			k := rd.split.kindOf(pi.ID)
			dc := sn.ByKind[k]
			if pi.Inbound {
				dc.In++
			} else {
				dc.Out++
			}
			sn.ByKind[k] = dc
		}
		nodes = append(nodes, sn)
	}
	for _, n := range rd.split.Nodes {
		if !seen[n.Name] {
			nodes = append(nodes, peersNode{Name: n.Name, Kind: n.Kind, Error: "not in peers.json"})
		}
	}
	_, _, c := evalPeers(rd.split, nodes, splitAB)
	return c
}

// splitTables re-judges E1(a,b) on A/B from the saved table entries of a tables.json.
func (rd *runData) splitTables(path, rel string) check {
	id := "E1 " + filepath.Join(rel, "tables.json")
	var res tablesResult
	if err := readJSON(path, &res); err != nil {
		return check{ID: id, Status: statusMissing, Detail: err.Error()}
	}
	ct := crawlTables{TEnd: res.Time.UnixMilli(), Partial: res.Partial, Nodes: make(map[string]crawlRead)}
	for _, nt := range res.Nodes {
		if !nt.Read {
			continue
		}
		cr := crawlRead{T: nt.T, Buckets: make(map[string][]crawlEntry)}
		for _, e := range nt.Table {
			b := strconv.Itoa(e.Bucket)
			cr.Buckets[b] = append(cr.Buckets[b], crawlEntry{e.ID, e.Alive})
		}
		ct.Nodes[nt.ID] = cr
	}
	c := tablesCheck(classifyTables(rd.split, &ct), rel, splitAB)
	c.ID = id
	return c
}

type splitGroups struct{ A, B []string }

func (rd *runData) splitGroups() *splitGroups {
	if rd.split == nil {
		return nil
	}
	g := new(splitGroups)
	for _, n := range rd.split.Nodes {
		if n.Kind == kindOld {
			g.A = append(g.A, n.Name)
		} else {
			g.B = append(g.B, n.Name)
		}
	}
	return g
}

func (g *splitGroups) line() string {
	list := func(names []string) string {
		if len(names) <= 2 {
			return strings.Join(names, ", ")
		}
		return fmt.Sprintf("%s, %s, … %d nodes", names[0], names[1], len(names))
	}
	return fmt.Sprintf("Single-kind run: E1, E2c and E3a judge A (%s, participant 0 included) against B (%s); other checks use the real kind.",
		list(g.A), list(g.B))
}
