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

	"github.com/datahop/topdisc-testbed/pkg/assign"
	"github.com/datahop/topdisc-testbed/pkg/workload"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/discover"
	"github.com/ethereum/go-ethereum/p2p/discover/topicindex"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

// lazyIter hands the dialer a TopicSearch iterator once the server is up.
// DialCandidates must exist before Start, but the discv5 instance only after.
// Close also releases a Next still waiting for ready: the dial scheduler
// blocks there, and the server's Stop waits for the dial scheduler.
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

type bucketCounts struct {
	Registered int `json:"registered"`
	Waiting    int `json:"waiting"`
	Standby    int `json:"standby"`
}

type sample struct {
	AtMs       int64                           `json:"at_ms"`
	Wire       map[string]discover.WireCounter `json:"wire"`
	CacheHeld  int                             `json:"cache_held"`
	CacheCap   int                             `json:"cache_cap"`
	ByTopic    map[string]int                  `json:"cache_by_topic"`
	RegBuckets []bucketCounts                  `json:"reg_buckets,omitempty"` // far to close
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
// a new one with the same key and address (up), with fresh tables and no ads.
type node struct {
	asg        assign.Assignment
	key        *ecdsa.PrivateKey
	id         enode.ID
	ip         string
	port       int
	maxPeers   int
	dialRatio  int
	topicName  string
	topic      topicindex.TopicID
	bootnodes  []*enode.Node
	lookupOnly bool

	ctl  sync.Mutex // serialises up, down and the final trace
	done bool       // the final trace is written; no more sessions

	mu             sync.Mutex // everything below, and the fields of the sessions
	drops          int
	refills        []int64
	lookupsDone    []workload.Lookup
	opsDone        []map[string]any
	wireDone       map[string]discover.WireCounter
	topicLoadDone  map[string]discover.TopicLoad
	samples        []sample
	firstCapableMs int64
	sessions       []span
	connDone       []workload.Found // every closed session's dialer finds, unix ms
	connLast       *session         // the last closed session that opened a dialer search
	cur            *session         // nil while down
	last           *session         // the last closed session
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
	connIt     enode.Iterator
	connSeen   map[enode.ID]struct{}
	connFound  []workload.Found
	connFullMs int64
	connSearch any

	adsFirst      map[string]map[string]int64 // topic hex -> advertiser id -> unix ms
	adsLast       map[string][]string
	adsExpiry     map[string]map[string]int64 // topic hex -> advertiser id -> expiry unix ms, last snapshot
	staleMaxMs    int64                       // longest an expired ad was still in the table
	adsSnapshotMs int64                       // when the last ad snapshot was taken
	bucketFullMs  []int64
	regCompleteMs int64
	bucketsLast   []topicindex.BucketStats

	lookups []workload.Lookup

	// Read from the server by snapshotLive and snapshotCounters.
	out, in, adsHeld int
	wire             map[string]discover.WireCounter
	ops              []map[string]any
	topicLoad        map[string]discover.TopicLoad
}

func newSession(started time.Time) *session {
	return &session{started: started, firstCapableMs: -1, connSeen: map[enode.ID]struct{}{}, connFound: []workload.Found{}, connFullMs: -1,
		adsFirst: map[string]map[string]int64{}, adsLast: map[string][]string{}, adsExpiry: map[string]map[string]int64{}, regCompleteMs: -1}
}

func newNode(asg assign.Assignment, key *ecdsa.PrivateKey) *node {
	return &node{asg: asg, key: key, id: enode.PubkeyToIDV4(&key.PublicKey), firstCapableMs: -1,
		lookupOnly: asg.Search.Model == "continuous" || asg.Search.Model == "scheduled"}
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

// up starts a new p2p server. Its phase times past, it registers and
// searches at once.
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
		Name: "topdisc-test", Version: 1, Length: 1,
		// A connection with no shared capability is dropped as useless, so
		// hold one open that carries no messages.
		Run: func(p *p2p.Peer, rw p2p.MsgReadWriter) error {
			for {
				if _, err := rw.ReadMsg(); err != nil {
					return err
				}
			}
		},
	}
	// Lookup-only models never dial; the connection-driven model records the
	// dialer's search: registrants handed to the dialer, its progress, and when
	// the outbound slots first filled.
	s.dialIt = newLazyIter(s.ready, func() enode.Iterator {
		it := s.srv.DiscoveryV5().TopicSearch(n.topic, uint64(n.port))
		n.mu.Lock()
		s.connStart, s.connIt = time.Now(), it
		n.mu.Unlock()
		return it
	}, func(nd *enode.Node) {
		n.mu.Lock()
		defer n.mu.Unlock()
		if _, ok := s.connSeen[nd.ID()]; !ok {
			s.connSeen[nd.ID()] = struct{}{}
			s.connFound = append(s.connFound, workload.Found{ID: nd.ID().String(), AtMs: time.Since(s.connStart).Milliseconds()})
		}
	})
	proto.DialCandidates = s.dialIt
	if n.lookupOnly {
		// The node only consumes its own lookups: no dialer, so no second search.
		proto.DialCandidates = enode.IterNodes(nil)
	}
	a := n.asg
	s.srv = &p2p.Server{Config: p2p.Config{
		PrivateKey: n.key, Name: "topdisc-node", MaxPeers: n.maxPeers, DialRatio: n.dialRatio, NoDial: n.lookupOnly,
		ListenAddr: fmt.Sprintf("%s:%d", n.ip, n.port), DiscoveryV4: false, DiscoveryV5: true,
		BootstrapNodesV5: n.bootnodes, Protocols: []p2p.Protocol{proto}, Logger: log.Root(),
		DiscoveryV5Topic: topicindex.Config{
			AdLifetime: time.Duration(a.Topic.AdLifetimeMs) * time.Millisecond, AdCacheSize: a.Topic.AdCacheSize,
			RegAttemptTimeout: time.Duration(a.Topic.RegAttemptTimeoutMs) * time.Millisecond, SearchBucketSize: a.Topic.SearchBucketSize,
			TopicNodesLimit: a.Topic.TopicNodesLimit, AuxNodesLimit: a.Topic.AuxNodesLimit, SearchYieldFloor: a.Topic.SearchYieldFloor, SearchAuxRadius: a.Topic.SearchAuxRadius,
			SearchTableDepth: a.Topic.SearchTableDepth, RegTableDepth: a.Topic.RegTableDepth,
			RegBucketSize: a.Topic.RegBucketSize, RegBucketStandbyLimit: a.Topic.RegBucketStandby,
		},
	}}
	if err := s.srv.Start(); err != nil {
		s.cancel()
		return err
	}
	s.srv.LocalNode().SetFallbackIP(net.ParseIP(n.ip))
	if a.ExtIP != "" {
		s.srv.LocalNode().SetStaticIP(net.ParseIP(a.ExtIP))
	}
	s.srv.LocalNode().Set(svcOf(n.topicName)) // the service, readable by legacy nodes too
	events := make(chan *p2p.PeerEvent, 64)
	s.sub = s.srv.SubscribeEvents(events)
	n.mu.Lock()
	n.cur = s
	n.mu.Unlock()
	s.wg.Add(3)
	go n.watchAds(s)
	go n.watchPeers(s, events)
	go n.phases(s)
	if a.SampleMs > 0 {
		s.wg.Add(1)
		go n.sampler(s)
	}
	log.Info("node up", "enode", s.srv.Self().URLv4(), "session", len(n.sessions)+1)
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
	log.Info("node down", "sessions", len(n.sessions))
	return nil
}

// fold closes the current session into the node's totals.
func (n *node) fold(s *session, end time.Time) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.lookupsDone = append(n.lookupsDone, s.lookups...)
	n.opsDone = append(n.opsDone, s.ops...)
	n.wireDone = addWire(n.wireDone, s.wire)
	n.topicLoadDone = addLoad(n.topicLoadDone, s.topicLoad)
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

func (n *node) phases(s *session) {
	defer s.wg.Done()
	a := n.asg
	if !waitCtx(s.ctx, a.Phases.RegisterAt) {
		return
	}
	s.srv.DiscoveryV5().RegisterTopic(n.topic, uint64(n.port))
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
	open := func() enode.Iterator { return s.srv.DiscoveryV5().TopicSearch(n.topic, uint64(n.port)+1) }
	timeout := time.Duration(a.Search.RequestTimeout) * time.Millisecond
	if a.Search.Model == "scheduled" {
		at := make([]time.Time, len(a.Search.LookupAtMs))
		for i, ms := range a.Search.LookupAtMs {
			at[i] = time.UnixMilli(ms)
		}
		workload.Scheduled(open, nil, a.Search.TargetCount, timeout, at, deadline, s.stop, func(l workload.Lookup) {
			n.mu.Lock()
			s.lookups = append(s.lookups, l)
			n.mu.Unlock()
		})
		return
	}
	workload.Continuous(open, nil, a.Search.InitialResults, time.Duration(a.Search.ResultIntervalMs)*time.Millisecond, deadline, s.stop, func(l workload.Lookup) {
		n.mu.Lock()
		s.lookups = []workload.Lookup{l}
		n.mu.Unlock()
	})
}

// watchAds is the registrar side: every second, which advertisers' ads this
// node holds, per topic. First-seen times give registration timing and
// placements; the last snapshot gives coverage. This is what simnet's harness
// probes from outside; here the registrar reports it. The advertiser side:
// when each registration bucket first held its target number of ads, when the
// whole table first was complete (every bucket full or out of candidates), and
// the last table state.
func (n *node) watchAds(s *session) {
	defer s.wg.Done()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	d := s.srv.DiscoveryV5()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		if bs := d.TopicRegistrationBuckets(n.topic); bs != nil {
			now := time.Now().UnixMilli()
			n.mu.Lock()
			if s.bucketFullMs == nil {
				s.bucketFullMs = make([]int64, len(bs))
				for i := range s.bucketFullMs {
					s.bucketFullMs[i] = -1
				}
			}
			complete := true
			for i, b := range bs {
				if b.Registered >= b.Target && s.bucketFullMs[i] < 0 {
					s.bucketFullMs[i] = now
				}
				if b.Registered < b.Target && (b.Waiting > 0 || b.Standby > 0) {
					complete = false
				}
			}
			if complete && s.regCompleteMs < 0 {
				s.regCompleteMs = now
			}
			s.bucketsLast = bs
			n.mu.Unlock()
		}
		_, _, byTopic := d.TopicCacheOccupancy()
		now := time.Now().UnixMilli()
		last, expiry, stale := map[string][]string{}, map[string]map[string]int64{}, int64(0)
		var seen [][2]string
		for tid, c := range byTopic {
			if c == 0 {
				continue
			}
			key := tid.String()
			expiry[key] = map[string]int64{}
			for _, ad := range d.LocalTopicAds(tid) {
				id := ad.Node.ID().String()
				if ad.ExpiresIn < 0 {
					stale = max(stale, (-ad.ExpiresIn).Milliseconds())
					continue
				}
				seen = append(seen, [2]string{key, id})
				last[key] = append(last[key], id)
				expiry[key][id] = now + ad.ExpiresIn.Milliseconds()
			}
		}
		n.mu.Lock()
		for _, ka := range seen {
			if s.adsFirst[ka[0]] == nil {
				s.adsFirst[ka[0]] = map[string]int64{}
			}
			if _, ok := s.adsFirst[ka[0]][ka[1]]; !ok {
				s.adsFirst[ka[0]][ka[1]] = now
			}
		}
		s.adsLast, s.adsExpiry, s.adsSnapshotMs = last, expiry, now
		s.staleMaxMs = max(s.staleMaxMs, stale)
		n.mu.Unlock()
	}
}

// sampler takes the periodic counters and cache occupancy for the time series.
func (n *node) sampler(s *session) {
	defer s.wg.Done()
	t := time.NewTicker(time.Duration(n.asg.SampleMs) * time.Millisecond)
	defer t.Stop()
	d := s.srv.DiscoveryV5()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		held, capacity, byTopic := d.TopicCacheOccupancy()
		bt := map[string]int{}
		for tid, c := range byTopic {
			bt[tid.String()] = c
		}
		n.mu.Lock()
		var rb []bucketCounts
		for _, b := range s.bucketsLast {
			rb = append(rb, bucketCounts{Registered: b.Registered, Waiting: b.Waiting, Standby: b.Standby})
		}
		n.mu.Unlock()
		n.addSample(sample{AtMs: time.Now().UnixMilli(), Wire: d.WireStats(), CacheHeld: held, CacheCap: capacity, ByTopic: bt, RegBuckets: rb})
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

// watchPeers measures refill latency from the server's own peer events: an
// outbound drop starts a clock, the next outbound add stops it.
func (n *node) watchPeers(s *session, events chan *p2p.PeerEvent) {
	defer s.wg.Done()
	self := s.srv.Self().ID()
	for {
		var ev *p2p.PeerEvent
		select {
		case <-s.ctx.Done():
			return
		case ev = <-events:
		}
		if ev.Type == p2p.PeerEventTypeDrop && ev.Peer != self {
			n.mu.Lock()
			n.drops++
			s.lostAt = append(s.lostAt, time.Now())
			n.mu.Unlock()
		} else if ev.Type == p2p.PeerEventTypeAdd {
			peers := s.srv.Peers()
			outbound, capable := 0, false
			for _, p := range peers {
				if !p.Inbound() {
					outbound++
				}
				if p.ID() == ev.Peer && topicindex.SupportsTopicDiscovery(p.Node()) {
					capable = true
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
			if s.firstCapableMs < 0 && capable {
				s.firstCapableMs = time.Since(s.started).Milliseconds()
			}
			n.mu.Unlock()
		}
	}
}

// snapshotLive reads what only a running server can tell: peers and held ads.
func (n *node) snapshotLive(s *session) {
	out, in := 0, 0
	for _, p := range s.srv.Peers() {
		if p.Inbound() {
			in++
		} else {
			out++
		}
	}
	held := len(s.srv.DiscoveryV5().LocalTopicNodes(n.topic))
	n.mu.Lock()
	s.out, s.in, s.adsHeld = out, in, held
	n.mu.Unlock()
}

// snapshotCounters reads the server's own counters; each new server starts
// them from zero.
func (n *node) snapshotCounters(s *session) {
	d := s.srv.DiscoveryV5()
	wire := d.WireStats()
	ops := []map[string]any{}
	for k, v := range d.OpStats() {
		ops = append(ops, map[string]any{"msg": k.Msg, "opid": k.OpID, "txMsgs": v.TxMsgs, "txBytes": v.TxBytes, "rxMsgs": v.RxMsgs, "rxBytes": v.RxBytes, "nodes": v.Nodes, "nodeIds": v.NodeIDs})
	}
	load := map[string]discover.TopicLoad{}
	for t, l := range d.TopicLoadStats() {
		load[t.String()] = l
	}
	n.mu.Lock()
	it := s.connIt
	n.mu.Unlock()
	var search any
	if st, ok := it.(interface {
		Stats() discover.TopicSearchStats
	}); ok {
		search = st.Stats()
	}
	n.mu.Lock()
	s.wire, s.ops, s.topicLoad, s.connSearch = wire, ops, load, search
	n.mu.Unlock()
}

// trace is the node's record of the whole run, one map with every session:
// counters summed, event lists joined, the registrar and peer state of the
// last session, and sessions[] listing each online period. The per-field rules
// are host.LoadTrace's for the session files older runs left.
func (n *node) trace() map[string]any {
	n.mu.Lock()
	defer n.mu.Unlock()
	now := time.Now()
	wire, load := addWire(n.wireDone, nil), addLoad(n.topicLoadDone, nil)
	lookups := append([]workload.Lookup{}, n.lookupsDone...)
	ops := append([]map[string]any{}, n.opsDone...)
	capable := n.firstCapableMs
	found := append([]workload.Found{}, n.connDone...)
	connSrc := n.connLast
	sessions := append([]span{}, n.sessions...)
	s := n.last
	if c := n.cur; c != nil {
		s = c
		wire, load = addWire(wire, c.wire), addLoad(load, c.topicLoad)
		lookups, ops = append(lookups, c.lookups...), append(ops, c.ops...)
		capable = minCapable(capable, c.firstCapableMs)
		if !c.connStart.IsZero() {
			found, connSrc = append(found, absFound(c)...), c
		}
		sessions = append(sessions, span{c.started.UnixMilli(), now.UnixMilli()})
	}
	if len(wire) == 0 {
		wire = nil
	}
	var conn map[string]any
	if !n.lookupOnly && connSrc != nil {
		start := connSrc.connStart.UnixMilli()
		rel := make([]workload.Found, len(found))
		for i, f := range found {
			rel[i] = workload.Found{ID: f.ID, AtMs: f.AtMs - start}
		}
		conn = map[string]any{"start_ms": start, "found": rel, "slots_filled_ms": connSrc.connFullMs}
		if connSrc.connSearch != nil {
			conn["search"] = connSrc.connSearch
		}
	}
	waits := map[string]discover.WaitTimeStats{}
	for tid, st := range discover.WaitTimeStatsSnapshot() {
		waits[tid.String()] = st
	}
	refills := append([]int64{}, n.refills...)
	samples := append([]sample{}, n.samples...)
	a := n.asg
	m := map[string]any{
		"idx": a.Idx, "id": n.id.String(), "peer_drops": n.drops, "refill_ms": refills, "lookups": lookups, "first_capable_ms": capable, "legacy": false,
		"wire": wire, "topic": n.topic.String(), "topics": a.Topics, "register_at_ms": a.Phases.RegisterAt, "search_at_ms": a.Phases.SearchAt,
		"wait": waits, "samples": samples, "admissions": discover.AdmissionEvents(n.id),
		"ops": ops, "topic_load": load, "conn": conn, "sessions": sessions,
		"outbound": 0, "inbound": 0, "ads_held": 0, "ads_first_seen_ms": map[string]map[string]int64{}, "ads_final": map[string][]string{},
		"ads_final_expiry_ms": map[string]map[string]int64{}, "ads_snapshot_ms": int64(0), "stale_ad_max_ms": int64(0),
		"reg_bucket_full_ms": []int64(nil), "reg_complete_ms": int64(-1), "reg_buckets_final": []topicindex.BucketStats(nil),
	}
	if s != nil {
		first := make(map[string]map[string]int64, len(s.adsFirst))
		for k, v := range s.adsFirst {
			first[k] = make(map[string]int64, len(v))
			for id, at := range v {
				first[k][id] = at
			}
		}
		m["outbound"], m["inbound"], m["ads_held"] = s.out, s.in, s.adsHeld
		m["ads_first_seen_ms"], m["ads_final"], m["ads_final_expiry_ms"], m["ads_snapshot_ms"], m["stale_ad_max_ms"] = first, s.adsLast, s.adsExpiry, s.adsSnapshotMs, s.staleMaxMs
		m["reg_bucket_full_ms"], m["reg_complete_ms"], m["reg_buckets_final"] = append([]int64(nil), s.bucketFullMs...), s.regCompleteMs, s.bucketsLast
	}
	return m
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

// status is what /status and the periodic log report; zeros while down.
func (n *node) status() map[string]any {
	n.mu.Lock()
	s := n.cur
	lookups, refills := len(n.lookupsDone), len(n.refills)
	if s != nil {
		lookups += len(s.lookups)
	}
	sessions := len(n.sessions)
	n.mu.Unlock()
	m := map[string]any{"up": s != nil, "sessions": sessions, "goroutines": runtime.NumGoroutine(), "enode": "", "peers": 0, "outbound": 0, "inbound": 0,
		"table": 0, "ads_held": 0, "lookups": lookups, "refills": refills}
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
	d := s.srv.DiscoveryV5()
	m["sessions"] = sessions + 1
	m["enode"], m["peers"], m["outbound"], m["inbound"] = s.srv.Self().URLv4(), out+in, out, in
	m["table"], m["ads_held"] = len(d.AllNodes()), len(d.LocalTopicNodes(n.topic))
	return m
}

func absFound(s *session) []workload.Found {
	start := s.connStart.UnixMilli()
	out := make([]workload.Found, len(s.connFound))
	for i, f := range s.connFound {
		out[i] = workload.Found{ID: f.ID, AtMs: start + f.AtMs}
	}
	return out
}

func minCapable(a, b int64) int64 {
	if a < 0 || (b >= 0 && b < a) {
		return b
	}
	return a
}

func addCounter(a, b discover.WireCounter) discover.WireCounter {
	return discover.WireCounter{TxMsgs: a.TxMsgs + b.TxMsgs, TxBytes: a.TxBytes + b.TxBytes, RxMsgs: a.RxMsgs + b.RxMsgs, RxBytes: a.RxBytes + b.RxBytes}
}

func addWire(a, b map[string]discover.WireCounter) map[string]discover.WireCounter {
	out := make(map[string]discover.WireCounter, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = addCounter(out[k], v)
	}
	return out
}

func addLoad(a, b map[string]discover.TopicLoad) map[string]discover.TopicLoad {
	out := make(map[string]discover.TopicLoad, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		o := out[k]
		out[k] = discover.TopicLoad{Regtopic: addCounter(o.Regtopic, v.Regtopic), TopicQuery: addCounter(o.TopicQuery, v.TopicQuery)}
	}
	return out
}
