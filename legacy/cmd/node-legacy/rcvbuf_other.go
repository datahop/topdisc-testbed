//go:build !linux

package main

import "net"

func udpRcvbuf(*net.UDPAddr) int { return 0 }

func probeRcvbuf() int { return 0 }
