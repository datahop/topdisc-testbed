package main

import (
	"net"
	"os"
	"strconv"
	"syscall"
)

// udpRcvbuf returns SO_RCVBUF of this process's UDP socket bound to addr, or
// 0 if there is none or more than one.
func udpRcvbuf(addr *net.UDPAddr) int {
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return 0
	}
	want := addr.IP
	if want == nil {
		want = net.IPv6unspecified
	}
	v, matches := 0, 0
	for _, e := range ents {
		fd, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if typ, err := syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_TYPE); err != nil || typ != syscall.SOCK_DGRAM {
			continue
		}
		var ip net.IP
		var port int
		switch sa, _ := syscall.Getsockname(fd); a := sa.(type) {
		case *syscall.SockaddrInet4:
			ip, port = net.IP(a.Addr[:]), a.Port
		case *syscall.SockaddrInet6:
			ip, port = net.IP(a.Addr[:]), a.Port
		default:
			continue
		}
		if port != addr.Port || !(ip.Equal(want) || ip.IsUnspecified() && want.IsUnspecified()) {
			continue
		}
		if b, err := syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_RCVBUF); err == nil {
			v, matches = b, matches+1
		}
	}
	if matches != 1 {
		return 0
	}
	return v
}

// probeRcvbuf returns SO_RCVBUF of a fresh UDP socket: the host default.
func probeRcvbuf() int {
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return 0
	}
	defer c.Close()
	rc, err := c.SyscallConn()
	if err != nil {
		return 0
	}
	v := 0
	rc.Control(func(fd uintptr) { v, _ = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF) })
	return v
}
