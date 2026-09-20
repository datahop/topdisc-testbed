// crawl runs a continuous discv5 crawl with a liveness prober and writes an
// event log from which node sessions and their chains can be reconstructed.
//
// Events (one JSON object per line in <out>/events.jsonl, t in unix ms):
//
//	seen   a node record learned for the first time (with its chain keys)
//	enr    a node's record changed (new seq)
//	up     the node answered a PING after not being known alive
//	pong   a node that ever answered answered again (rtt in ms)
//	miss   a node that ever answered did not answer (err)
//	down   -misses unanswered PINGs in a row over at least two probe periods
//	sweep  periodic counts, every -sweep
//
// Every node ever seen is pinged forever. A node that has answered at least
// once is pinged every -probe, also after it went down, so a rejoin is
// caught with the same granularity; it is down once -misses pings in a row
// went unanswered over at least two probe periods. A new node gets -new-tries
// attempts at the same rate before it counts as a never-responder, which is
// then pinged every -probe-dead. Never-responders have their own, smaller
// worker pool, so they cannot delay the schedule of the nodes whose sessions
// are being measured.
package main

import (
	"bufio"
	"crypto/ecdsa"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/discover"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"
	"github.com/ethereum/go-ethereum/rlp"
)

func main() {
	var (
		listen    = flag.String("listen", ":30303", "UDP listen address")
		extIP     = flag.String("extip", "", "public IP to advertise")
		bnFile    = flag.String("bootnodes", "bootnodes.txt", "file with one enr: per line")
		out       = flag.String("out", "crawl-out", "output directory")
		duration  = flag.Duration("duration", 24*time.Hour, "how long to run; 0 = until SIGTERM")
		crawlers  = flag.Int("crawlers", 16, "concurrent random-walk lookups")
		probe     = flag.Duration("probe", 2*time.Minute, "ping period for nodes that ever answered")
		probeDead = flag.Duration("probe-dead", 30*time.Minute, "ping period for nodes that never answered")
		misses    = flag.Int("misses", 3, "unanswered pings in a row before a live node counts as down")
		newTries  = flag.Int("new-tries", 3, "attempts at the probe rate before a new node counts as a never-responder")
		workers   = flag.Int("workers", 256, "concurrent pings to nodes that ever answered or are new")
		deadWork  = flag.Int("dead-workers", 32, "concurrent pings to never-responders")
		sweep     = flag.Duration("sweep", 10*time.Minute, "period of the sweep count events")
		verbosity = flag.Int("verbosity", 3, "log verbosity")
	)
	flag.Parse()
	log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, log.FromLegacyLevel(*verbosity), false)))

	if err := os.MkdirAll(*out, 0o755); err != nil {
		fatal(err)
	}
	bootnodes, err := readBootnodes(*bnFile)
	if err != nil {
		fatal(err)
	}
	key, err := loadKey(filepath.Join(*out, "key"))
	if err != nil {
		fatal(err)
	}
	events, err := newEventLog(filepath.Join(*out, "events.jsonl"))
	if err != nil {
		fatal(err)
	}
	defer events.Close()

	addr, err := net.ResolveUDPAddr("udp", *listen)
	if err != nil {
		fatal(err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		fatal(err)
	}
	db, _ := enode.OpenDB("")
	ln := enode.NewLocalNode(db, key)
	if *extIP != "" {
		ln.SetStaticIP(net.ParseIP(*extIP))
	}
	ln.SetFallbackUDP(addr.Port)
	disc, err := discover.ListenV5(conn, ln, discover.Config{
		PrivateKey: key,
		Bootnodes:  bootnodes,
		Log:        log.Root(),
	})
	if err != nil {
		fatal(err)
	}
	log.Info("crawler up", "self", disc.Self().String(), "bootnodes", len(bootnodes))

	reg := newRegistry(events)
	for _, n := range bootnodes {
		reg.observe(n, "bootnode")
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < *crawlers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			it := disc.RandomNodes()
			defer it.Close()
			for it.Next() {
				reg.observe(it.Node(), "crawl")
				select {
				case <-stop:
					return
				default:
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		reg.probeLoop(disc, stop, probeConfig{probe: *probe, probeDead: *probeDead, misses: *misses, newTries: *newTries, workers: *workers, deadWorkers: *deadWork})
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(*sweep)
		defer t.Stop()
		n := 0
		for {
			select {
			case <-t.C:
				n++
				reg.sweep(n, *probe, *newTries)
			case <-stop:
				return
			}
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	var timeout <-chan time.Time
	if *duration > 0 {
		timeout = time.After(*duration)
	}
	select {
	case s := <-sig:
		log.Info("stopping", "signal", s)
	case <-timeout:
		log.Info("duration reached")
	}
	close(stop)
	disc.Close()
	wg.Wait()
	reg.sweep(-1, *probe, *newTries)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "crawl:", err)
	os.Exit(1)
}

func readBootnodes(path string) ([]*enode.Node, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var nodes []*enode.Node
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		s := strings.TrimSpace(sc.Text())
		s = strings.TrimPrefix(s, "- ")
		s = strings.Trim(s, `"'`)
		if !strings.HasPrefix(s, "enr:") {
			continue
		}
		n, err := enode.Parse(enode.ValidSchemes, s)
		if err != nil {
			log.Warn("bad bootnode", "line", s[:min(len(s), 40)], "err", err)
			continue
		}
		nodes = append(nodes, n)
	}
	return nodes, sc.Err()
}

func loadKey(path string) (*ecdsa.PrivateKey, error) {
	if key, err := crypto.LoadECDSA(path); err == nil {
		return key, nil
	}
	key, err := crypto.GenerateKey()
	if err != nil {
		return nil, err
	}
	return key, crypto.SaveECDSA(path, key)
}

// eventLog is an append-only JSON lines file.
type eventLog struct {
	mu sync.Mutex
	f  *os.File
	w  *bufio.Writer
}

func newEventLog(path string) (*eventLog, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	e := &eventLog{f: f, w: bufio.NewWriterSize(f, 1<<20)}
	go func() {
		for range time.Tick(5 * time.Second) {
			e.mu.Lock()
			e.w.Flush()
			e.mu.Unlock()
		}
	}()
	return e, nil
}

func (e *eventLog) write(v any) {
	b, _ := json.Marshal(v)
	e.mu.Lock()
	e.w.Write(b)
	e.w.WriteByte('\n')
	e.mu.Unlock()
}

func (e *eventLog) Close() {
	e.mu.Lock()
	e.w.Flush()
	e.f.Close()
	e.mu.Unlock()
}

// recordInfo is what the log keeps of a node record: address, keys present
// and the chain identifiers the known keys carry.
type recordInfo struct {
	T       int64    `json:"t"`
	Ev      string   `json:"ev"`
	ID      string   `json:"id"`
	Seq     uint64   `json:"seq"`
	IP      string   `json:"ip,omitempty"`
	UDP     int      `json:"udp,omitempty"`
	TCP     int      `json:"tcp,omitempty"`
	Keys    []string `json:"keys"`
	Eth2    string   `json:"eth2,omitempty"`    // fork digest (consensus)
	Eth     string   `json:"eth,omitempty"`     // fork hash (execution)
	EthNext uint64   `json:"ethNext,omitempty"` // next fork of the execution fork id
	OpStack uint64   `json:"opstack,omitempty"` // OP stack chain id
	ENR     string   `json:"enr"`
	Src     string   `json:"src"`
}

func describe(n *enode.Node, ev, src string) recordInfo {
	r := n.Record()
	info := recordInfo{T: now(), Ev: ev, ID: n.ID().String(), Seq: n.Seq(), ENR: n.String(), Src: src}
	if ip := n.IP(); ip != nil {
		info.IP = ip.String()
	}
	info.UDP, info.TCP = n.UDP(), n.TCP()
	info.Keys = recordKeys(r)
	var eth2 []byte
	if r.Load(enr.WithEntry("eth2", &eth2)) == nil && len(eth2) >= 4 {
		info.Eth2 = hex.EncodeToString(eth2[:4])
	}
	// The eth entry is [[forkHash, forkNext], ...].
	var eth struct {
		ForkID struct {
			Hash [4]byte
			Next uint64
		}
		Rest []rlp.RawValue `rlp:"tail"`
	}
	if r.Load(enr.WithEntry("eth", &eth)) == nil {
		info.Eth = hex.EncodeToString(eth.ForkID.Hash[:])
		info.EthNext = eth.ForkID.Next
	}
	var op []byte
	if r.Load(enr.WithEntry("opstack", &op)) == nil && len(op) > 0 {
		info.OpStack, _ = binary.Uvarint(op)
	}
	return info
}

// recordKeys lists the keys of a record from its RLP encoding: the record is
// [signature, seq, k1, v1, k2, v2, ...].
func recordKeys(r *enr.Record) []string {
	enc, err := rlp.EncodeToBytes(r)
	if err != nil {
		return nil
	}
	var elems []rlp.RawValue
	if err := rlp.DecodeBytes(enc, &elems); err != nil {
		return nil
	}
	var keys []string
	for i := 2; i+1 < len(elems); i += 2 {
		var k string
		if rlp.DecodeBytes(elems[i], &k) == nil {
			keys = append(keys, k)
		}
	}
	return keys
}

func now() int64 { return time.Now().UnixMilli() }

// registry tracks every node seen and its probe state.
type registry struct {
	mu        sync.Mutex
	nodes     map[enode.ID]*nodeState
	events    *eventLog
	pings     atomic.Int64
	pongs     atomic.Int64
	deadPings atomic.Int64
	deadPongs atomic.Int64
	errs      map[string]int64 // ping errors of ever-answered nodes, by kind
}

type nodeState struct {
	node     *enode.Node
	up       bool
	everUp   bool
	attempts int // pings sent while the node never answered
	misses   int // unanswered pings since the last answer
	lastPong time.Time
	upSince  time.Time
	pongs    int // answers in the current session
	next     time.Time
	queued   bool
}

// fast reports whether the node is on the probe schedule: it answered once,
// or it is new and still within its first tries.
func (st *nodeState) fast(newTries int) bool {
	return st.everUp || st.attempts < newTries
}

func newRegistry(events *eventLog) *registry {
	return &registry{nodes: make(map[enode.ID]*nodeState), events: events, errs: make(map[string]int64)}
}

// observe records a node learned from the crawl, a probe or the bootnode list.
func (r *registry) observe(n *enode.Node, src string) {
	r.mu.Lock()
	st, ok := r.nodes[n.ID()]
	switch {
	case !ok:
		r.nodes[n.ID()] = &nodeState{node: n}
		r.mu.Unlock()
		r.events.write(describe(n, "seen", src))
	case n.Seq() > st.node.Seq():
		st.node = n
		r.mu.Unlock()
		r.events.write(describe(n, "enr", src))
	default:
		r.mu.Unlock()
	}
}

type transition struct {
	T       int64  `json:"t"`
	Ev      string `json:"ev"`
	ID      string `json:"id"`
	Misses  int    `json:"misses,omitempty"`  // down: unanswered pings in a row
	SinceUp int64  `json:"sinceUp,omitempty"` // down: seconds since the session started
	Pongs   int    `json:"pongs,omitempty"`   // down: answers during the session
	Err     string `json:"err,omitempty"`     // down, miss: the ping's error
	RTT     int64  `json:"rtt,omitempty"`     // pong: round trip in ms
}

type sweepEvent struct {
	T         int64            `json:"t"`
	Ev        string           `json:"ev"`
	N         int              `json:"n"`
	Known     int              `json:"known"`
	EverUp    int              `json:"everUp"`
	Up        int              `json:"up"`
	Pings     int64            `json:"pings"`     // to nodes that ever answered
	Pongs     int64            `json:"pongs"`     // from them
	DeadPings int64            `json:"deadPings"` // to nodes that never answered
	DeadPongs int64            `json:"deadPongs"` // first answers
	Late      int              `json:"late"`      // ever-answered nodes overdue by more than a probe period
	New       int              `json:"new"`       // never answered, still within their first tries
	Dead      int              `json:"dead"`      // never answered after their first tries
	Errs      map[string]int64 `json:"errs"`      // ping errors of ever-answered nodes, by kind
}

func (r *registry) sweep(n int, probe time.Duration, newTries int) {
	r.mu.Lock()
	ev := sweepEvent{T: now(), Ev: "sweep", N: n, Known: len(r.nodes), Pings: r.pings.Load(), Pongs: r.pongs.Load(),
		DeadPings: r.deadPings.Load(), DeadPongs: r.deadPongs.Load(), Errs: make(map[string]int64, len(r.errs))}
	for k, v := range r.errs {
		ev.Errs[k] = v
	}
	late := time.Now().Add(-probe)
	for _, st := range r.nodes {
		switch {
		case st.everUp:
			ev.EverUp++
			if st.next.Before(late) {
				ev.Late++
			}
		case st.attempts < newTries:
			ev.New++
		default:
			ev.Dead++
		}
		if st.up {
			ev.Up++
		}
	}
	r.mu.Unlock()
	r.events.write(ev)
	log.Info("sweep", "n", n, "known", ev.Known, "everUp", ev.EverUp, "up", ev.Up, "new", ev.New, "dead", ev.Dead, "late", ev.Late,
		"pings", ev.Pings, "pongs", ev.Pongs, "deadPings", ev.DeadPings, "deadPongs", ev.DeadPongs, "errs", fmt.Sprint(ev.Errs))
}

type probeConfig struct {
	probe, probeDead     time.Duration
	misses, newTries     int
	workers, deadWorkers int
}

// probeLoop pings every node when it is due and records up/down transitions.
// Nodes on the fast schedule (ever answered, or new) are served by their own
// pool; never-responders by a smaller one whose schedule may slip.
func (r *registry) probeLoop(disc *discover.UDPv5, stop <-chan struct{}, cfg probeConfig) {
	live := make(chan *enode.Node, cfg.workers*4)
	dead := make(chan *enode.Node, cfg.deadWorkers*4)
	var wg sync.WaitGroup
	ping := func(n *enode.Node) {
		st := r.state(n.ID())
		fast := st != nil && st.fast(cfg.newTries)
		if fast {
			r.pings.Add(1)
		} else {
			r.deadPings.Add(1)
		}
		sent := time.Now()
		pong, err := disc.Ping(n)
		t := time.Now()
		r.mu.Lock()
		st = r.nodes[n.ID()]
		st.queued = false
		var ev, raw transition
		var refresh bool
		if err == nil {
			if fast {
				r.pongs.Add(1)
			} else {
				r.deadPongs.Add(1)
			}
			st.lastPong, st.misses = t, 0
			st.pongs++
			if st.everUp {
				raw = transition{Ev: "pong", RTT: t.Sub(sent).Milliseconds()}
			}
			if !st.up {
				st.up, st.everUp = true, true
				st.upSince, st.pongs = t, 1
				ev = transition{Ev: "up"}
			}
			refresh = pong.ENRSeq > st.node.Seq()
		} else {
			st.misses++
			if !st.everUp {
				st.attempts++
			} else {
				r.errs[errKind(err)]++
				raw = transition{Ev: "miss", Err: errKind(err)}
			}
			if st.up && st.misses >= cfg.misses && t.Sub(st.lastPong) >= 2*cfg.probe {
				st.up = false
				ev = transition{Ev: "down", Misses: st.misses, SinceUp: int64(t.Sub(st.upSince).Seconds()), Pongs: st.pongs, Err: errKind(err)}
			}
		}
		if st.fast(cfg.newTries) {
			st.next = t.Add(cfg.probe)
		} else {
			st.next = t.Add(cfg.probeDead)
		}
		r.mu.Unlock()
		if raw.Ev != "" {
			raw.T, raw.ID = now(), n.ID().String()
			r.events.write(raw)
		}
		if ev.Ev != "" {
			ev.T, ev.ID = now(), n.ID().String()
			r.events.write(ev)
		}
		if refresh {
			if nn, err := disc.RequestENR(n); err == nil {
				r.observe(nn, "probe")
			}
		}
	}
	for i := 0; i < cfg.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := range live {
				ping(n)
			}
		}()
	}
	for i := 0; i < cfg.deadWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := range dead {
				ping(n)
			}
		}()
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			close(live)
			close(dead)
			wg.Wait()
			return
		case <-tick.C:
		}
		var dueLive, dueDead []*enode.Node
		t := time.Now()
		r.mu.Lock()
		for _, st := range r.nodes {
			if st.queued || st.node.IP() == nil || st.node.UDP() == 0 || st.next.After(t) {
				continue
			}
			st.queued = true
			if st.fast(cfg.newTries) {
				dueLive = append(dueLive, st.node)
			} else {
				dueDead = append(dueDead, st.node)
			}
		}
		r.mu.Unlock()
		// Fast-schedule nodes are never dropped; never-responders are offered
		// only while their pool keeps up, so their schedule slips instead.
		for _, n := range dueLive {
			select {
			case live <- n:
			case <-stop:
				close(live)
				close(dead)
				wg.Wait()
				return
			}
		}
		for _, n := range dueDead {
			select {
			case dead <- n:
			default:
				r.mu.Lock()
				r.nodes[n.ID()].queued = false
				r.mu.Unlock()
			}
		}
	}
}

// errKind reduces a ping error to a short label for counting.
func errKind(err error) string {
	s := err.Error()
	if i := strings.IndexByte(s, ':'); i > 0 {
		s = s[:i]
	}
	if len(s) > 40 {
		s = s[:40]
	}
	return s
}

func (r *registry) state(id enode.ID) *nodeState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.nodes[id]
}
