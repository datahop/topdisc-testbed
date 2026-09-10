// Package wan emulates WAN conditions per node. The star model gives each node
// one delay to a virtual core, so a pair's RTT is the sum of the two; drawing
// one-way delays from 4–45 ms (mean 17) reproduces RTTs of 8–91 ms, mean 34,
// with one qdisc per node and no per-pair state.
package wan

import (
	"fmt"
	"math"
	"math/rand"
	"time"
)

type Star struct {
	MinMs, MaxMs float64 // one-way delay range per node
	JitterMs     float64
	RateKbps     int // per-node cap; 0 = unshaped
}

func (s Star) Delay(rng *rand.Rand) time.Duration {
	// Log-uniform: mean (max-min)/ln(max/min) = 16.9 ms for 4..45, matching
	// the measured mean while keeping the tail.
	d := s.MinMs * math.Exp(rng.Float64()*math.Log(s.MaxMs/s.MinMs))
	return time.Duration(d * float64(time.Millisecond))
}

// Commands returns the shell needed to give node <name> its own network stack
// on host bridge <bridge> with address <cidr>, shaped by <delay>. Run as root.
func (s Star) Commands(name, bridge, cidr string, delay time.Duration) []string {
	veth, peer := "v"+name, "h"+name
	c := []string{
		fmt.Sprintf("ip netns add %s", name),
		fmt.Sprintf("ip link add %s type veth peer name %s", veth, peer),
		fmt.Sprintf("ip link set %s netns %s", veth, name),
		fmt.Sprintf("ip link set %s master %s up", peer, bridge),
		fmt.Sprintf("ip -n %s addr add %s dev %s", name, cidr, veth),
		fmt.Sprintf("ip -n %s link set lo up", name),
		fmt.Sprintf("ip -n %s link set %s up", name, veth),
	}
	netem := fmt.Sprintf("ip netns exec %s tc qdisc add dev %s root netem delay %dms %dms", name, veth, delay.Milliseconds(), int(s.JitterMs))
	if s.RateKbps > 0 {
		netem += fmt.Sprintf(" rate %dkbit", s.RateKbps)
	}
	return append(c, netem)
}

func Teardown(name string) []string { return []string{fmt.Sprintf("ip netns del %s", name)} }
