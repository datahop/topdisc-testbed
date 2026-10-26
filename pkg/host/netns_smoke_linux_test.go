//go:build linux && !386

package host

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/datahop/topdisc-testbed/pkg/wan"
)

// TestNetnsSmoke builds the namespaces Prepare does (bridge, veth, netem at
// 1 Mbit/s) and checks the monitor attributes a flood to the sender's qdisc
// and the receiver's socket buffer. It needs root; without sudo:
//
//	go test -c -o /tmp/host.test ./pkg/host
//	unshare -rnm sh -c 'mount -t tmpfs none /run && TB_NETNS_SMOKE=500 /tmp/host.test -test.run NetnsSmoke -test.v'
func TestNetnsSmoke(t *testing.T) {
	n, _ := strconv.Atoi(os.Getenv("TB_NETNS_SMOKE"))
	if n < 2 || os.Geteuid() != 0 {
		t.Skip("set TB_NETNS_SMOKE=<namespaces> and run as root (see the comment)")
	}
	star := &wan.Star{MinMs: 4, MaxMs: 45, RateKbps: 1000}
	cmds := []string{fmt.Sprintf("ip link add %s type bridge", Bridge), fmt.Sprintf("ip link set %s up", Bridge)}
	r := &Runner{Wan: star}
	r.init()
	for k := range n {
		r.delay[k] = 10 * time.Millisecond
		cmds = append(cmds, star.Commands(nsName(k), Bridge, NodeIP(0, k)+"/16", r.delay[k])...)
	}
	t0 := time.Now()
	if err := root(cmds); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d namespaces built in %v", n, time.Since(t0).Round(time.Millisecond))

	// Node 0 floods node 1, which never reads and has the smallest buffer.
	// One socket's send buffer holds fewer packets than netem's limit of
	// 1000, so it takes several to overflow the qdisc.
	var tx [8]int
	var rx int
	for i := range tx {
		inNetns(t, nsName(0), func() (err error) { tx[i], err = udpSocket(0, 0); return })
		defer syscall.Close(tx[i])
	}
	inNetns(t, nsName(1), func() (err error) { rx, err = udpSocket(9000, 1); return })
	defer syscall.Close(rx)
	dst := &syscall.SockaddrInet4{Port: 9000, Addr: [4]byte{10, 100, 0, 2}}
	pkt := make([]byte, 1200)
	syscall.Sendto(tx[0], pkt, 0, dst) // resolve the neighbour first: packets queued behind ARP are dropped past 100
	time.Sleep(200 * time.Millisecond)
	for i := range 3000 {
		syscall.Sendto(tx[i%len(tx)], pkt, syscall.MSG_DONTWAIT, dst)
	}
	time.Sleep(2 * time.Second)
	out, _ := exec.Command("tc", "-n", nsName(0), "-s", "qdisc", "show", "dev", "v"+nsName(0)).CombinedOutput()
	t.Logf("tc: %s", out)

	r.mon = &monitor{lastCPU: map[int]float64{}}
	r.sample()
	s := r.mon.samples[0]
	if s.NetnsSampled != n || s.NetnsExpected != n || s.Netns == nil {
		t.Fatalf("sampled %d of %d namespaces", s.NetnsSampled, n)
	}
	got := map[int]NetSample{}
	for k := range 2 {
		ns, err := netnsCounters([]string{nsName(k)}, func(ns string) string { return "v" + ns })
		if err != nil {
			t.Fatal(err)
		}
		got[k] = ns[nsName(k)]
		t.Logf("node %d: %+v", k, got[k])
	}
	if got[0].QdiscDrops == 0 || got[0].QdiscBacklog == 0 || got[0].UDPSndbufErrors == 0 {
		t.Errorf("sender: want qdisc drops, backlog and sndbuf errors, got %+v", got[0])
	}
	if got[1].UDPRcvbufErrors == 0 || got[1].QdiscDrops != 0 {
		t.Errorf("receiver: want rcvbuf errors and no qdisc drops, got %+v", got[1])
	}
	if s.Netns.QdiscDrops < got[0].QdiscDrops || s.NetnsMaxBacklog == 0 {
		t.Errorf("host sum %+v (max backlog %d) misses node 0", *s.Netns, s.NetnsMaxBacklog)
	}

	const passes = 5
	t0 = time.Now()
	var cpu0 syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &cpu0)
	for range passes {
		r.sample()
	}
	var cpu1 syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &cpu1)
	cpu := time.Duration(cpu1.Utime.Nano()+cpu1.Stime.Nano()-cpu0.Utime.Nano()-cpu0.Stime.Nano()) / passes
	t.Logf("one pass over %d namespaces: %v wall, %v cpu (netns_ms %.1f)", n, time.Since(t0)/passes, cpu, r.mon.samples[len(r.mon.samples)-1].NetnsMs)
}

func inNetns(t *testing.T, name string, f func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		orig, err := syscall.Open("/proc/thread-self/ns/net", syscall.O_RDONLY, 0)
		if err != nil {
			done <- err
			return
		}
		defer syscall.Close(orig)
		fd, err := syscall.Open("/var/run/netns/"+name, syscall.O_RDONLY, 0)
		if err != nil {
			done <- err
			return
		}
		err = setns(fd)
		syscall.Close(fd)
		if err != nil {
			done <- err
			return
		}
		err = f()
		if setns(orig) == nil {
			runtime.UnlockOSThread()
		}
		done <- err
	}()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func udpSocket(port, rcvbuf int) (int, error) {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return -1, err
	}
	if rcvbuf > 0 {
		syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_RCVBUF, rcvbuf)
	}
	return fd, syscall.Bind(fd, &syscall.SockaddrInet4{Port: port})
}
