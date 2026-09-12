package wan

import (
	"math/rand"
	"strings"
	"testing"
	"time"
)

func TestStarRTTDistribution(t *testing.T) {
	s := Star{MinMs: 4, MaxMs: 45, JitterMs: 3, RateKbps: 160}
	rng := rand.New(rand.NewSource(1))
	var sum time.Duration
	lo, hi := time.Hour, time.Duration(0)
	const n = 20000
	for i := 0; i < n; i++ {
		rtt := s.Delay(rng) + s.Delay(rng)
		sum += rtt
		if rtt < lo {
			lo = rtt
		}
		if rtt > hi {
			hi = rtt
		}
	}
	mean := sum / n
	if mean < 30*time.Millisecond || mean > 38*time.Millisecond {
		t.Fatalf("mean RTT %v, want ~34ms", mean)
	}
	if lo < 8*time.Millisecond || hi > 91*time.Millisecond {
		t.Fatalf("RTT range %v..%v, want within 8..91ms", lo, hi)
	}
	cmds := s.Commands("n42", "br0", "10.10.0.42/16", 17*time.Millisecond)
	if !strings.Contains(cmds[len(cmds)-1], "netem delay 17ms 3ms rate 160kbit") {
		t.Fatalf("netem line wrong: %s", cmds[len(cmds)-1])
	}
}
