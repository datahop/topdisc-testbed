package host

import (
	"bufio"
	"encoding/json"
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
}

type NodeSample struct {
	Idx    int     `json:"idx"`
	CPUPct float64 `json:"cpu_pct"` // of one core, since the previous sample
	RSSMB  float64 `json:"rss_mb"`
	FDs    int     `json:"fds"`
}

type monitor struct {
	mu      sync.Mutex
	samples []Sample
	lastCPU map[int]float64 // idx -> cumulative cpu seconds
	lastAt  time.Time
	stop    chan struct{}
}

// Monitor samples every period until Stop; samples are kept in memory and
// written by WriteSamples.
func (r *Runner) Monitor(period time.Duration) {
	r.init()
	r.mon = &monitor{lastCPU: map[int]float64{}, stop: make(chan struct{})}
	go func() {
		t := time.NewTicker(period)
		defer t.Stop()
		for {
			select {
			case <-r.mon.stop:
				return
			case <-t.C:
				r.sample()
			}
		}
	}()
}

func (r *Runner) StopMonitor() {
	if r.mon != nil {
		close(r.mon.stop)
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
	r.mu.Lock()
	pids := map[int]int{}
	for idx, c := range r.procs {
		if c.Process != nil && c.ProcessState == nil {
			pids[idx] = c.Process.Pid
		}
	}
	r.mu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	dt := now.Sub(m.lastAt).Seconds()
	for idx, pid := range pids {
		cpuSec, rssMB, fds := procStats(pid)
		ns := NodeSample{Idx: idx, RSSMB: rssMB, FDs: fds}
		if last, ok := m.lastCPU[idx]; ok && dt > 0 {
			ns.CPUPct = 100 * (cpuSec - last) / dt
		}
		m.lastCPU[idx] = cpuSec
		s.Nodes = append(s.Nodes, ns)
	}
	m.lastAt = now
	m.samples = append(m.samples, s)
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
	lines := strings.Split(string(b), "\n")
	for i := 0; i+1 < len(lines); i++ {
		if strings.HasPrefix(lines[i], "Udp:") && strings.HasPrefix(lines[i+1], "Udp:") {
			keys, vals := strings.Fields(lines[i])[1:], strings.Fields(lines[i+1])[1:]
			for j, k := range keys {
				if j < len(vals) {
					v, _ := strconv.ParseInt(vals[j], 10, 64)
					switch k {
					case "RcvbufErrors":
						rcvbuf = v
					case "InErrors":
						inErrors = v
					}
				}
			}
			return
		}
	}
	return
}
