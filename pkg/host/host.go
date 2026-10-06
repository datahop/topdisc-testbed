// Package host runs node processes on one machine, optionally each in its own
// network namespace shaped by netem. The local backend uses it in-process; the
// cloud backend drives one per host through cmd/hostagent.
package host

import (
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/datahop/topdisc-testbed/pkg/assign"
	"github.com/datahop/topdisc-testbed/pkg/wan"
)

const Bridge = "tbbr0"

// Subnet of host h is 10.(100+h).0.0/16; node with local index k gets
// address k+1 in it, the bridge .255.254. Hosts route each other's /16.
func Subnet(h int) string     { return fmt.Sprintf("10.%d.0.0/16", 100+h) }
func BridgeAddr(h int) string { return fmt.Sprintf("10.%d.255.254", 100+h) }
func NodeIP(h, k int) string  { return fmt.Sprintf("10.%d.%d.%d", 100+h, (k+1)>>8, (k+1)&255) }
func nsName(idx int) string   { return "tb" + strconv.Itoa(idx) }

// Route is one prefix reachable through another machine. A run on the
// synthetic addressing needs one per machine; a run on crawl addresses needs
// one per /24, because its nodes are scattered across the address space.
type Route struct {
	Prefix string `json:"prefix"`
	Via    string `json:"via"`
}

type Runner struct {
	NodeBinary   string
	LegacyBinary string // for assignments with Legacy set
	Verbosity    int
	AsgDir       string // node<idx>.json per node
	LogDir       string
	Wan          *wan.Star // nil: plain processes on the host's own address
	Host         int       // this host's index (subnet)
	Seed         int64
	// Routes and RealAddrs come from the coordinator for a run on crawl
	// addresses: each node keeps the address the crawl gave it, so the
	// per-/24 limits and the ad cache's ipTree score see real diversity.
	Routes    []Route
	RealAddrs bool

	mu     sync.Mutex
	procs  map[int]*exec.Cmd
	delay  map[int]time.Duration
	legacy map[int]bool
	mon    *monitor
}

func (r *Runner) init() {
	if r.procs == nil {
		r.procs, r.delay, r.legacy = map[int]*exec.Cmd{}, map[int]time.Duration{}, map[int]bool{}
	}
}

// Prepare writes assignments and, with WAN emulation, creates the bridge and
// one netns per node. peers maps other hosts' indices to their addresses so
// their subnets are routed.
func (r *Runner) Prepare(as []assign.Assignment, peers map[int]string) error {
	r.init()
	for _, d := range []string{r.AsgDir, r.LogDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	if err := assign.Write(r.AsgDir, as); err != nil {
		return err
	}
	for _, a := range as {
		if a.Legacy {
			if r.LegacyBinary == "" {
				return fmt.Errorf("node %d is legacy but no legacy binary is configured", a.Idx)
			}
			r.legacy[a.Idx] = true
		}
	}
	if r.Wan == nil {
		return nil
	}
	if runtime.GOOS != "linux" {
		return fmt.Errorf("wan emulation needs Linux netns")
	}
	cmds := []string{
		"sysctl -q -w net.ipv4.ip_forward=1",
		// The host keeps one neighbour entry per node on the bridge, and the
		// defaults (512 soft, 1024 hard) sit below the node counts this runs
		// at. Overflowing the table evicts entries indiscriminately -- the
		// host's own default gateway among them -- and the machine drops off
		// the network mid-run while its BMC still reports it alive.
		"sysctl -q -w net.ipv4.neigh.default.gc_thresh1=8192",
		"sysctl -q -w net.ipv4.neigh.default.gc_thresh2=32768",
		"sysctl -q -w net.ipv4.neigh.default.gc_thresh3=65536",
		fmt.Sprintf("ip link add %s type bridge", Bridge),
		fmt.Sprintf("ip addr add %s/16 dev %s", BridgeAddr(r.Host), Bridge),
		fmt.Sprintf("ip link set %s up", Bridge),
	}
	if r.RealAddrs {
		// A node's address no longer tells you which machine it is on, so
		// reverse-path filtering has nothing to check it against. Debian
		// ships this off, but a kadeploy image that turned it on would drop
		// every packet between namespaces without a word.
		cmds = append(cmds,
			"sysctl -q -w net.ipv4.conf.all.rp_filter=0",
			"sysctl -q -w net.ipv4.conf.default.rp_filter=0")
	}
	rng := rand.New(rand.NewSource(r.Seed + int64(r.Host)))
	var routes []string
	for k, a := range as {
		d := r.Wan.Delay(rng)
		r.delay[a.Idx] = d
		ns := nsName(a.Idx)
		if r.RealAddrs {
			// The address is a /32 out of a foreign /24, so nothing is on-link
			// in either direction: the namespace reaches the bridge through an
			// explicit host route and everything else through it, and the
			// machine reaches the namespace through a host route on the bridge,
			// which is what makes it ARP there and the namespace answer.
			veth := "v" + ns
			cmds = append(cmds, r.Wan.Commands(ns, Bridge, a.IP+"/32", d)...)
			cmds = append(cmds,
				fmt.Sprintf("ip -n %s route add %s/32 dev %s", ns, BridgeAddr(r.Host), veth),
				fmt.Sprintf("ip -n %s route add default via %s", ns, BridgeAddr(r.Host)))
			routes = append(routes, fmt.Sprintf("route replace %s/32 dev %s", a.IP, Bridge))
			continue
		}
		cmds = append(cmds, r.Wan.Commands(ns, Bridge, NodeIP(r.Host, k)+"/16", d)...)
		cmds = append(cmds, fmt.Sprintf("ip -n %s route add default via %s", ns, BridgeAddr(r.Host)))
	}
	if r.RealAddrs {
		for _, rt := range r.Routes {
			routes = append(routes, fmt.Sprintf("route replace %s via %s", rt.Prefix, rt.Via))
		}
	} else {
		for h, addr := range peers {
			if h != r.Host {
				routes = append(routes, fmt.Sprintf("route replace %s via %s", Subnet(h), addr))
			}
		}
	}
	if err := root(cmds); err != nil {
		return err
	}
	// One `ip` per route costs a process each: at ~13k routes per machine on
	// crawl addresses that is minutes of forking before the run can start.
	return ipBatch(routes)
}

// Start launches node idx (again, with the same identity, if it ran before).
func (r *Runner) Start(idx int) error {
	r.init()
	logf, err := os.OpenFile(filepath.Join(r.LogDir, fmt.Sprintf("node%d.log", idx)), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	// The child gets its own descriptor at Start, so the parent's copy is dead
	// weight: holding it kept one fd per node (and one more per churn restart)
	// open for the life of the agent, which caps vnodes_per_machine at the
	// agent's own file limit.
	defer logf.Close()
	bin := r.NodeBinary
	if r.legacy[idx] {
		bin = r.LegacyBinary
	}
	args := []string{bin, "-assignment", filepath.Join(r.AsgDir, fmt.Sprintf("node%d.json", idx)), "-v", strconv.Itoa(r.Verbosity)}
	if r.Wan != nil {
		args = append(rootArgs("ip", "netns", "exec", nsName(idx)), args...)
	}
	c := exec.Command(args[0], args[1:]...)
	c.Stdout, c.Stderr = logf, logf
	if err := c.Start(); err != nil {
		return fmt.Errorf("node %d: %w", idx, err)
	}
	r.mu.Lock()
	r.procs[idx] = c
	r.mu.Unlock()
	return nil
}

func (r *Runner) Kill(idx int) bool {
	r.mu.Lock()
	c := r.procs[idx]
	delete(r.procs, idx)
	r.mu.Unlock()
	if c == nil || c.Process == nil {
		return false
	}
	c.Process.Kill()
	c.Wait()
	return true
}

func (r *Runner) StopAll() {
	r.mu.Lock()
	idx := make([]int, 0, len(r.procs))
	for i := range r.procs {
		idx = append(idx, i)
	}
	r.mu.Unlock()
	for _, i := range idx {
		r.Kill(i)
	}
	if r.Wan != nil {
		var cmds []string
		for i := range r.delay {
			cmds = append(cmds, wan.Teardown(nsName(i))...)
		}
		root(append(cmds, fmt.Sprintf("ip link del %s", Bridge)))
	}
}

// Delays reports the one-way delay assigned to each node (for the trace).
func (r *Runner) Delays() map[int]time.Duration { return r.delay }

func rootArgs(args ...string) []string {
	if os.Geteuid() == 0 {
		return args
	}
	return append([]string{"sudo", "-n"}, args...)
}

// ipBatch feeds many route commands to a single `ip -batch -`.
func ipBatch(cmds []string) error {
	if len(cmds) == 0 {
		return nil
	}
	a := rootArgs("ip", "-batch", "-")
	c := exec.Command(a[0], a[1:]...)
	c.Stdin = strings.NewReader(strings.Join(cmds, "\n") + "\n")
	if out, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("ip -batch (%d routes): %v: %s", len(cmds), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func root(cmds []string) error {
	for _, c := range cmds {
		a := rootArgs(strings.Fields(c)...)
		if out, err := exec.Command(a[0], a[1:]...).CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %v: %s", c, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}
