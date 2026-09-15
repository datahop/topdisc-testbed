// node-legacy is the legacy half of the TopDisc-vs-discv5 comparison: a stock
// upstream geth p2p.Server with discv5 and no topic discovery at all. It
// publishes its service in the ENR and finds providers by random-walk
// discovery, which is all a topic-unaware client can do. Same assignment file
// and trace record as cmd/node, so the figures compare directly.
package main

import (
	crand "crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"
	"github.com/ethereum/go-ethereum/rlp"
)

// The subset of pkg/assign.Assignment this binary reads; kept in step by hand.
type assignment struct {
	Idx        int      `json:"idx"`
	Key        string   `json:"key"`
	IP         string   `json:"ip"`
	Port       int      `json:"port"`
	StatusPort int      `json:"status_port"`
	Bootnodes  []string `json:"bootnodes"`
	Topics     []string `json:"topics"`
	MaxPeers   int      `json:"max_peers"`
	DialRatio  int      `json:"dial_ratio"`
	Phases     struct {
		StartAt    int64 `json:"start_at_ms"`
		RegisterAt int64 `json:"register_at_ms"`
		SearchAt   int64 `json:"search_at_ms"`
		StopAt     int64 `json:"stop_at_ms"`
	} `json:"phases"`
	Search struct {
		Model            string  `json:"model"`
		TargetCount      int     `json:"target_count"`
		RequestTimeout   int64   `json:"request_timeout_ms"`
		LookupAtMs       []int64 `json:"lookup_at_ms"`
		InitialResults   int     `json:"initial_results"`
		ResultIntervalMs int64   `json:"result_interval_ms"`
	} `json:"search"`
	TraceFile string `json:"trace_file"`
}

type lookup struct {
	StartMs   int64 `json:"start_ms"`
	LatencyMs int64 `json:"latency_ms"`
	Results   int   `json:"results"`
	HitTarget bool  `json:"hit_target"`
}

// svcEntry is the "svc" ENR entry every testbed node carries: the service it
// provides, as a 32-bit hash of the topic name.
type svcEntry uint32

func (svcEntry) ENRKey() string { return "svc" }

func svcOf(topic string) svcEntry {
	h := fnv.New32a()
	h.Write([]byte(topic))
	return svcEntry(h.Sum32())
}

// capableEntry reads the fork's "ng" (topic discovery) ENR key without knowing its
// type: any value means a TopDisc-capable peer.
type capableEntry []byte

func (capableEntry) ENRKey() string { return "ng" }
func (c *capableEntry) DecodeRLP(s *rlp.Stream) error {
	b, err := s.Raw()
	*c = b
	return err
}

type lazyIter struct {
	ready <-chan struct{}
	open  func() enode.Iterator
	seen  func(*enode.Node) // called for every node handed to the dialer
	once  sync.Once
	it    enode.Iterator
}

func (l *lazyIter) Next() bool {
	l.once.Do(func() { <-l.ready; l.it = l.open() })
	ok := l.it.Next()
	if ok && l.seen != nil {
		l.seen(l.it.Node())
	}
	return ok
}

// found is a provider first seen at AtMs after the search started.
type found struct {
	ID   string `json:"id"`
	AtMs int64  `json:"at_ms"`
}

func (l *lazyIter) Node() *enode.Node { return l.it.Node() }
func (l *lazyIter) Close() {
	if l.it != nil {
		l.it.Close()
	}
}

func main() {
	assignmentPath := flag.String("assignment", "", "assignment JSON from the coordinator")
	verbosity := flag.Int("v", 2, "log verbosity")
	flag.Parse()
	log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, log.FromLegacyLevel(*verbosity), false)))
	if *assignmentPath == "" {
		fmt.Fprintln(os.Stderr, "need -assignment")
		os.Exit(2)
	}
	var asg assignment
	b, err := os.ReadFile(*assignmentPath)
	if err == nil {
		err = json.Unmarshal(b, &asg)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	key, err := crypto.HexToECDSA(asg.Key)
	if err != nil {
		panic(err)
	}
	wait := func(atMs int64) {
		if d := time.Until(time.UnixMilli(atMs)); d > 0 {
			time.Sleep(d)
		}
	}
	svc := svcOf(asg.Topics[0])
	var bootnodes []*enode.Node
	for _, u := range asg.Bootnodes {
		if u = strings.TrimSpace(u); u != "" {
			n, err := enode.Parse(enode.ValidSchemes, u)
			if err != nil {
				fmt.Fprintln(os.Stderr, "bad bootnode:", err)
				os.Exit(2)
			}
			bootnodes = append(bootnodes, n)
		}
	}

	wait(asg.Phases.StartAt)
	ready := make(chan struct{})
	var srv *p2p.Server
	isProvider := func(n *enode.Node) bool {
		var s svcEntry
		return n.Load(&s) == nil && s == svc
	}
	// Lookup-only models never dial; the connection-driven model records the
	// providers the dialer's random walk handed out and when slots filled.
	lookupOnly := asg.Search.Model == "continuous" || asg.Search.Model == "scheduled"
	var (
		connMu     sync.Mutex
		connStart  time.Time
		connSeen         = map[enode.ID]struct{}{}
		connFound        = []found{}
		connFullMs int64 = -1
	)
	proto := p2p.Protocol{
		Name: "topdisc-test", Version: 1, Length: 1, // same capability as cmd/node so RLPx peers stay
		Run: func(p *p2p.Peer, rw p2p.MsgReadWriter) error {
			for {
				if _, err := rw.ReadMsg(); err != nil {
					return err
				}
			}
		},
		DialCandidates: &lazyIter{ready: ready,
			open: func() enode.Iterator {
				connMu.Lock()
				connStart = time.Now()
				connMu.Unlock()
				return srv.DiscoveryV5().RandomNodes()
			},
			seen: func(n *enode.Node) {
				if !isProvider(n) {
					return
				}
				connMu.Lock()
				defer connMu.Unlock()
				if _, ok := connSeen[n.ID()]; !ok {
					connSeen[n.ID()] = struct{}{}
					connFound = append(connFound, found{ID: n.ID().String(), AtMs: time.Since(connStart).Milliseconds()})
				}
			}},
	}
	if lookupOnly {
		// The node only consumes its own walks: no dialer, so no second walk.
		proto.DialCandidates = enode.IterNodes(nil)
	}
	srv = &p2p.Server{Config: p2p.Config{
		PrivateKey: key, Name: "topdisc-node-legacy", MaxPeers: asg.MaxPeers, DialRatio: asg.DialRatio, NoDial: lookupOnly,
		ListenAddr: fmt.Sprintf("%s:%d", asg.IP, asg.Port), DiscoveryV4: false, DiscoveryV5: true,
		BootstrapNodesV5: bootnodes, Protocols: []p2p.Protocol{proto}, Logger: log.Root(),
	}}
	if err := srv.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "start:", err)
		os.Exit(1)
	}
	srv.LocalNode().SetFallbackIP(net.ParseIP(asg.IP))
	srv.LocalNode().Set(svc)

	var (
		mu             sync.Mutex
		lostAt         []time.Time
		refills        []int64
		drops          int
		lookups              = []lookup{}
		firstCapableMs int64 = -1
		start                = time.Now()
	)
	events := make(chan *p2p.PeerEvent, 64)
	sub := srv.SubscribeEvents(events)
	defer sub.Unsubscribe()
	go func() {
		for ev := range events {
			mu.Lock()
			switch ev.Type {
			case p2p.PeerEventTypeDrop:
				drops++
				lostAt = append(lostAt, time.Now())
			case p2p.PeerEventTypeAdd:
				if len(lostAt) > 0 {
					refills = append(refills, time.Since(lostAt[0]).Milliseconds())
					lostAt = lostAt[1:]
				}
				if !lookupOnly {
					outbound := 0
					for _, p := range srv.Peers() {
						if !p.Inbound() {
							outbound++
						}
					}
					connMu.Lock()
					if connFullMs < 0 && !connStart.IsZero() && outbound >= srv.MaxDialedConns() {
						connFullMs = time.Since(connStart).Milliseconds()
					}
					connMu.Unlock()
				}
				if firstCapableMs < 0 {
					for _, p := range srv.Peers() {
						var c capableEntry
						if p.ID() == ev.Peer && p.Node().Load(&c) == nil {
							firstCapableMs = time.Since(start).Milliseconds()
						}
					}
				}
			}
			mu.Unlock()
		}
	}()
	// A legacy node has nothing to register: its ENR carries the service.
	wait(asg.Phases.SearchAt)
	close(ready)
	if asg.Search.Model == "continuous" || asg.Search.Model == "scheduled" {
		deadline := time.UnixMilli(asg.Phases.StopAt)
		if asg.Phases.StopAt == 0 {
			deadline = time.Now().Add(24 * time.Hour)
		}
		timeout := time.Duration(asg.Search.RequestTimeout) * time.Millisecond
		rec := func(l lookup) { mu.Lock(); lookups = append(lookups, l); mu.Unlock() }
		disc := srv.DiscoveryV5()
		go func() {
			if asg.Search.Model == "scheduled" {
				for _, ms := range asg.Search.LookupAtMs {
					t := time.UnixMilli(ms)
					if !t.Before(deadline) {
						return
					}
					time.Sleep(time.Until(t))
					rec(randomWalk(disc, isProvider, asg.Search.TargetCount, timeout, deadline, nil, 0, 0))
				}
				return
			}
			// continuous: one walk for the whole phase, reported as a single lookup.
			set := func(l lookup) { mu.Lock(); lookups = []lookup{l}; mu.Unlock() }
			set(randomWalk(disc, isProvider, 0, 0, deadline, set, asg.Search.InitialResults, time.Duration(asg.Search.ResultIntervalMs)*time.Millisecond))
		}()
	}
	writeTrace := func() {
		if asg.TraceFile == "" {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		out, in := 0, 0
		for _, p := range srv.Peers() {
			if p.Inbound() {
				in++
			} else {
				out++
			}
		}
		if refills == nil {
			refills = []int64{}
		}
		var conn map[string]any
		connMu.Lock()
		if !lookupOnly && !connStart.IsZero() {
			conn = map[string]any{"start_ms": connStart.UnixMilli(), "found": connFound, "slots_filled_ms": connFullMs}
		}
		connMu.Unlock()
		b, _ := json.MarshalIndent(map[string]any{
			"conn": conn,
			"idx":  asg.Idx, "id": srv.Self().ID().String(), "legacy": true, "outbound": out, "inbound": in,
			"peer_drops": drops, "refill_ms": refills, "lookups": lookups, "first_capable_ms": firstCapableMs,
			"ads_held": 0, "wire": map[string]any{},
		}, "", " ")
		os.WriteFile(asg.TraceFile, b, 0o644)
	}
	if asg.Phases.StopAt > 0 {
		go func() { wait(asg.Phases.StopAt); writeTrace(); srv.Stop(); os.Exit(0) }()
	}
	log.Info("legacy node up", "enode", srv.Self().URLv4())
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	srv.Stop()
}

// randomWalk is what a topic-unaware client can do to find providers: check
// the nodes it already knows, then run random-target Kademlia lookups (each
// costs real FINDNODE traffic) until target providers are seen, the timeout
// or the deadline. progress, if set, gets the lookup so far after each round.
func randomWalk(disc interface {
	AllNodes() []*enode.Node
	Lookup(enode.ID) []*enode.Node
}, isProvider func(*enode.Node) bool, target int, timeout time.Duration, deadline time.Time, progress func(lookup), initial int, interval time.Duration) lookup {
	start := time.Now()
	stop := deadline
	if timeout > 0 && start.Add(timeout).Before(stop) {
		stop = start.Add(timeout)
	}
	seen := map[enode.ID]struct{}{}
	var paceStart time.Time
	add := func(nodes []*enode.Node) bool {
		for _, n := range nodes {
			if isProvider(n) {
				seen[n.ID()] = struct{}{}
			}
		}
		return target > 0 && len(seen) >= target
	}
	done := func(hit bool) lookup {
		return lookup{StartMs: start.UnixMilli(), LatencyMs: time.Since(start).Milliseconds(), Results: len(seen), HitTarget: hit}
	}
	if add(disc.AllNodes()) {
		return done(true)
	}
	for time.Now().Before(stop) {
		res := make(chan []*enode.Node, 1)
		go func() {
			var target enode.ID
			crand.Read(target[:])
			res <- disc.Lookup(target)
		}()
		select {
		case nodes := <-res:
			if add(nodes) {
				return done(true)
			}
			if progress != nil {
				progress(done(false))
			}
			if over := len(seen) - initial; interval > 0 && over > 0 {
				if paceStart.IsZero() {
					paceStart = time.Now()
				}
				select {
				case <-time.After(time.Until(paceStart.Add(time.Duration(over) * interval))):
				case <-time.After(time.Until(stop)):
					return done(false)
				}
			}
		case <-time.After(time.Until(stop)):
			return done(false)
		}
	}
	return done(false)
}

var _ enr.Entry = svcEntry(0)
