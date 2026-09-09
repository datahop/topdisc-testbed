package main

import (
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/p2p/enode"
)

// connTable models peer slots the way geth's p2p server does: a fixed total
// capacity per node, split between dialed (outbound) and accepted (inbound)
// peers by the dial ratio. Slots are per node, not per topic, so a node
// searching for several topics shares one budget — as it would in practice.
//
// The point of modelling this is that a real node stops pulling nodes out of
// discovery once its slots are full. Without it a searcher consumes the
// iterator forever and topic search never settles, so a churn-free run has no
// steady state to measure.
type connTable struct {
	mu       sync.Mutex
	idxOf    map[enode.ID]int
	ids      []enode.ID
	out      []int
	in       []int
	peers    []map[int]struct{} // symmetric: connected either way
	outPeers []map[int]struct{} // directed: nodes this one dialed
	wake     []chan struct{}    // signalled when this node loses an outbound slot
	offline  []bool             // node is away for a churn session gap
	lostAt   [][]time.Time      // when each still-unfilled outbound slot was lost
	maxOut   int
	maxIn    int

	rejectedFull  int // dials refused because the target had no inbound slot
	dupDropped    int // discovered peers skipped because already connected either way
	disconnects   int // connections dropped by the disconnect driver
	departs       int // session-churn departures (nodes going offline)
	losses        int // outbound slots lost by a node that stays online, i.e. ones it must refill
	refills       int // outbound slots refilled after a disconnect
	refillTotalMs int64
}

func newConnTable(all []nodeRec, maxPeers, dialRatio int) *connTable {
	if dialRatio < 1 {
		dialRatio = 1
	}
	maxOut := maxPeers / dialRatio
	if maxOut < 1 {
		maxOut = 1
	}
	c := &connTable{
		idxOf:    make(map[enode.ID]int, len(all)),
		ids:      make([]enode.ID, len(all)),
		out:      make([]int, len(all)),
		in:       make([]int, len(all)),
		peers:    make([]map[int]struct{}, len(all)),
		outPeers: make([]map[int]struct{}, len(all)),
		wake:     make([]chan struct{}, len(all)),
		offline:  make([]bool, len(all)),
		lostAt:   make([][]time.Time, len(all)),
		maxOut:   maxOut,
		maxIn:    maxPeers - maxOut,
	}
	for i, n := range all {
		id := n.ln.ID()
		c.idxOf[id] = i
		c.ids[i] = id
		c.peers[i] = make(map[int]struct{}, maxPeers)
		c.outPeers[i] = make(map[int]struct{}, maxOut)
		c.wake[i] = make(chan struct{}, 1)
	}
	return c
}

// wakeCh is closed-over by a searcher that has filled its slots: a receive
// blocks until this node loses an outbound peer and needs to dial again.
func (c *connTable) wakeCh(idx int) <-chan struct{} { return c.wake[idx] }

// disconnect drops the connection from -> to, freeing an outbound slot on
// from and an inbound slot on to. Only the dialer is woken: a node whose
// inbound peer vanished does not dial a replacement, it just has room for
// another inbound, which mirrors geth.
func (c *connTable) disconnect(from, to int) bool { return c.dropConn(from, to, true) }

// dropConn removes the connection; wakeDialer is false when the dialer itself is
// the one leaving, so it does not try to refill while it is away.
func (c *connTable) dropConn(from, to int, wakeDialer bool) bool {
	c.mu.Lock()
	if _, ok := c.outPeers[from][to]; !ok {
		c.mu.Unlock()
		return false
	}
	delete(c.outPeers[from], to)
	delete(c.peers[from], to)
	delete(c.peers[to], from)
	c.out[from]--
	c.in[to]--
	c.disconnects++
	if wakeDialer {
		c.losses++
		c.lostAt[from] = append(c.lostAt[from], time.Now())
	}
	ch := c.wake[from]
	c.mu.Unlock()

	if !wakeDialer {
		return true
	}
	select {
	case ch <- struct{}{}:
	default: // already pending; the searcher will re-check its slot count
	}
	return true
}

// shouldPark reports whether the node has nothing to do: either it is away for a
// churn gap, or its outbound slots are full.
func (c *connTable) isOffline(idx int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.offline[idx]
}

func (c *connTable) shouldPark(idx int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.offline[idx] || c.out[idx] >= c.maxOut
}

// dropRandom disconnects up to count live connections chosen at random. Every
// node holds the same outbound budget in steady state, so picking a random
// node and then one of its outbound edges is close to uniform over edges.
func (c *connTable) dropRandom(rng *rand.Rand, count int) int {
	dropped := 0
	for attempts := 0; dropped < count && attempts < count*20; attempts++ {
		i := rng.Intn(len(c.out))
		c.mu.Lock()
		var target = -1
		for j := range c.outPeers[i] {
			target = j
			break
		}
		c.mu.Unlock()
		if target >= 0 && c.disconnect(i, target) {
			dropped++
		}
	}
	return dropped
}

// runDisconnectDriver drops frac of all live connections every interval,
// modelling transient link failure with no node leaving the network.
func runDisconnectDriver(c *connTable, interval time.Duration, frac float64, seed int64, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	if interval <= 0 || frac <= 0 {
		return
	}
	rng := rand.New(rand.NewSource(seed))
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			c.mu.Lock()
			live := 0
			for _, v := range c.out {
				live += v
			}
			c.mu.Unlock()
			n := int(float64(live) * frac)
			if n < 1 {
				n = 1
			}
			got := c.dropRandom(rng, n)
			fmt.Printf("[disconnect] dropped %d of %d live connections\n", got, live)
		}
	}
}

// dial attempts an outbound connection from -> to. It reports whether a new
// connection was established, and whether the attempt failed specifically
// because the destination had no inbound capacity left.
func (c *connTable) dial(from int, to enode.ID) (ok, targetFull bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	j, known := c.idxOf[to]
	if !known || j == from || c.offline[from] {
		return false, false
	}
	if _, dup := c.peers[from][j]; dup {
		c.dupDropped++
		return false, false
	}
	if c.out[from] >= c.maxOut {
		return false, false
	}
	if c.offline[j] || c.in[j] >= c.maxIn {
		c.rejectedFull++
		return false, true
	}
	c.out[from]++
	c.in[j]++
	c.peers[from][j] = struct{}{}
	c.peers[j][from] = struct{}{}
	c.outPeers[from][j] = struct{}{}
	if q := c.lostAt[from]; len(q) > 0 {
		c.refills++
		c.refillTotalMs += time.Since(q[0]).Milliseconds()
		c.lostAt[from] = q[1:]
	}
	return true, false
}

func (c *connTable) outboundFull(idx int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.out[idx] >= c.maxOut
}

// report prints the outbound/inbound distributions plus inbound load across
// ID space, which is where topic-centre saturation shows up.
func (c *connTable) report() {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := append([]int(nil), c.out...)
	in := append([]int(nil), c.in...)
	sort.Ints(out)
	sort.Ints(in)

	filled, saturated := 0, 0
	for _, v := range c.out {
		if v >= c.maxOut {
			filled++
		}
	}
	for _, v := range c.in {
		if v >= c.maxIn {
			saturated++
		}
	}

	fmt.Printf("=== connection model ===\n")
	fmt.Printf("slots: max-out=%d max-in=%d nodes=%d\n", c.maxOut, c.maxIn, len(c.out))
	fmt.Printf("outbound: p5=%d p50=%d p95=%d max=%d; filled=%d/%d (%.1f%%)\n",
		pctInt(out, 5), pctInt(out, 50), pctInt(out, 95), maxOrZero(out),
		filled, len(out), 100*float64(filled)/float64(len(out)))
	fmt.Printf("inbound: p5=%d p50=%d p95=%d max=%d; saturated=%d/%d (%.1f%%); dials-refused=%d\n",
		pctInt(in, 5), pctInt(in, 50), pctInt(in, 95), maxOrZero(in),
		saturated, len(in), 100*float64(saturated)/float64(len(in)), c.rejectedFull)
	fmt.Printf("already-connected discoveries dropped: %d\n", c.dupDropped)
	if c.disconnects > 0 {
		mean := 0.0
		if c.refills > 0 {
			mean = float64(c.refillTotalMs) / float64(c.refills) / 1000
		}
		fmt.Printf("disconnects=%d departs=%d slots-lost=%d refilled=%d (%.1f%%) mean-refill=%.1fs\n",
			c.disconnects, c.departs, c.losses, c.refills, 100*float64(c.refills)/float64(max(c.losses, 1)), mean)
	}

	buckets := make([]int, overheadBuckets)
	counts := make([]int, overheadBuckets)
	for i, id := range c.ids {
		b := idBucket(id)
		buckets[b] += c.in[i]
		counts[b]++
	}
	fmt.Printf("inbound-by-idspace:")
	for b := range buckets {
		mean := 0.0
		if counts[b] > 0 {
			mean = float64(buckets[b]) / float64(counts[b])
		}
		fmt.Printf(" %.1f", mean)
	}
	fmt.Printf("\n")
}

// recentlyDialed reports whether this node was dialed within the cooldown,
// mirroring the dial history geth keeps for dialHistoryExpiration.
func recentlyDialed(last map[enode.ID]time.Time, id enode.ID, wait time.Duration) bool {
	if wait <= 0 {
		return false
	}
	t, ok := last[id]
	return ok && time.Since(t) < wait
}

func pctInt(sorted []int, p int) int {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[(p*(len(sorted)-1))/100]
}

// Session churn drawn from a measured discv5 crawl: the population is bimodal,
// with 42.3% of sessions outlasting the observation window and the rest falling
// off geometrically from a mode at the resolution floor. Departures here are
// topology-level — a node drops every connection and stops accepting dials for
// a gap, then returns and refills — so peers must rediscover rather than the
// discv5 node itself being torn down.

var sessionCDF = [][2]float64{
	{0.3214, 0.01190}, {0.5142, 0.03571}, {0.5684, 0.04762}, {0.6096, 0.05952},
	{0.6445, 0.07143}, {0.6717, 0.08333}, {0.6996, 0.09524}, {0.7215, 0.10714},
	{0.7449, 0.11905}, {0.7710, 0.13095}, {0.8066, 0.15476}, {0.8353, 0.17857},
	{0.8755, 0.22619}, {0.9060, 0.27381}, {0.9368, 0.32143}, {0.9513, 0.34524},
	{0.9716, 0.42857}, {0.9902, 0.59524}, {1.0000, 0.95238},
}

// sessionLength decides once whether a node churns at all, and if so samples
// its first session length as a fraction of the window.
func sessionLength(rng *rand.Rand, alwaysOn, scale float64) (float64, bool) {
	if rng.Float64() < alwaysOn {
		return 0, false
	}
	return churnerSession(rng) * scale, true
}

// churnerSession samples a session length for a node already known to churn.
func churnerSession(rng *rand.Rand) float64 {
	u := rng.Float64()
	prev := 0.0
	for i, p := range sessionCDF {
		if u <= p[0] {
			lo := 0.0
			if i > 0 {
				lo = sessionCDF[i-1][1]
			}
			span := p[0] - prev
			if span <= 0 {
				return p[1]
			}
			return lo + (p[1]-lo)*(u-prev)/span
		}
		prev = p[0]
	}
	return sessionCDF[len(sessionCDF)-1][1]
}

// depart drops every connection the node holds in either direction and marks it
// unreachable. Peers that dialed it are woken so they replace it.
func (c *connTable) depart(i int) {
	c.mu.Lock()
	dialed := make([]int, 0, len(c.outPeers[i]))
	dialers := make([]int, 0, len(c.peers[i]))
	for j := range c.peers[i] {
		if _, out := c.outPeers[i][j]; out {
			dialed = append(dialed, j)
		} else {
			dialers = append(dialers, j)
		}
	}
	c.offline[i] = true
	c.departs++
	c.lostAt[i] = nil // a returning node fills from empty; that is not a refill
	c.mu.Unlock()

	for _, j := range dialed {
		c.dropConn(i, j, false)
	}
	for _, j := range dialers {
		c.disconnect(j, i)
	}
}

// rejoin makes the node reachable again and wakes it so it refills its slots.
func (c *connTable) rejoin(i int) {
	c.mu.Lock()
	c.offline[i] = false
	ch := c.wake[i]
	c.mu.Unlock()
	select {
	case ch <- struct{}{}:
	default:
	}
}

// runSessionChurn gives every node a session length and cycles it offline and
// back for the duration of the run.
func runSessionChurn(c *connTable, window, gap time.Duration, alwaysOn, scale float64, seed int64, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)

	if !waitForTopology(c, stop) {
		return
	}
	fmt.Printf("[session-churn] started; window=%s gap=%s always-on=%.3f scale=%.2f\n", window, gap, alwaysOn, scale)
	go logChurnProgress(c, stop)

	rng := rand.New(rand.NewSource(seed))
	var wg sync.WaitGroup
	for i := range c.out {
		frac, churns := sessionLength(rng, alwaysOn, scale)
		if !churns {
			continue
		}
		wg.Add(1)
		go func(idx int, first time.Duration, r *rand.Rand) {
			defer wg.Done()
			t := time.NewTimer(first)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
				}
				c.depart(idx)
				g := time.NewTimer(gap)
				select {
				case <-stop:
					g.Stop()
					return
				case <-g.C:
				}
				c.rejoin(idx)
				t.Reset(time.Duration(churnerSession(r) * scale * float64(window)))
			}
		}(i, time.Duration(frac*float64(window)), rand.New(rand.NewSource(seed+int64(i)*2654435761)))
	}
	<-stop
	wg.Wait()
}

// waitForTopology blocks until the first connection exists, so churn timers
// do not start ticking during registration on an empty topology.
func waitForTopology(c *connTable, stop <-chan struct{}) bool {
	for {
		c.mu.Lock()
		live := 0
		for _, v := range c.out {
			live += v
		}
		c.mu.Unlock()
		if live > 0 {
			return true
		}
		select {
		case <-stop:
			return false
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func logChurnProgress(c *connTable, stop <-chan struct{}) {
	tick := time.NewTicker(60 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			c.mu.Lock()
			fmt.Printf("[session-churn] departs=%d disconnects=%d slots-lost=%d refilled=%d\n", c.departs, c.disconnects, c.losses, c.refills)
			c.mu.Unlock()
		}
	}
}
