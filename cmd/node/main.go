// node is a full devp2p node whose only job is to register a topic and
// keep its peer slots filled from topic search: geth's p2p server, real RLPx
// connections, and the dialer fed by the TopicSearch iterator.
package main

import (
	"crypto/ecdsa"
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/datahop/topdisc-testbed/pkg/assign"
	"github.com/datahop/topdisc-testbed/pkg/workload"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/discover"
	"github.com/ethereum/go-ethereum/p2p/discover/topicindex"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

// lazyIter hands the dialer a TopicSearch iterator once the server is up.
// DialCandidates must exist before Start, but the discv5 instance only after.
type lazyIter struct {
	ready <-chan struct{}
	open  func() enode.Iterator
	once  sync.Once
	it    enode.Iterator
}

func (l *lazyIter) Next() bool {
	l.once.Do(func() { <-l.ready; l.it = l.open() })
	return l.it.Next()
}
func (l *lazyIter) Node() *enode.Node { return l.it.Node() }
func (l *lazyIter) Close() {
	if l.it != nil {
		l.it.Close()
	}
}

func main() {
	port := flag.Int("port", 30303, "TCP+UDP listen port")
	statusPort := flag.Int("status", 0, "status HTTP port (0 = port+10000)")
	boot := flag.String("bootnodes", "", "comma-separated enode URLs")
	topicName := flag.String("topic", "topdisc-test", "topic to register and search")
	maxPeers := flag.Int("maxpeers", 50, "peer slots")
	dialRatio := flag.Int("dialratio", 3, "1/N of slots are dialed")
	ip := flag.String("ip", "127.0.0.1", "address to listen on and advertise")
	verbosity := flag.Int("v", 2, "log verbosity")
	assignment := flag.String("assignment", "", "assignment JSON from the coordinator; overrides the other flags")
	flag.Parse()
	if *statusPort == 0 {
		*statusPort = *port + 10000
	}
	log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, log.FromLegacyLevel(*verbosity), false)))

	var (
		key  *ecdsa.PrivateKey
		err  error
		asg  assign.Assignment
		wait = func(int64) {}
	)
	if *assignment != "" {
		if asg, err = assign.Read(*assignment); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		key, err = crypto.HexToECDSA(asg.Key)
		*ip, *port, *statusPort, *maxPeers, *dialRatio = asg.IP, asg.Port, asg.StatusPort, asg.MaxPeers, asg.DialRatio
		*topicName, *boot = asg.Topics[0], strings.Join(asg.Bootnodes, ",")
		wait = func(atMs int64) {
			if d := time.Until(time.UnixMilli(atMs)); d > 0 {
				time.Sleep(d)
			}
		}
	} else {
		key, err = crypto.GenerateKey()
	}
	if err != nil {
		panic(err)
	}
	topic := topicindex.TopicID(crypto.Keccak256Hash([]byte(*topicName)))
	var bootnodes []*enode.Node
	for _, u := range strings.Split(*boot, ",") {
		if u = strings.TrimSpace(u); u != "" {
			n, err := enode.Parse(enode.ValidSchemes, u)
			if err != nil {
				fmt.Fprintln(os.Stderr, "bad bootnode:", err)
				os.Exit(2)
			}
			bootnodes = append(bootnodes, n)
		}
	}

	wait(asg.Phases.StartAt) // spread startups; a churn restart is past it and starts at once
	ready := make(chan struct{})
	var srv *p2p.Server
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
		DialCandidates: &lazyIter{ready: ready, open: func() enode.Iterator {
			return srv.DiscoveryV5().TopicSearch(topic, uint64(*port))
		}},
	}
	if asg.Search.Model == "continuous" {
		// The node only consumes its own search: no dialer, so no second search.
		proto.DialCandidates = enode.IterNodes(nil)
	}
	srv = &p2p.Server{Config: p2p.Config{
		PrivateKey: key, Name: "topdisc-node", MaxPeers: *maxPeers, DialRatio: *dialRatio, NoDial: asg.Search.Model == "continuous",
		ListenAddr: fmt.Sprintf("%s:%d", *ip, *port), DiscoveryV4: false, DiscoveryV5: true,
		BootstrapNodesV5: bootnodes, Protocols: []p2p.Protocol{proto}, Logger: log.Root(),
		DiscoveryV5Topic: topicindex.Config{
			AdLifetime: time.Duration(asg.Topic.AdLifetimeMs) * time.Millisecond, AdCacheSize: asg.Topic.AdCacheSize,
			RegAttemptTimeout: time.Duration(asg.Topic.RegAttemptTimeoutMs) * time.Millisecond, SearchBucketSize: asg.Topic.SearchBucketSize,
			TopicNodesLimit: asg.Topic.TopicNodesLimit, AuxNodesLimit: asg.Topic.AuxNodesLimit,
			SearchTableDepth: asg.Topic.SearchTableDepth, RegTableDepth: asg.Topic.RegTableDepth,
			RegBucketSize: asg.Topic.RegBucketSize, RegBucketStandbyLimit: asg.Topic.RegBucketStandby,
		},
	}}
	discover.EnableWireStats()
	discover.EnableWaitStats()
	if err := srv.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "start:", err)
		os.Exit(1)
	}
	srv.LocalNode().SetFallbackIP(net.ParseIP(*ip))
	srv.LocalNode().Set(svcOf(*topicName)) // the service, readable by legacy nodes too

	// Registrar side: every second, which advertisers' ads this node holds,
	// per topic. First-seen times give registration timing and placements;
	// the last snapshot gives coverage. This is what simnet's harness probes
	// from outside; here the registrar reports it.
	adsFirst := map[string]map[string]int64{} // topic hex -> advertiser id -> unix ms
	adsLast := map[string][]string{}
	var adsMu sync.Mutex
	// Advertiser side: when each registration bucket first held its target
	// number of ads, when the whole table first was complete (every bucket
	// full or out of candidates), and the last table state.
	var (
		bucketFullMs  []int64
		regCompleteMs = int64(-1)
		bucketsLast   []topicindex.BucketStats
	)
	go func() {
		d := srv.DiscoveryV5()
		for range time.Tick(time.Second) {
			if bs := d.TopicRegistrationBuckets(topic); bs != nil {
				now := time.Now().UnixMilli()
				adsMu.Lock()
				if bucketFullMs == nil {
					bucketFullMs = make([]int64, len(bs))
					for i := range bucketFullMs {
						bucketFullMs[i] = -1
					}
				}
				complete := true
				for i, b := range bs {
					if b.Registered >= b.Target && bucketFullMs[i] < 0 {
						bucketFullMs[i] = now
					}
					if b.Registered < b.Target && (b.Waiting > 0 || b.Standby > 0) {
						complete = false
					}
				}
				if complete && regCompleteMs < 0 {
					regCompleteMs = now
				}
				bucketsLast = bs
				adsMu.Unlock()
			}
			_, _, byTopic := d.TopicCacheOccupancy()
			now := time.Now().UnixMilli()
			adsMu.Lock()
			adsLast = map[string][]string{}
			for tid, n := range byTopic {
				if n == 0 {
					continue
				}
				key := tid.String()
				if adsFirst[key] == nil {
					adsFirst[key] = map[string]int64{}
				}
				for _, ad := range d.LocalTopicNodes(tid) {
					id := ad.ID().String()
					if _, ok := adsFirst[key][id]; !ok {
						adsFirst[key][id] = now
					}
					adsLast[key] = append(adsLast[key], id)
				}
			}
			adsMu.Unlock()
		}
	}()
	// Periodic counters and cache occupancy for the time series.
	type sample struct {
		AtMs      int64                           `json:"at_ms"`
		Wire      map[string]discover.WireCounter `json:"wire"`
		CacheHeld int                             `json:"cache_held"`
		CacheCap  int                             `json:"cache_cap"`
		ByTopic   map[string]int                  `json:"cache_by_topic"`
	}
	samples := []sample{}
	var samplesMu sync.Mutex
	if asg.SampleMs > 0 {
		go func() {
			d := srv.DiscoveryV5()
			for range time.Tick(time.Duration(asg.SampleMs) * time.Millisecond) {
				held, capacity, byTopic := d.TopicCacheOccupancy()
				bt := map[string]int{}
				for tid, n := range byTopic {
					bt[tid.String()] = n
				}
				samplesMu.Lock()
				samples = append(samples, sample{AtMs: time.Now().UnixMilli(), Wire: d.WireStats(), CacheHeld: held, CacheCap: capacity, ByTopic: bt})
				samplesMu.Unlock()
			}
		}()
	}

	// Refill latency from the server's own peer events: an outbound drop
	// starts a clock, the next outbound add stops it.
	var (
		mu      sync.Mutex
		lostAt  []time.Time
		refills []int64
		drops   int
		lookups []workload.Lookup

		firstCapableMs int64 = -1 // first TopDisc-capable RLPx peer, ms since start (§4)
		started              = time.Now()
	)
	events := make(chan *p2p.PeerEvent, 64)
	sub := srv.SubscribeEvents(events)
	defer sub.Unsubscribe()
	go func() {
		for ev := range events {
			if ev.Type == p2p.PeerEventTypeDrop && ev.Peer != enode.ID(srv.Self().ID()) {
				mu.Lock()
				drops++
				lostAt = append(lostAt, time.Now())
				mu.Unlock()
			} else if ev.Type == p2p.PeerEventTypeAdd {
				mu.Lock()
				if len(lostAt) > 0 {
					refills = append(refills, time.Since(lostAt[0]).Milliseconds())
					lostAt = lostAt[1:]
				}
				if firstCapableMs < 0 {
					for _, p := range srv.Peers() {
						if p.ID() == ev.Peer && topicindex.SupportsTopicDiscovery(p.Node()) {
							firstCapableMs = time.Since(started).Milliseconds()
						}
					}
				}
				mu.Unlock()
			}
		}
	}()
	wait(asg.Phases.RegisterAt)
	srv.DiscoveryV5().RegisterTopic(topic, uint64(*port))
	wait(asg.Phases.SearchAt)
	close(ready)
	if asg.Search.Model == "continuous" || asg.Search.Model == "scheduled" {
		deadline := time.UnixMilli(asg.Phases.StopAt)
		if asg.Phases.StopAt == 0 {
			deadline = time.Now().Add(24 * time.Hour)
		}
		open := func() enode.Iterator { return srv.DiscoveryV5().TopicSearch(topic, uint64(*port)+1) }
		rec := func(l workload.Lookup) { mu.Lock(); lookups = append(lookups, l); mu.Unlock() }
		timeout := time.Duration(asg.Search.RequestTimeout) * time.Millisecond
		if asg.Search.Model == "scheduled" {
			at := make([]time.Time, len(asg.Search.LookupAtMs))
			for i, ms := range asg.Search.LookupAtMs {
				at[i] = time.UnixMilli(ms)
			}
			go workload.Scheduled(open, nil, asg.Search.TargetCount, timeout, at, deadline, rec)
		} else {
			go workload.Continuous(open, nil, deadline, func(l workload.Lookup) { mu.Lock(); lookups = []workload.Lookup{l}; mu.Unlock() })
		}
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
		var wire map[string]discover.WireCounter
		if ws := srv.DiscoveryV5().WireStats(); len(ws) > 0 {
			wire = ws
		}
		wait := map[string]discover.WaitTimeStats{}
		for tid, st := range discover.WaitTimeStatsSnapshot() {
			wait[tid.String()] = st
		}
		adsMu.Lock()
		first, last := adsFirst, adsLast
		bFull, bLast, rComplete := bucketFullMs, bucketsLast, regCompleteMs
		adsMu.Unlock()
		samplesMu.Lock()
		smp := samples
		samplesMu.Unlock()
		if lookups == nil {
			lookups = []workload.Lookup{}
		}
		ops := []map[string]any{}
		for k, v := range srv.DiscoveryV5().OpStats() {
			ops = append(ops, map[string]any{"msg": k.Msg, "opid": k.OpID, "txMsgs": v.TxMsgs, "txBytes": v.TxBytes, "rxMsgs": v.RxMsgs, "rxBytes": v.RxBytes, "nodes": v.Nodes})
		}
		topicLoad := map[string]discover.TopicLoad{}
		for t, l := range srv.DiscoveryV5().TopicLoadStats() {
			topicLoad[t.String()] = l
		}
		b, _ := json.MarshalIndent(map[string]any{
			"idx": asg.Idx, "id": srv.Self().ID().String(), "outbound": out, "inbound": in,
			"peer_drops": drops, "refill_ms": refills, "lookups": lookups, "first_capable_ms": firstCapableMs, "legacy": false,
			"ads_held": len(srv.DiscoveryV5().LocalTopicNodes(topic)), "wire": wire,
			"topic": topic.String(), "topics": asg.Topics, "register_at_ms": asg.Phases.RegisterAt, "search_at_ms": asg.Phases.SearchAt,
			"ads_first_seen_ms": first, "ads_final": last, "wait": wait, "samples": smp,
			"reg_bucket_full_ms": bFull, "reg_complete_ms": rComplete, "reg_buckets_final": bLast,
			"ops": ops, "topic_load": topicLoad,
		}, "", " ")
		os.WriteFile(asg.TraceFile, b, 0o644)
	}
	if asg.Phases.StopAt > 0 {
		go func() { wait(asg.Phases.StopAt); writeTrace(); srv.Stop(); os.Exit(0) }()
	}
	log.Info("node up", "enode", srv.Self().URLv4(), "status", *statusPort)

	status := func() map[string]any {
		out, in := 0, 0
		for _, p := range srv.Peers() {
			if p.Inbound() {
				in++
			} else {
				out++
			}
		}
		d := srv.DiscoveryV5()
		return map[string]any{
			"enode": srv.Self().URLv4(), "peers": out + in, "outbound": out, "inbound": in,
			"table": len(d.AllNodes()), "ads_held": len(d.LocalTopicNodes(topic)),
			"lookups": func() int { mu.Lock(); defer mu.Unlock(); return len(lookups) }(),
			"refills": func() int { mu.Lock(); defer mu.Unlock(); return len(refills) }(),
		}
	}
	http.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(status()) })
	go http.ListenAndServe(fmt.Sprintf("%s:%d", *ip, *statusPort), nil)
	go func() {
		for range time.Tick(30 * time.Second) {
			s := status()
			log.Info("status", "peers", s["peers"], "out", s["outbound"], "in", s["inbound"], "table", s["table"], "ads", s["ads_held"])
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	srv.Stop()
}

// svcEntry is the "svc" ENR entry: the service a node provides, as a 32-bit
// hash of the topic name. Legacy nodes filter random-walk discovery on it.
type svcEntry uint32

func (svcEntry) ENRKey() string { return "svc" }

func svcOf(topic string) svcEntry {
	h := fnv.New32a()
	h.Write([]byte(topic))
	return svcEntry(h.Sum32())
}
