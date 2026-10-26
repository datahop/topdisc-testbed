package host

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Sample is one monitoring tick: the host and every node process on it. It
// tells operational trouble (a saturated machine, dropped UDP datagrams) from
// protocol behaviour when a run looks wrong.
type Sample struct {
	AtMs            int64        `json:"at_ms"`
	Load1           float64      `json:"load1"`
	MemAvailMB      int64        `json:"mem_avail_mb"`
	UDPRcvbufErrors int64        `json:"udp_rcvbuf_errors"` // cumulative, host-wide
	UDPInErrors     int64        `json:"udp_in_errors"`
	Nodes           []NodeSample `json:"nodes"`
	// With testbed.wan every node has its own namespace, whose drops the
	// counters above do not see. Netns sums them over the NetnsSampled of
	// NetnsExpected namespaces read (nil if none was); NetnsMs is the cost.
	Netns           *NetSample `json:"netns,omitempty"`
	NetnsMaxBacklog int64      `json:"netns_max_backlog_bytes,omitempty"`
	NetnsSampled    int        `json:"netns_sampled,omitempty"`
	NetnsExpected   int        `json:"netns_expected,omitempty"`
	NetnsMs         float64    `json:"netns_ms,omitempty"`
	ARP             *ARPSample `json:"arp,omitempty"` // nil if unread
	ENASample
	ENAError string `json:"ena_error,omitempty"` // an ENA host whose counters could not be read this time
}

// ARPSample: the ARP table is one for the machine, namespaces included. A
// packet to a neighbour not yet resolved, or refused by a full table, is
// dropped before any qdisc sees it. Cumulative since boot, except Entries.
type ARPSample struct {
	Entries            int64 `json:"entries"`
	UnresolvedDiscards int64 `json:"unresolved_discards"`
	TableFulls         int64 `json:"table_fulls"`
	ForcedGCRuns       int64 `json:"forced_gc_runs"`
}

// enaReader holds mappings until Close, which is called before every reader
// is dropped.
type enaReader interface {
	ena() (ENASample, error)
	Close()
}

// openENA is a variable for the tests.
var openENA = func() (enaReader, error) {
	e, err := openPrimaryENA()
	if err != nil {
		return nil, err
	}
	return e, nil
}

type NodeSample struct {
	Idx    int     `json:"idx"`
	CPUPct float64 `json:"cpu_pct"` // of one core, since the previous sample
	RSSMB  float64 `json:"rss_mb"`
	FDs    int     `json:"fds"`
	NetSample
}

type monitor struct {
	mu       sync.Mutex
	samples  []Sample
	lastCPU  map[int]float64 // idx -> cumulative cpu seconds
	lastAt   time.Time
	nsWarned bool
	ena      enaReader     // the primary interface, once opened
	enaOff   bool          // the host has no ENA device
	done     chan struct{} // closed once the sampling goroutine has cleaned up
	stop     chan struct{}
	stopOnce sync.Once
}

// Monitor samples every period until Stop; samples are kept in memory and
// written by WriteSamples.
func (r *Runner) Monitor(period time.Duration) {
	r.init()
	m := &monitor{lastCPU: map[int]float64{}, stop: make(chan struct{}), done: make(chan struct{})}
	r.mon = m
	go func() {
		t := time.NewTicker(period)
		defer t.Stop()
		defer close(m.done)
		defer m.closeENA() // on this goroutine: sample may be using the reader
		for {
			select {
			case <-m.stop:
				return
			case <-t.C:
				r.sample()
			}
		}
	}()
}

// StopMonitor is idempotent. The coordinator stops every agent at the end of
// a run and the next run prepares against the same agent, so a fleet that
// serves more than one run stops the same monitor twice; closing the channel
// again panicked the handler before main could swap in the new Runner, which
// left the agent wedged on the stale one for every later prepare. It returns
// once the sampler has stopped and released the ENA reader.
func (r *Runner) StopMonitor() {
	if r.mon == nil {
		return
	}
	r.mon.stopOnce.Do(func() { close(r.mon.stop) })
	if r.mon.done != nil {
		<-r.mon.done
	}
}

func (r *Runner) Samples() []Sample {
	if r.mon == nil {
		return nil
	}
	r.mon.mu.Lock()
	defer r.mon.mu.Unlock()
	return append([]Sample(nil), r.mon.samples...)
}

func (r *Runner) WriteSamples(path string) error {
	b, _ := json.Marshal(r.Samples())
	return os.WriteFile(path, b, 0o644)
}

func (r *Runner) sample() {
	m := r.mon
	now := time.Now()
	s := Sample{AtMs: now.UnixMilli()}
	s.Load1, s.MemAvailMB = hostLoad()
	s.UDPRcvbufErrors, s.UDPInErrors = udpErrors()
	if b, err := os.ReadFile("/proc/net/stat/arp_cache"); err == nil {
		if a := parseNeighStat(b); a != nil {
			s.ARP = &ARPSample{a["entries"], a["unresolved_discards"], a["table_fulls"], a["forced_gc_runs"]}
		}
	}
	m.sampleENA(&s)
	r.mu.Lock()
	pids := map[int]int{}
	var names []string
	if r.Wan != nil {
		for idx := range r.delay {
			names = append(names, nsName(idx))
		}
	}
	for idx, p := range r.procs {
		select {
		case <-p.done:
			continue
		default:
		}
		pids[idx] = p.cmd.Process.Pid
	}
	r.mu.Unlock()
	var nets map[string]NetSample
	if len(names) > 0 {
		// Expected even when unread: a sample that says nothing about the
		// namespaces would pass for a run without them.
		var err error
		if os.Geteuid() == 0 {
			t0 := time.Now()
			nets, err = netnsCounters(names, func(ns string) string { return "v" + ns })
			s.NetnsMs = float64(time.Since(t0).Microseconds()) / 1000
		} else {
			err = errors.New("entering a namespace needs root")
		}
		s.NetnsSampled, s.NetnsExpected = len(nets), len(names)
		if len(nets) > 0 {
			s.Netns = &NetSample{}
		}
		for _, n := range nets {
			s.Netns.add(n)
			s.NetnsMaxBacklog = max(s.NetnsMaxBacklog, n.QdiscBacklog)
		}
		if err != nil && !m.nsWarned {
			m.nsWarned = true
			fmt.Fprintf(os.Stderr, "hostmetrics: %d of %d namespaces unread: %v\n", len(names)-len(nets), len(names), err)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	dt := now.Sub(m.lastAt).Seconds()
	for idx, pid := range pids {
		cpuSec, rssMB, fds := procStats(pid)
		ns := NodeSample{Idx: idx, RSSMB: rssMB, FDs: fds, NetSample: nets[nsName(idx)]}
		if last, ok := m.lastCPU[idx]; ok && dt > 0 {
			ns.CPUPct = 100 * (cpuSec - last) / dt
		}
		m.lastCPU[idx] = cpuSec
		s.Nodes = append(s.Nodes, ns)
	}
	m.lastAt = now
	m.samples = append(m.samples, s)
}

// sampleENA reads the ENA counters into s. Only a host without an ENA device
// stops trying; any other failure is retried next sample and noted in this one.
func (m *monitor) sampleENA(s *Sample) {
	if m.enaOff {
		return
	}
	if m.ena == nil {
		e, err := openENA()
		if err == errNotENA {
			m.enaOff = true
			return
		}
		if err != nil {
			s.ENAError = err.Error()
			return
		}
		m.ena = e
	}
	v, err := m.ena.ena()
	if err != nil {
		s.ENAError = err.Error()
		m.closeENA()
		return
	}
	s.ENASample = v
}

func (m *monitor) closeENA() {
	if m.ena != nil {
		m.ena.Close()
		m.ena = nil
	}
}

// procStats reads /proc on Linux; elsewhere it asks ps.
func procStats(pid int) (cpuSec, rssMB float64, fds int) {
	if runtime.GOOS == "linux" {
		if b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
			f := strings.Fields(string(b[strings.LastIndexByte(string(b), ')')+2:]))
			if len(f) > 22 {
				ut, _ := strconv.ParseFloat(f[11], 64)
				st, _ := strconv.ParseFloat(f[12], 64)
				cpuSec = (ut + st) / 100
				rssPages, _ := strconv.ParseFloat(f[21], 64)
				rssMB = rssPages * float64(os.Getpagesize()) / (1 << 20)
			}
		}
		if es, err := os.ReadDir("/proc/" + strconv.Itoa(pid) + "/fd"); err == nil {
			fds = len(es)
		}
		return
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "time=,rss=").Output()
	if err != nil {
		return
	}
	f := strings.Fields(string(out))
	if len(f) == 2 {
		cpuSec = parseClock(f[0])
		kb, _ := strconv.ParseFloat(f[1], 64)
		rssMB = kb / 1024
	}
	return
}

func parseClock(s string) float64 { // [dd-]hh:mm:ss or mm:ss.ss
	var sec float64
	for _, p := range strings.Split(strings.ReplaceAll(s, "-", ":"), ":") {
		v, _ := strconv.ParseFloat(p, 64)
		sec = sec*60 + v
	}
	return sec
}

func hostLoad() (load1 float64, memAvailMB int64) {
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		load1, _ = strconv.ParseFloat(strings.Fields(string(b))[0], 64)
	}
	if f, err := os.Open("/proc/meminfo"); err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "MemAvailable:") {
				kb, _ := strconv.ParseInt(strings.Fields(sc.Text())[1], 10, 64)
				memAvailMB = kb / 1024
			}
		}
	}
	return
}

// udpErrors reads the Udp line of /proc/net/snmp: InErrors and RcvbufErrors,
// cumulative since boot. A rising RcvbufErrors means the kernel dropped
// datagrams because a socket buffer was full: the host, not the protocol.
func udpErrors() (rcvbuf, inErrors int64) {
	b, err := os.ReadFile("/proc/net/snmp")
	if err != nil {
		return
	}
	u := parseSnmp(b, "Udp")
	return u["RcvbufErrors"], u["InErrors"]
}
