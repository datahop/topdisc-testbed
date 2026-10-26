// node-legacy is the legacy half of the TopDisc-vs-discv5 comparison: a stock
// upstream geth p2p.Server with discv5 and no topic discovery at all. Built
// with -tags fork -modfile=go.fork.mod it runs on the fork's discv5
// instrumentation branch instead, with its per-message counters and pinned
// entries (wire_fork.go). It
// publishes its service in the ENR and finds providers by random-walk
// discovery, which is all a topic-unaware client can do. Same assignment file
// and trace record as cmd/node, so the figures compare directly.
package main

import (
	crand "crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"
	"github.com/ethereum/go-ethereum/rlp"
)

// The subset of pkg/assign.Assignment this binary reads; kept in step by hand.
type assignment struct {
	Idx        int      `json:"idx"`
	Key        string   `json:"key"`
	IP         string   `json:"ip"`
	ExtIP      string   `json:"ext_ip"`
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
	SampleMs  int64    `json:"sample_ms"`
	TraceFile string   `json:"trace_file"`
	Ghosts    []string `json:"ghosts"`
	Churn     []struct {
		DownAt, UpAt float64 // seconds from search start
	} `json:"churn"`
}

type lookup struct {
	StartMs   int64 `json:"start_ms"`
	LatencyMs int64 `json:"latency_ms"`
	Results   int   `json:"results"`
	HitTarget bool  `json:"hit_target"`
	// Which providers the walk turned up and when, the same shape cmd/node
	// records. Without it a walk reports only how many it found, so the
	// aggregator can build no recall-over-time curve and no per-provider
	// timing for a legacy searcher -- the figures the TopDisc runs are read
	// through. The count alone also hides whether a walk was still making
	// progress when the phase ended.
	Found []found `json:"found"`
	// The walk's cost: FINDNODE requests the node sent while it ran (fork
	// build, so it compares with cmd/node's TOPICQUERY count), or the
	// random-target lookups it started (stock build, which counts no
	// requests). Contacted stays "not reported" in both.
	Queries   int `json:"queries"`
	Contacted int `json:"contacted"`
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

// ghostEntry reads the "ghost" ENR key the testbed sets on every ghost record
// (the fork's discover.TestbedGhost, which the stock build lacks).
type ghostEntry uint

func (ghostEntry) ENRKey() string { return "ghost" }

// found is a provider first seen at AtMs after the search started.
type found struct {
	ID   string `json:"id"`
	AtMs int64  `json:"at_ms"`
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
	n := newNode(asg, key)
	for _, u := range asg.Bootnodes {
		if u = strings.TrimSpace(u); u != "" {
			bn, err := enode.Parse(enode.ValidSchemes, u)
			if err != nil {
				fmt.Fprintln(os.Stderr, "bad bootnode:", err)
				os.Exit(2)
			}
			n.bootnodes = append(n.bootnodes, bn)
			n.bootIDs[bn.ID()] = struct{}{}
		}
	}
	if n.ghosts, err = parseGhosts(asg.Ghosts); err != nil {
		fmt.Fprintln(os.Stderr, "bad ghost:", err)
		os.Exit(2)
	}
	for _, g := range n.ghosts {
		n.ghostIDs[g.ID()] = struct{}{}
	}
	enableWireStats()

	// The status port carries the churn control, so it is up before the node
	// is: a departure can come before the node's own search start.
	control := func(w http.ResponseWriter, err error) {
		switch {
		case err == nil:
			fmt.Fprintln(w, "ok")
		case errors.Is(err, errUp), errors.Is(err, errDown), errors.Is(err, errStopped):
			http.Error(w, err.Error(), http.StatusConflict)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
	http.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(n.status()) })
	http.HandleFunc("/down", func(w http.ResponseWriter, r *http.Request) { control(w, n.down()) })
	http.HandleFunc("/up", func(w http.ResponseWriter, r *http.Request) { control(w, n.up()) })
	go http.ListenAndServe(fmt.Sprintf("%s:%d", asg.IP, asg.StatusPort), nil)

	// An arrival is not there before its first return: it waits for /up.
	arrival := len(asg.Churn) > 0 && asg.Churn[0].DownAt == 0
	wait(asg.Phases.StartAt)
	if !arrival {
		if err := n.up(); err != nil {
			fmt.Fprintln(os.Stderr, "start:", err)
			os.Exit(1)
		}
	}
	if asg.Phases.StopAt > 0 {
		go func() { wait(asg.Phases.StopAt); n.finish(); os.Exit(0) }()
	}
	log.Info("legacy node ready", "status", asg.StatusPort, "arrival", arrival)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	n.finish()
}

// randomWalk is one scheduled probe of a topic-unaware client: check the
// nodes it already knows, then run random-target Kademlia lookups (each
// costs real FINDNODE traffic) until target providers are seen, the timeout
// or the deadline. cost, if set, reads the node's FINDNODE requests sent so
// far.
func randomWalk(disc interface {
	AllNodes() []*enode.Node
	Lookup(enode.ID) []*enode.Node
}, cost func() int64, isProvider func(*enode.Node) bool, target int, timeout time.Duration, deadline time.Time, stopCh <-chan struct{}) lookup {
	start := time.Now()
	stop := deadline
	if timeout > 0 && start.Add(timeout).Before(stop) {
		stop = start.Add(timeout)
	}
	// Insertion order is the discovery order, so the slice carries both the
	// identities and when each one first appeared.
	seen := map[enode.ID]struct{}{}
	var order []found
	rounds := 0
	var sent0 int64
	if cost != nil {
		sent0 = cost()
	}
	add := func(nodes []*enode.Node) bool {
		for _, n := range nodes {
			if !isProvider(n) {
				continue
			}
			if _, dup := seen[n.ID()]; dup {
				continue
			}
			seen[n.ID()] = struct{}{}
			order = append(order, found{ID: n.ID().String(), AtMs: time.Since(start).Milliseconds()})
		}
		return target > 0 && len(seen) >= target
	}
	done := func(hit bool) lookup {
		f := order[:len(order):len(order)]
		q := rounds
		if cost != nil {
			q = int(cost() - sent0)
		}
		return lookup{StartMs: start.UnixMilli(), LatencyMs: time.Since(start).Milliseconds(), Results: len(seen), HitTarget: hit, Found: f, Queries: q, Contacted: -1}
	}
	if add(disc.AllNodes()) {
		return done(true)
	}
	for time.Now().Before(stop) {
		rounds++ // counted when sent, so a lookup cut off by the deadline still counts
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
		case <-time.After(time.Until(stop)):
			return done(false)
		case <-stopCh:
			return done(false)
		}
	}
	return done(false)
}

// walkIterator is the continuous search of a topic-unaware client: check the
// nodes it already knows, then read the discovery random walk the way a
// dialer does, checking every record the walk yields, until the deadline. A
// finished lookup only returns the nodes closest to its target; the walk
// iterator yields every new node each reply carried, so it sees far more of
// the network per request. progress, if set, gets the lookup so far whenever
// a provider is added. cost, if set, reads the node's FINDNODE requests sent
// so far; without it the request count is not reported. After initial
// providers, the walk is paced to one new provider per interval.
func walkIterator(disc interface {
	AllNodes() []*enode.Node
	RandomNodes() enode.Iterator
}, cost func() int64, isProvider func(*enode.Node) bool, deadline time.Time, stopCh <-chan struct{}, progress func(lookup), initial int, interval time.Duration) lookup {
	start := time.Now()
	seen := map[enode.ID]struct{}{}
	var order []found
	var sent0 int64
	if cost != nil {
		sent0 = cost()
	}
	add := func(n *enode.Node) bool {
		if n == nil || !isProvider(n) {
			return false
		}
		if _, dup := seen[n.ID()]; dup {
			return false
		}
		seen[n.ID()] = struct{}{}
		order = append(order, found{ID: n.ID().String(), AtMs: time.Since(start).Milliseconds()})
		return true
	}
	snap := func() lookup {
		// Capacity-capped so later appends never land in a snapshot a
		// progress callback already holds.
		f := order[:len(order):len(order)]
		q := -1
		if cost != nil {
			q = int(cost() - sent0)
		}
		return lookup{StartMs: start.UnixMilli(), LatencyMs: time.Since(start).Milliseconds(), Results: len(seen), Found: f, Queries: q, Contacted: -1}
	}
	for _, n := range disc.AllNodes() {
		add(n)
	}
	if progress != nil && len(order) > 0 {
		progress(snap())
	}
	it := disc.RandomNodes()
	stop := time.AfterFunc(time.Until(deadline), it.Close)
	defer stop.Stop()
	halt := make(chan struct{})
	defer close(halt)
	go func() {
		select {
		case <-stopCh:
			it.Close()
		case <-halt:
		}
	}()
	var paceStart time.Time
	// The trace is written at the deadline, which is also when the walk
	// ends, so the last snapshot it holds is what gets recorded: refresh it
	// as the request count grows, not only when a provider turns up.
	lastSnap := time.Now()
	for it.Next() {
		if !add(it.Node()) {
			if progress != nil && time.Since(lastSnap) > 10*time.Second {
				progress(snap())
				lastSnap = time.Now()
			}
			continue
		}
		if progress != nil {
			progress(snap())
			lastSnap = time.Now()
		}
		if over := len(seen) - initial; interval > 0 && over > 0 {
			if paceStart.IsZero() {
				paceStart = time.Now()
			}
			select {
			case <-time.After(time.Until(paceStart.Add(time.Duration(over) * interval))):
			case <-time.After(time.Until(deadline)):
				it.Close()
			case <-stopCh:
				it.Close()
			}
		}
	}
	return snap()
}

var _ enr.Entry = svcEntry(0)
