package main

import (
	"context"
	"net"
	"syscall"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
)

func listenUDP(t *testing.T, addr string, readBuf int, reusePort bool) *net.UDPConn {
	t.Helper()
	lc := net.ListenConfig{}
	if reusePort {
		lc.Control = func(_, _ string, c syscall.RawConn) error {
			return c.Control(func(fd uintptr) { syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, 0xf /* SO_REUSEPORT */, 1) })
		}
	}
	pc, err := lc.ListenPacket(context.Background(), "udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	c := pc.(*net.UDPConn)
	t.Cleanup(func() { c.Close() })
	if err := c.SetReadBuffer(readBuf); err != nil {
		t.Fatal(err)
	}
	return c
}

func sockRcvbuf(t *testing.T, c *net.UDPConn) int {
	t.Helper()
	rc, _ := c.SyscallConn()
	v := 0
	rc.Control(func(fd uintptr) { v, _ = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF) })
	return v
}

func at(ip string, port int) *net.UDPAddr { return &net.UDPAddr{IP: net.ParseIP(ip), Port: port} }

func TestUDPRcvbuf(t *testing.T) {
	def := probeRcvbuf()
	if def <= 0 {
		t.Fatalf("probe SO_RCVBUF = %d", def)
	}
	for _, ip := range []string{"127.0.0.1", "0.0.0.0"} {
		c := listenUDP(t, ip+":0", 65536, false)
		want := sockRcvbuf(t, c)
		if want == def {
			t.Fatalf("socket buffer %d equals the default; the test cannot tell them apart", want)
		}
		port := c.LocalAddr().(*net.UDPAddr).Port
		if got := udpRcvbuf(at(ip, port)); got != want {
			t.Errorf("%s: udpRcvbuf = %d, want the bound socket's %d", ip, got, want)
		}
	}
}

func TestUDPRcvbufNotFound(t *testing.T) {
	c := listenUDP(t, "127.0.0.1:0", 65536, false)
	port := c.LocalAddr().(*net.UDPAddr).Port
	if got := udpRcvbuf(at("127.0.0.3", port)); got != 0 {
		t.Errorf("other address: udpRcvbuf = %d, want 0 (unknown), not the probe default %d", got, probeRcvbuf())
	}
}

func TestUDPRcvbufMatchesAddress(t *testing.T) {
	a := listenUDP(t, "127.0.0.1:0", 65536, false)
	port := a.LocalAddr().(*net.UDPAddr).Port
	b := listenUDP(t, at("127.0.0.2", port).String(), 262144, false)
	for ip, c := range map[string]*net.UDPConn{"127.0.0.1": a, "127.0.0.2": b} {
		if got, want := udpRcvbuf(at(ip, port)), sockRcvbuf(t, c); got != want {
			t.Errorf("%s: udpRcvbuf = %d, want %d", ip, got, want)
		}
	}
}

func TestUDPRcvbufAmbiguous(t *testing.T) {
	a := listenUDP(t, "127.0.0.1:0", 65536, true)
	port := a.LocalAddr().(*net.UDPAddr).Port
	listenUDP(t, at("127.0.0.1", port).String(), 262144, true)
	if got := udpRcvbuf(at("127.0.0.1", port)); got != 0 {
		t.Errorf("two sockets on one address: udpRcvbuf = %d, want 0", got)
	}
}

func TestUpRecordsRcvbuf(t *testing.T) {
	key, _ := crypto.GenerateKey()
	n := newNode(assignment{IP: "127.0.0.1", Port: freePort(t), Topics: []string{"t"}}, key)
	if err := n.up(); err != nil {
		t.Fatal(err)
	}
	defer n.down()
	m := n.trace()
	if d, def := m["udp_rcvbuf_bytes"].(int), m["udp_rcvbuf_default_bytes"].(int); d <= 0 || def <= 0 {
		t.Errorf("udp_rcvbuf_bytes %d udp_rcvbuf_default_bytes %d, want both measured", d, def)
	}
}

func freePort(t *testing.T) int {
	c := listenUDP(t, "127.0.0.1:0", 65536, false)
	port := c.LocalAddr().(*net.UDPAddr).Port
	c.Close()
	return port
}
