package main

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

// lazyIter hands the dialer the random walk once the server is up. Close also
// releases a Next still waiting for ready: the dial scheduler blocks there,
// and the server's Stop waits for the dial scheduler.
type lazyIter struct {
	ready     <-chan struct{}
	open      func() enode.Iterator
	seen      func(*enode.Node) // called for every node handed to the dialer
	once      sync.Once
	mu        sync.Mutex
	it        enode.Iterator
	closed    chan struct{}
	closeOnce sync.Once
}

func newLazyIter(ready <-chan struct{}, open func() enode.Iterator, seen func(*enode.Node)) *lazyIter {
	return &lazyIter{ready: ready, open: open, seen: seen, closed: make(chan struct{})}
}

func (l *lazyIter) Next() bool {
	l.once.Do(func() {
		select {
		case <-l.ready:
		case <-l.closed:
			return
		}
		l.mu.Lock()
		defer l.mu.Unlock()
		select {
		case <-l.closed:
		default:
			l.it = l.open()
		}
	})
	l.mu.Lock()
	it := l.it
	l.mu.Unlock()
	if it == nil {
		return false
	}
	ok := it.Next()
	if ok && l.seen != nil {
		l.seen(it.Node())
	}
	return ok
}
func (l *lazyIter) Node() *enode.Node { return l.it.Node() }
func (l *lazyIter) Close() {
	l.closeOnce.Do(func() {
		close(l.closed)
		l.mu.Lock()
		it := l.it
		l.mu.Unlock()
		if it != nil {
			it.Close()
		}
	})
}

// sample is one counter sample for series.json, as cmd/node takes them; the
// stock build has no counters to sample.
type sample struct {
	AtMs int64                  `json:"at_ms"`
	Wire map[string]wireCounter `json:"wire"`
}

// span is one online period of the node.
type span struct {
	StartMs int64 `json:"start_ms"`
	EndMs   int64 `json:"end_ms"`
}

var (
	errUp      = errors.New("already up")
	errDown    = errors.New("already down")
	errStopped = errors.New("stopped")
)

// node is the node process: its identity and what it records over the whole
// run. A churn departure stops the p2p server in place (down); a return builds
// a new one with the same key and address (up), with a fresh table.
type node struct {
	asg        assignment
	key        *ecdsa.PrivateKey
	id         enode.ID
	svc        svcEntry
	bootnodes  []*enode.Node
	bootIDs    map[enode.ID]struct{}
	ghosts     []*enode.Node // pinned in every session's table (fork build)
	ghostIDs   map[enode.ID]struct{}
	lookupOnly bool

	ctl  sync.Mutex // serialises up, down and the final trace
	done bool       // the final trace is written; no more sessions

	mu             sync.Mutex // everything below, and the fields of the sessions
	drops          int
	refills        []int64
	lookupsDone    []lookup
	wireDone       map[string]wireCounter
	samples        []sample
	firstCapableMs int64
	sessions       []span
	connDone       []found  // every closed session's dialer finds, unix ms
	connLast       *session // the last closed session that opened a dialer search
	cur            *session // nil while down
	last           *session // the last closed session
}

// session is one online period: a p2p server and the state a restarted
// process would start empty.
type session struct {
	srv     *p2p.Server
	ready   chan struct{}
	dialIt  *lazyIter
	ctx     context.Context
	cancel  context.CancelFunc
	stop    chan struct{}
	wg      sync.WaitGroup
	started time.Time
	sub     event.Subscription

	lostAt         []time.Time
	firstCapableMs int64

	connStart  time.Time
	connSeen   map[enode.ID]struct{}
	connFound  []found
	connFullMs int64

	lookups []lookup

	// Read from the server by snapshotLive and snapshotCounters.
	out, in int
	wire    map[string]wireCounter

	ghostsInTable, tableSize int
}

func newSession(started time.Time) *session {
	return &session{started: started, firstCapableMs: -1, connSeen: map[enode.ID]struct{}{}, connFound: []found{}, connFullMs: -1}
}

func newNode(asg assignment, key *ecdsa.PrivateKey) *node {
	return &node{asg: asg, key: key, id: enode.PubkeyToIDV4(&key.PublicKey), svc: svcOf(asg.Topics[0]), firstCapableMs: -1, bootIDs: map[enode.ID]struct{}{}, ghostIDs: map[enode.ID]struct{}{},
		lookupOnly: asg.Search.Model == "continuous" || asg.Search.Model == "scheduled"}
}

func (n *node) isProvider(nd *enode.Node) bool {
	var s svcEntry
	return nd.Load(&s) == nil && s == n.svc
}

// waitCtx sleeps until atMs and reports whether ctx is still live.
func waitCtx(ctx context.Context, atMs int64) bool {
	if d := time.Until(time.UnixMilli(atMs)); d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
		}
	}
	return ctx.Err() == nil
}

// up starts a new p2p server. Its search time past, it searches at once.
func (n *node) up() error {
	n.ctl.Lock()
	defer n.ctl.Unlock()
	if n.done {
		return errStopped
	}
	n.mu.Lock()
	running := n.cur != nil
	n.mu.Unlock()
	if running {
		return errUp
	}
	s := newSession(time.Now())
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.stop, s.ready = make(chan struct{}), make(chan struct{})
	proto := p2p.Protocol{
		Name: "topdisc-test", Version: 1, Length: 1, // same capability as cmd/node so RLPx peers stay
		Run: func(p *p2p.Peer, rw p2p.MsgReadWriter) error {
			for {
				if _, err := rw.ReadMsg(); err != nil {
					return err
				}
			}
		},
	}
	// Lookup-only models never dial; the connection-driven model dials the
	// providers its random walk finds and records them and when slots filled.
	s.dialIt = newLazyIter(s.ready, func() enode.Iterator {
		n.mu.Lock()
		s.connStart = time.Now()
		n.mu.Unlock()
		// Dial providers only, as a client filtering candidates by its
		// ENR entry does; the walk itself is unchanged.
		return enode.Filter(s.srv.DiscoveryV5().RandomNodes(), n.isProvider)
	}, func(nd *enode.Node) {
		n.mu.Lock()
		defer n.mu.Unlock()
		if _, ok := s.connSeen[nd.ID()]; !ok {
			s.connSeen[nd.ID()] = struct{}{}
			s.connFound = append(s.connFound, found{ID: nd.ID().String(), AtMs: time.Since(s.connStart).Milliseconds()})
		}
	})
	proto.DialCandidates = s.dialIt
	if n.lookupOnly {
		// The node only consumes its own walks: no dialer, so no second walk.
		proto.DialCandidates = enode.IterNodes(nil)
	}
	a := n.asg
	s.srv = &p2p.Server{Config: p2p.Config{
		PrivateKey: n.key, Name: "topdisc-node-legacy", MaxPeers: a.MaxPeers, DialRatio: a.DialRatio, NoDial: n.lookupOnly,
		ListenAddr: fmt.Sprintf("%s:%d", a.IP, a.Port), DiscoveryV4: false, DiscoveryV5: true,
		BootstrapNodesV5: n.bootnodes, Protocols: []p2p.Protocol{proto}, Logger: log.Root(),
	}}
	pinGhosts(&s.srv.Config, n.ghosts)
	if err := s.srv.Start(); err != nil {
		s.cancel()
		return err
	}
	s.srv.LocalNode().SetFallbackIP(net.ParseIP(a.IP))
	if a.ExtIP != "" {
		s.srv.LocalNode().SetStaticIP(net.ParseIP(a.ExtIP))
	}
	s.srv.LocalNode().Set(n.svc)
	events := make(chan *p2p.PeerEvent, 64)
	s.sub = s.srv.SubscribeEvents(events)
	n.mu.Lock()
	n.cur = s
	n.mu.Unlock()
	s.wg.Add(3)
	go n.watchPeers(s, events)
	go n.watchCapable(s)
	go n.search(s)
	if a.SampleMs > 0 && forkBuild {
		s.wg.Add(1)
		go n.sampler(s)
	}
	log.Info("legacy node up", "enode", s.srv.Self().URLv4(), "session", len(n.sessions)+1)
	return nil
}

// down stops the server; the node keeps what the session recorded.
func (n *node) down() error {
	n.ctl.Lock()
	defer n.ctl.Unlock()
	return n.downLocked()
}

func (n *node) downLocked() error {
	n.mu.Lock()
	s := n.cur
	n.mu.Unlock()
	if s == nil {
		return errDown
	}
	s.cancel()
	close(s.stop)
	s.dialIt.Close()
	s.sub.Unsubscribe()
	n.snapshotLive(s)
	s.srv.Stop()
	s.wg.Wait()
	n.snapshotCounters(s)
	n.fold(s, time.Now())
	log.Info("legacy node down", "sessions", len(n.sessions))
	return nil
}

// fold closes the current session into the node's totals.
func (n *node) fold(s *session, end time.Time) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.lookupsDone = append(n.lookupsDone, s.lookups...)
	n.wireDone = addWire(n.wireDone, s.wire)
	n.firstCapableMs = minCapable(n.firstCapableMs, s.firstCapableMs)
	if !s.connStart.IsZero() {
		n.connDone = append(n.connDone, absFound(s)...)
		n.connLast = s
	}
	n.sessions = append(n.sessions, span{s.started.UnixMilli(), end.UnixMilli()})
	n.last, n.cur = s, nil
}

// finish writes the trace and takes the node down for good.
func (n *node) finish() {
	n.ctl.Lock()
	defer n.ctl.Unlock()
	if n.done {
		return
	}
	n.done = true
	n.mu.Lock()
	s := n.cur
	n.mu.Unlock()
	if s != nil {
		n.snapshotLive(s)
		n.snapshotCounters(s)
	}
	n.writeTrace()
	n.downLocked()
}

// search runs the session's walks. A legacy node has nothing to register:
// its ENR carries the service.
func (n *node) search(s *session) {
	defer s.wg.Done()
	a := n.asg
	if !waitCtx(s.ctx, a.Phases.SearchAt) {
		return
	}
	close(s.ready)
	if !n.lookupOnly {
		return
	}
	deadline := time.UnixMilli(a.Phases.StopAt)
	if a.Phases.StopAt == 0 {
		deadline = time.Now().Add(24 * time.Hour)
	}
	timeout := time.Duration(a.Search.RequestTimeout) * time.Millisecond
	disc := s.srv.DiscoveryV5()
	cost := findnodesSent(s.srv)
	if a.Search.Model == "scheduled" {
		for _, ms := range a.Search.LookupAtMs {
			t := time.UnixMilli(ms)
			if !t.Before(deadline) {
				return
			}
			select {
			case <-time.After(time.Until(t)):
			case <-s.stop:
				return
			}
			l := randomWalk(disc, cost, n.isProvider, a.Search.TargetCount, timeout, deadline, s.stop)
			select {
			case <-s.stop:
				return // cut short by the departure
			default:
			}
			n.mu.Lock()
			s.lookups = append(s.lookups, l)
			n.mu.Unlock()
		}
		return
	}
	// continuous: one walk for the whole phase, reported as a single lookup.
	set := func(l lookup) { n.mu.Lock(); s.lookups = []lookup{l}; n.mu.Unlock() }
	set(walkIterator(disc, cost, n.isProvider, deadline, s.stop, set, a.Search.InitialResults, time.Duration(a.Search.ResultIntervalMs)*time.Millisecond))
}

func (n *node) sampler(s *session) {
	defer s.wg.Done()
	t := time.NewTicker(time.Duration(n.asg.SampleMs) * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		n.addSample(sample{AtMs: time.Now().UnixMilli(), Wire: wireStats(s.srv)})
	}
}

// addSample records a sample of the live server, its counters carried on from
// the earlier sessions' totals so the series stays cumulative.
func (n *node) addSample(smp sample) {
	n.mu.Lock()
	defer n.mu.Unlock()
	smp.Wire = addWire(n.wireDone, smp.Wire)
	n.samples = append(n.samples, smp)
}

func (n *node) watchPeers(s *session, events chan *p2p.PeerEvent) {
	defer s.wg.Done()
	for {
		var ev *p2p.PeerEvent
		select {
		case <-s.ctx.Done():
			return
		case ev = <-events:
		}
		switch ev.Type {
		case p2p.PeerEventTypeDrop:
			n.mu.Lock()
			n.drops++
			s.lostAt = append(s.lostAt, time.Now())
			n.mu.Unlock()
		case p2p.PeerEventTypeAdd:
			outbound := 0
			for _, p := range s.srv.Peers() {
				if !p.Inbound() {
					outbound++
				}
			}
			n.mu.Lock()
			if len(s.lostAt) > 0 {
				n.refills = append(n.refills, time.Since(s.lostAt[0]).Milliseconds())
				s.lostAt = s.lostAt[1:]
			}
			if !n.lookupOnly && s.connFullMs < 0 && !s.connStart.IsZero() && outbound >= s.srv.MaxDialedConns() {
				s.connFullMs = time.Since(s.connStart).Milliseconds()
			}
			n.mu.Unlock()
		}
	}
}

// watchCapable records when the session's table first holds a
// TopDisc-capable record that is neither a ghost nor a bootnode.
func (n *node) watchCapable(s *session) {
	defer s.wg.Done()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		if n.hasCapable(s.srv.DiscoveryV5().AllNodes()) {
			n.mu.Lock()
			s.firstCapableMs = time.Since(s.started).Milliseconds()
			n.mu.Unlock()
			return
		}
	}
}

// hasCapable skips every ghost record, also those other nodes pinned and
// passed on: ghosts carry "ng".
func (n *node) hasCapable(nodes []*enode.Node) bool {
	for _, nd := range nodes {
		_, boot := n.bootIDs[nd.ID()]
		var c capableEntry
		var g ghostEntry
		if !boot && nd.Load(&g) != nil && nd.Load(&c) == nil {
			return true
		}
	}
	return false
}

// snapshotLive reads what only a running server can tell: its peers and table.
func (n *node) snapshotLive(s *session) {
	out, in := 0, 0
	for _, p := range s.srv.Peers() {
		if p.Inbound() {
			in++
		} else {
			out++
		}
	}
	ghosts, table := ghostsInTable(s.srv, n.ghostIDs)
	n.mu.Lock()
	s.out, s.in = out, in
	s.ghostsInTable, s.tableSize = ghosts, table
	n.mu.Unlock()
}

// snapshotCounters reads the server's own counters; each new server starts
// them from zero.
func (n *node) snapshotCounters(s *session) {
	w := wireStats(s.srv)
	n.mu.Lock()
	s.wire = w
	n.mu.Unlock()
}

// trace is the node's record of the whole run, one map with every session:
// counters summed, event lists joined, the peer state of the last session,
// and sessions[] listing each online period. The per-field rules are
// host.LoadTrace's for the session files older runs left.
func (n *node) trace() map[string]any {
	n.mu.Lock()
	defer n.mu.Unlock()
	wire := addWire(n.wireDone, nil)
	lookups := append([]lookup{}, n.lookupsDone...)
	capable := n.firstCapableMs
	fd := append([]found{}, n.connDone...)
	connSrc := n.connLast
	sessions := append([]span{}, n.sessions...)
	s := n.last
	if c := n.cur; c != nil {
		s = c
		wire = addWire(wire, c.wire)
		lookups = append(lookups, c.lookups...)
		capable = minCapable(capable, c.firstCapableMs)
		if !c.connStart.IsZero() {
			fd, connSrc = append(fd, absFound(c)...), c
		}
		sessions = append(sessions, span{c.started.UnixMilli(), time.Now().UnixMilli()})
	}
	var conn map[string]any
	if !n.lookupOnly && connSrc != nil {
		start := connSrc.connStart.UnixMilli()
		rel := make([]found, len(fd))
		for i, f := range fd {
			rel[i] = found{ID: f.ID, AtMs: f.AtMs - start}
		}
		conn = map[string]any{"start_ms": start, "found": rel, "slots_filled_ms": connSrc.connFullMs}
	}
	out, in, ghosts, table := 0, 0, 0, 0
	if s != nil {
		out, in, ghosts, table = s.out, s.in, s.ghostsInTable, s.tableSize
	}
	a := n.asg
	return map[string]any{
		"conn": conn,
		"idx":  a.Idx, "id": n.id.String(), "legacy": true, "outbound": out, "inbound": in,
		"peer_drops": n.drops, "refill_ms": append([]int64{}, n.refills...), "lookups": lookups, "first_capable_ms": capable,
		"ads_held": 0, "wire": wire, "samples": append([]sample{}, n.samples...),
		// The phase clock, as cmd/node records it. Leaving it out left the
		// aggregator subtracting a zero search start, so every discovery
		// time it derived for a legacy searcher came out as an absolute
		// epoch value instead of ms into the search.
		"register_at_ms": a.Phases.RegisterAt, "search_at_ms": a.Phases.SearchAt,
		"sessions": sessions, "ghosts_assigned": len(n.ghosts), "ghosts_in_table": ghosts, "table_size": table,
		"fork": forkVersion(),
	}
}

func (n *node) writeTrace() {
	if n.asg.TraceFile == "" {
		return
	}
	b, err := json.MarshalIndent(n.trace(), "", " ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "trace:", err)
		return
	}
	if err := os.WriteFile(n.asg.TraceFile, b, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "trace:", err)
	}
}

// status is what /status reports; zeros while down.
func (n *node) status() map[string]any {
	n.mu.Lock()
	s := n.cur
	lookups, refills, sessions := len(n.lookupsDone), len(n.refills), len(n.sessions)
	if s != nil {
		lookups += len(s.lookups)
	}
	n.mu.Unlock()
	m := map[string]any{"up": s != nil, "sessions": sessions, "goroutines": runtime.NumGoroutine(), "enode": "", "peers": 0, "outbound": 0, "inbound": 0,
		"table": 0, "lookups": lookups, "refills": refills, "fork": forkVersion()}
	if s == nil {
		return m
	}
	out, in := 0, 0
	for _, p := range s.srv.Peers() {
		if p.Inbound() {
			in++
		} else {
			out++
		}
	}
	m["sessions"] = sessions + 1
	m["enode"], m["peers"], m["outbound"], m["inbound"] = s.srv.Self().URLv4(), out+in, out, in
	m["table"] = len(s.srv.DiscoveryV5().AllNodes())
	return m
}

func absFound(s *session) []found {
	start := s.connStart.UnixMilli()
	out := make([]found, len(s.connFound))
	for i, f := range s.connFound {
		out[i] = found{ID: f.ID, AtMs: start + f.AtMs}
	}
	return out
}

func minCapable(a, b int64) int64 {
	if a < 0 || (b >= 0 && b < a) {
		return b
	}
	return a
}

func addWire(a, b map[string]wireCounter) map[string]wireCounter {
	out := make(map[string]wireCounter, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = addCounter(out[k], v)
	}
	return out
}
