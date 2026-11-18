package main

import (
	"time"

	"github.com/ethereum/go-ethereum/p2p/discover"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

// mixedSearch is geth's --discovery.topic source: the topic search and the
// random walk, both filtered on the service, mixed fairly. The topic search's
// progress stays readable through it.
type mixedSearch struct {
	*enode.FairMix
	topic     enode.Iterator
	findnodes func() int64
	sent0     int64
}

func newMixedSearch(topic, walk enode.Iterator, isProvider func(*enode.Node) bool, findnodes func() int64) *mixedSearch {
	m := &mixedSearch{FairMix: enode.NewFairMix(100 * time.Millisecond), topic: topic, findnodes: findnodes, sent0: findnodes()}
	m.AddSource(enode.WithSourceName("topic", enode.Filter(topic, isProvider)))
	m.AddSource(enode.WithSourceName("walk", enode.Filter(walk, isProvider)))
	return m
}

func (m *mixedSearch) Stats() discover.TopicSearchStats {
	if s, ok := m.topic.(interface {
		Stats() discover.TopicSearchStats
	}); ok {
		return s.Stats()
	}
	return discover.TopicSearchStats{}
}

func (m *mixedSearch) Contacts() (int, int) {
	if c, ok := m.topic.(interface{ Contacts() (int, int) }); ok {
		return c.Contacts()
	}
	return -1, -1
}

func (m *mixedSearch) WalkQueries() int { return int(m.findnodes() - m.sent0) }

// openSearch opens the node's search: the topic search alone, or mixed with
// the random walk under search.topdisc_walk.
func (n *node) openSearch(d *discover.UDPv5, opid uint64) enode.Iterator {
	topic := d.TopicSearch(n.topic, opid)
	if !n.asg.Search.TopdiscWalk {
		return topic
	}
	return newMixedSearch(topic, d.RandomNodes(), n.isProvider, func() int64 { return d.WireStats()["FINDNODE/v5"].TxMsgs })
}

func (n *node) isProvider(nd *enode.Node) bool {
	var s svcEntry
	return nd.Load(&s) == nil && s == n.svc
}
