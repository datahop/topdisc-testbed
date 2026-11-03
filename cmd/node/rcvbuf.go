package main

// rcvbufs are a node's UDP receive buffers, the raw getsockopt(SO_RCVBUF)
// value: Linux reports rmem_default as is for a socket that sets none, and
// 2*min(N, rmem_max) after SetReadBuffer(N). So 16 MiB is either a 16 MiB
// rmem_default or an 8 MiB request (go-ethereum#176), and Disc equal to
// Default does not tell the two apart.
type rcvbufs struct {
	Disc    int // the discovery socket's; 0 if it was not found or not unique
	Default int // a fresh socket's: what one that sets none gets
}
