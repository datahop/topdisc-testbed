package main

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/p2p/enode"
)

// deadBucket is the time resolution of the dead-result series.
const deadBucket = 10 * time.Second

// deadResults is the dead-result snapshot written to metrics.json; nil when the
// run has no churn.
var deadResults any

// deadResultTracker measures how often topic searches return registrants that
// are offline at the moment they are returned, and how long they have been
// offline. Every returned result counts, including a registrant returned again,
// and a node that comes back is live again.
type deadResultTracker struct {
	start      time.Time
	adLifetime time.Duration
	offlineAt  sync.Map // enode.ID -> time.Time

	mu     sync.Mutex
	topics map[int]*deadLocal
}

func newDeadResultTracker(adLifetime time.Duration) *deadResultTracker {
	if adLifetime <= 0 {
		adLifetime = 15 * time.Minute
	}
	return &deadResultTracker{start: time.Now(), adLifetime: adLifetime, topics: make(map[int]*deadLocal)}
}

// begin sets the start of the time series; call it when searches start.
func (d *deadResultTracker) begin() { d.start = time.Now() }

// markOffline records that a node left at time t.
func (d *deadResultTracker) markOffline(id enode.ID, t time.Time) { d.offlineAt.Store(id, t) }

// markOnline records that a node is reachable again.
func (d *deadResultTracker) markOnline(id enode.ID) { d.offlineAt.Delete(id) }

// deadAge returns how long id had been offline at time at, and whether it was.
func (d *deadResultTracker) deadAge(id enode.ID, at time.Time) (time.Duration, bool) {
	v, ok := d.offlineAt.Load(id)
	if !ok {
		return 0, false
	}
	t := v.(time.Time)
	if at.Before(t) {
		return 0, false
	}
	return at.Sub(t), true
}

// deadLocal accumulates one searcher's results without locking; merge folds it
// into the tracker when the searcher ends.
type deadLocal struct {
	topic    int
	returned int
	dead     int
	ageHistS map[int]int    // dead age in whole seconds -> results
	buckets  map[int][2]int // time bucket since start -> {returned, dead}
}

func (l *deadLocal) record(d *deadResultTracker, id enode.ID, at time.Time) {
	if l.buckets == nil {
		l.buckets = make(map[int][2]int)
		l.ageHistS = make(map[int]int)
	}
	b := int(at.Sub(d.start) / deadBucket)
	c := l.buckets[b]
	c[0]++
	l.returned++
	if age, ok := d.deadAge(id, at); ok {
		c[1]++
		l.dead++
		l.ageHistS[int(age/time.Second)]++
	}
	l.buckets[b] = c
}

func (d *deadResultTracker) merge(l *deadLocal) {
	d.mu.Lock()
	defer d.mu.Unlock()
	t := d.topics[l.topic]
	if t == nil {
		t = &deadLocal{topic: l.topic, ageHistS: make(map[int]int), buckets: make(map[int][2]int)}
		d.topics[l.topic] = t
	}
	t.returned += l.returned
	t.dead += l.dead
	for k, v := range l.ageHistS {
		t.ageHistS[k] += v
	}
	for k, v := range l.buckets {
		c := t.buckets[k]
		c[0] += v[0]
		c[1] += v[1]
		t.buckets[k] = c
	}
}

// sortedTopics returns the per-topic totals by topic; the caller holds mu.
func (d *deadResultTracker) sortedTopics() []*deadLocal {
	out := make([]*deadLocal, 0, len(d.topics))
	for _, t := range d.topics {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].topic < out[j].topic })
	return out
}

// histPercentile returns the p-th percentile of a seconds histogram.
func histPercentile(h map[int]int, p int) int {
	keys := make([]int, 0, len(h))
	total := 0
	for k, v := range h {
		keys = append(keys, k)
		total += v
	}
	if total == 0 {
		return 0
	}
	sort.Ints(keys)
	need := (p*total + 99) / 100
	seen := 0
	for _, k := range keys {
		seen += h[k]
		if seen >= need {
			return k
		}
	}
	return keys[len(keys)-1]
}

// report prints the dead-result rate and dead-age distribution, in total and
// per topic.
func (d *deadResultTracker) report() {
	d.mu.Lock()
	defer d.mu.Unlock()
	topics := d.sortedTopics()
	total := &deadLocal{ageHistS: make(map[int]int)}
	for _, t := range topics {
		total.returned += t.returned
		total.dead += t.dead
		for k, v := range t.ageHistS {
			total.ageHistS[k] += v
		}
	}
	pct := func(l *deadLocal) float64 {
		if l.returned == 0 {
			return 0
		}
		return 100 * float64(l.dead) / float64(l.returned)
	}
	fmt.Println()
	fmt.Println("=== dead results in searches ===")
	fmt.Printf("  results returned: %d   dead-when-returned: %d (%.2f%%)\n", total.returned, total.dead, pct(total))
	if total.dead > 0 {
		sum := 0
		for age, n := range total.ageHistS {
			sum += age * n
		}
		h := total.ageHistS
		fmt.Printf("  dead-age (how long dead when returned): min=%ds p50=%ds p90=%ds p99=%ds max=%ds mean=%ds\n",
			histPercentile(h, 0), histPercentile(h, 50), histPercentile(h, 90), histPercentile(h, 99), histPercentile(h, 100), sum/total.dead)
	}
	fmt.Printf("%-6s %12s %10s %8s %9s %9s %9s %11s\n", "topic", "returned", "dead", "dead%", "age-p50", "age-p90", "age-max", ">lifetime")
	for _, t := range append(topics, nil) {
		name := "all"
		l := total
		if t != nil {
			name, l = fmt.Sprint(t.topic), t
		}
		over := 0
		for age, n := range l.ageHistS {
			if time.Duration(age)*time.Second > d.adLifetime {
				over += n
			}
		}
		fmt.Printf("%-6s %12d %10d %7.2f%% %8ds %8ds %8ds %11d\n", name, l.returned, l.dead, pct(l),
			histPercentile(l.ageHistS, 50), histPercentile(l.ageHistS, 90), histPercentile(l.ageHistS, 100), over)
	}
}

type deadTopicOut struct {
	Topic    int         `json:"topic"`
	Returned int         `json:"returned"`
	Dead     int         `json:"dead"`
	AgeHistS map[int]int `json:"ageHistS"`
	OverTime [][3]int64  `json:"overTime"` // {ms since search start, returned, dead}
}

// snapshot returns the per-topic counts for metrics.json.
func (d *deadResultTracker) snapshot() map[string]any {
	d.mu.Lock()
	defer d.mu.Unlock()
	topics := make([]deadTopicOut, 0, len(d.topics))
	for _, t := range d.sortedTopics() {
		keys := make([]int, 0, len(t.buckets))
		for k := range t.buckets {
			keys = append(keys, k)
		}
		sort.Ints(keys)
		series := make([][3]int64, 0, len(keys))
		for _, k := range keys {
			c := t.buckets[k]
			series = append(series, [3]int64{int64(k) * deadBucket.Milliseconds(), int64(c[0]), int64(c[1])})
		}
		topics = append(topics, deadTopicOut{t.topic, t.returned, t.dead, t.ageHistS, series})
	}
	return map[string]any{"bucketMs": deadBucket.Milliseconds(), "adLifetimeMs": d.adLifetime.Milliseconds(), "perTopic": topics}
}
