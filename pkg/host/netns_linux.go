//go:build linux && !386

package host

import (
	"encoding/binary"
	"fmt"
	"os"
	"runtime"
	"syscall"
)

// netnsCounters reads a NetSample from each named namespace (iproute2's
// /var/run/netns/<name>), entering them in turn from one locked thread: a
// namespace's /proc/net is visible only from inside it, and a netlink socket
// sees the qdiscs of the namespace it was opened in. veth names the node's
// interface in a namespace. Needs CAP_SYS_ADMIN.
func netnsCounters(names []string, veth func(string) string) (map[string]NetSample, error) {
	type result struct {
		m   map[string]NetSample
		err error
	}
	ch := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		orig, err := syscall.Open("/proc/thread-self/ns/net", syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
		if err != nil {
			runtime.UnlockOSThread()
			ch <- result{nil, err}
			return
		}
		defer syscall.Close(orig)
		m := make(map[string]NetSample, len(names))
		buf := make([]byte, 64<<10)
		var first error
		for _, name := range names {
			s, err := readNetns(name, veth(name), buf)
			if err != nil {
				if first == nil {
					first = fmt.Errorf("netns %s: %w", name, err)
				}
				continue
			}
			m[name] = s
		}
		// A thread stuck in a node's namespace must not run other goroutines:
		// left locked, the runtime retires it when this goroutine returns.
		if setns(orig) == nil {
			runtime.UnlockOSThread()
		}
		ch <- result{m, first}
	}()
	r := <-ch
	return r.m, r.err
}

func setns(fd int) error {
	if _, _, e := syscall.RawSyscall(sysSetns, uintptr(fd), syscall.CLONE_NEWNET, 0); e != 0 {
		return e
	}
	return nil
}

func readNetns(name, veth string, buf []byte) (NetSample, error) {
	var s NetSample
	fd, err := syscall.Open("/var/run/netns/"+name, syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return s, err
	}
	err = setns(fd)
	syscall.Close(fd)
	if err != nil {
		return s, err
	}
	// A namespace read in part would show its counters falling to zero: it
	// counts as unread instead.
	b, err := os.ReadFile("/proc/thread-self/net/snmp")
	if err != nil {
		return s, err
	}
	u := parseSnmp(b, "Udp")
	if u == nil {
		return s, fmt.Errorf("no Udp row in snmp")
	}
	s.UDPRcvbufErrors, s.UDPSndbufErrors, s.UDPInErrors, s.UDPNoPorts = u["RcvbufErrors"], u["SndbufErrors"], u["InErrors"], u["NoPorts"]
	if b, err = os.ReadFile("/proc/thread-self/net/dev"); err != nil {
		return s, err
	}
	var ok bool
	if s.VethRxDropped, s.VethTxDropped, ok = parseNetDev(b, veth); !ok {
		return s, fmt.Errorf("no %s in net/dev", veth)
	}
	qs, err := dumpQdiscs(buf)
	if err != nil {
		return s, err
	}
	for _, q := range qs {
		s.QdiscDrops += int64(q.Drops)
		s.QdiscOverlimits += int64(q.Overlims)
		s.QdiscBacklog += int64(q.Backlog)
		s.QdiscQlen += int64(q.Qlen)
	}
	return s, nil
}

// dumpQdiscs lists the root qdiscs of the current thread's namespace.
func dumpQdiscs(buf []byte) ([]qdiscStats, error) {
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, syscall.NETLINK_ROUTE)
	if err != nil {
		return nil, err
	}
	defer syscall.Close(fd)
	tv := syscall.Timeval{Sec: 1}
	if err := syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv); err != nil {
		return nil, err
	}
	sa := &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}
	if err := syscall.Bind(fd, sa); err != nil {
		return nil, err
	}
	req := make([]byte, nlmsgHdrLen+tcmsgLen)
	ne := binary.NativeEndian
	ne.PutUint32(req, uint32(len(req)))
	ne.PutUint16(req[4:], syscall.RTM_GETQDISC)
	ne.PutUint16(req[6:], syscall.NLM_F_REQUEST|syscall.NLM_F_DUMP)
	ne.PutUint32(req[8:], 1)
	if err := syscall.Sendto(fd, req, 0, sa); err != nil {
		return nil, err
	}
	var all []qdiscStats
	for {
		n, _, err := syscall.Recvfrom(fd, buf, 0)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return all, err
		}
		qs, done, err := parseQdiscDump(buf[:n])
		all = append(all, qs...)
		if err != nil || done {
			return all, err
		}
	}
}
