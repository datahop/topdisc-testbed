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

type Runner struct {
	NodeBinary string
	Verbosity  int
	AsgDir     string // node<idx>.json per node
	LogDir     string
	Wan        *wan.Star // nil: plain processes on the host's own address
	Host       int       // this host's index (subnet)
	Seed       int64

	mu    sync.Mutex
	procs map[int]*exec.Cmd
	delay map[int]time.Duration
}

func (r *Runner) init() {
	if r.procs == nil {
		r.procs, r.delay = map[int]*exec.Cmd{}, map[int]time.Duration{}
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
	if r.Wan == nil {
		return nil
	}
	if runtime.GOOS != "linux" {
		return fmt.Errorf("wan emulation needs Linux netns")
	}
	cmds := []string{
		"sysctl -q -w net.ipv4.ip_forward=1",
		fmt.Sprintf("ip link add %s type bridge", Bridge),
		fmt.Sprintf("ip addr add %s/16 dev %s", BridgeAddr(r.Host), Bridge),
		fmt.Sprintf("ip link set %s up", Bridge),
	}
	for h, addr := range peers {
		if h != r.Host {
			cmds = append(cmds, fmt.Sprintf("ip route replace %s via %s", Subnet(h), addr))
		}
	}
	rng := rand.New(rand.NewSource(r.Seed + int64(r.Host)))
	for k, a := range as {
		d := r.Wan.Delay(rng)
		r.delay[a.Idx] = d
		ns := nsName(a.Idx)
		cmds = append(cmds, r.Wan.Commands(ns, Bridge, NodeIP(r.Host, k)+"/16", d)...)
		cmds = append(cmds, fmt.Sprintf("ip -n %s route add default via %s", ns, BridgeAddr(r.Host)))
	}
	return root(cmds)
}

// Start launches node idx (again, with the same identity, if it ran before).
func (r *Runner) Start(idx int) error {
	r.init()
	logf, err := os.OpenFile(filepath.Join(r.LogDir, fmt.Sprintf("node%d.log", idx)), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	args := []string{r.NodeBinary, "-assignment", filepath.Join(r.AsgDir, fmt.Sprintf("node%d.json", idx)), "-v", strconv.Itoa(r.Verbosity)}
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

func root(cmds []string) error {
	for _, c := range cmds {
		a := rootArgs(strings.Fields(c)...)
		if out, err := exec.Command(a[0], a[1:]...).CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %v: %s", c, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}
