// Package churn is the fitted session-churn model: an age-conditioned hazard
// and an absence-length distribution derived from crawl data, per topic when
// the crawl provides it.
package churn

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"sort"
	"strconv"
)

// Model is fitted from crawl data. Time inside the model is in crawl units
// (Cadence real hours each). A model from cmd/crawl/model.py carries one
// Topic per chain, in descending share, plus the global fit; the older
// Nebula model has only the global fit.
type Model struct {
	Window  [2]string
	Cadence float64 // real hours per crawl unit
	Hours   float64 // real hours the crawl covered: the span InitialUp and AlwaysOn refer to
	Peers   int
	Global  Topic
	Topics  []Topic // descending share; may be empty
}

// Topic is the fit for one topic: its share of the live nodes, how many of
// them are present when the window opens, how many of those never leave, the
// survival of a session that starts inside the window, the absences that end
// in a return, and the share of departures that never return in the window.
type Topic struct {
	ID, Name     string
	Share        float64
	InitialUp    float64 // fraction present at the start (1 when unknown)
	AlwaysOn     float64 // of the initially present, the fraction that never leaves
	LeaveForever float64 // of the departures, the fraction that never returns
	ArrivalRate  float64 // new nodes per hour per live node (informational)
	FlickerRate  float64 // returns after an absence per hour per live node (informational)
	GoneRate     float64 // departures without return per hour per live node (informational)
	km           [][2]float64
	kmReturn     [][2]float64 // survival of a session that follows a return; km when absent
	gapCDF       [][2]float64
}

type rawTopic struct {
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	Share        float64        `json:"share"`
	InitialUp    *float64       `json:"initial_up_frac"`
	AlwaysOn     float64        `json:"always_on_frac"`
	LeaveForever float64        `json:"leave_forever_frac"`
	ArrivalRate  float64        `json:"arrival_rate_per_h"`
	FlickerRate  float64        `json:"flicker_rate_per_h"`
	GoneRate     float64        `json:"gone_rate_per_h"`
	KM           [][2]float64   `json:"km_survival"`
	KMReturn     [][2]float64   `json:"km_survival_after_return"`
	GapHist      map[string]int `json:"gap_hist_crawls"`
}

func (r rawTopic) topic() Topic {
	t := Topic{ID: r.ID, Name: r.Name, Share: r.Share, InitialUp: 1, AlwaysOn: r.AlwaysOn, LeaveForever: r.LeaveForever,
		ArrivalRate: r.ArrivalRate, FlickerRate: r.FlickerRate, GoneRate: r.GoneRate, km: r.KM, kmReturn: r.KMReturn}
	if len(t.kmReturn) < 2 {
		t.kmReturn = t.km
	}
	if r.InitialUp != nil {
		t.InitialUp = *r.InitialUp
	}
	type kv struct{ g, n int }
	var gs []kv
	total := 0
	for k, n := range r.GapHist {
		g, _ := strconv.Atoi(k)
		gs = append(gs, kv{g, n})
		total += n
	}
	sort.Slice(gs, func(i, j int) bool { return gs[i].g < gs[j].g })
	cum := 0
	for _, e := range gs {
		cum += e.n
		t.gapCDF = append(t.gapCDF, [2]float64{float64(e.g), float64(cum) / float64(total)})
	}
	return t
}

func Load(path string) (*Model, error) {
	var raw struct {
		rawTopic
		Window       [2]string  `json:"window"`
		CadenceHours float64    `json:"cadence_hours"`
		WindowHours  float64    `json:"window_hours"`
		Peers        int        `json:"peers"`
		Topics       []rawTopic `json:"topics"`
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	// The older model keeps always_on_frac at the top level for information
	// only; its schedule never used it, so it stays out of the global fit.
	raw.rawTopic.ID, raw.rawTopic.Share = "*", 1
	if len(raw.Topics) == 0 {
		raw.rawTopic.AlwaysOn = 0
	}
	m := &Model{Window: raw.Window, Cadence: raw.CadenceHours, Hours: raw.WindowHours, Peers: raw.Peers, Global: raw.rawTopic.topic()}
	if m.Cadence == 0 {
		m.Cadence = 2
	}
	if m.Hours == 0 {
		m.Hours = 24
	}
	for _, rt := range raw.Topics {
		m.Topics = append(m.Topics, rt.topic())
	}
	return m, nil
}

// Topic returns the fit for topic index i in share order, the global fit when
// the model has no topics or i is out of range.
func (m *Model) Topic(i int) *Topic {
	if i < 0 || i >= len(m.Topics) {
		return &m.Global
	}
	return &m.Topics[i]
}

// TopicFor is Topic for a run with k topics: the last of the k stands for
// every chain below the k-1 largest, so it gets the fit of the folded tail
// (the model's "other" topic, else the global fit).
func (m *Model) TopicFor(i, k int) *Topic {
	if k > 0 && k < len(m.Topics) && i == k-1 {
		for j := range m.Topics {
			if m.Topics[j].ID == "other" {
				return &m.Topics[j]
			}
		}
		return &m.Global
	}
	return m.Topic(i)
}

// Shares returns the topic shares for a run with k topics: the k-1 largest
// topics keep their share and the last topic takes the rest, so the tail
// still gets its nodes. k <= 0 or k > len(Topics) means every topic.
func (m *Model) Shares(k int) []float64 {
	n := len(m.Topics)
	if n == 0 {
		return []float64{1}
	}
	if k <= 0 || k > n {
		k = n
	}
	out := make([]float64, k)
	sum := 0.0
	for i := 0; i < k-1; i++ {
		out[i] = m.Topics[i].Share
		sum += out[i]
	}
	out[k-1] = math.Max(0, 1-sum)
	return out
}

// Survival is S(t) for t in crawls, log-linear between fitted points.
func (t *Topic) Survival(x float64) float64 { return survival(t.km, x) }

func survival(km [][2]float64, x float64) float64 {
	if x <= 0 || len(km) == 0 {
		return 1
	}
	i := sort.Search(len(km), func(i int) bool { return km[i][0] >= x })
	if i == len(km) {
		return km[len(km)-1][1]
	}
	if i == 0 || km[i][0] == x {
		return km[i][1]
	}
	t0, s0 := km[i-1][0], km[i-1][1]
	t1, s1 := km[i][0], km[i][1]
	if s0 <= 0 || s1 <= 0 {
		return s1
	}
	f := (x - t0) / (t1 - t0)
	return s0 * math.Pow(s1/s0, f)
}

// LeaveProb is P(leave within dt | up for age), both in crawls, for a first
// session; returned is the same for a session that follows a return.
func (t *Topic) LeaveProb(age, dt float64) float64 { return leaveProb(t.km, age, dt) }

func leaveProb(km [][2]float64, age, dt float64) float64 {
	s := survival(km, age)
	if s <= 0 {
		return 1
	}
	return 1 - survival(km, age+dt)/s
}

// Gap samples an absence length in crawls.
func (t *Topic) Gap(rng *rand.Rand) float64 {
	if len(t.gapCDF) == 0 {
		return 1
	}
	u := rng.Float64()
	for _, e := range t.gapCDF {
		if u <= e[1] {
			return e[0]
		}
	}
	return t.gapCDF[len(t.gapCDF)-1][0]
}

// InitialAge samples how long a node has already been up when the run starts,
// from the stationary age distribution (proportional to Survival), so early
// churn is not overstated by starting every node at age zero. Ages run up to
// the last observed departure: beyond it the fitted survival is flat only
// because the window ended, and a node placed there would never leave.
func (t *Topic) InitialAge(rng *rand.Rand) float64 { return initialAge(rng, t.kmReturn) }

func initialAge(rng *rand.Rand, km [][2]float64) float64 {
	if len(km) == 0 {
		return 0
	}
	maxT := 0.0
	for i := 1; i < len(km); i++ {
		if km[i][1] < km[i-1][1] {
			maxT = km[i][0]
		}
	}
	if maxT <= 0 {
		maxT = km[len(km)-1][0]
	}
	total := 0.0
	for x := 0.0; x < maxT; x++ {
		total += survival(km, x)
	}
	u := rng.Float64() * total
	for x := 0.0; x < maxT; x++ {
		u -= survival(km, x)
		if u <= 0 {
			return x
		}
	}
	return maxT
}

// NodeSeed mixes the run seed and a node index into a seed for that node's
// draws. math/rand's seeding leaves the first outputs of arithmetic seed
// progressions correlated, so seed+i*constant is not usable directly: with
// it, the first draw of every node fell in the same range and no node ever
// arrived late.
func NodeSeed(seed int64, i int) int64 {
	x := uint64(seed)*0x9E3779B97F4A7C15 ^ uint64(i)*0xBF58476D1CE4E5B9
	x ^= x >> 31
	x *= 0x94D049BB133111EB
	x ^= x >> 29
	return int64(x >> 1)
}

// Event is one planned absence of a node, in offsets from the window start.
// UpAt >= the window length means the node does not return in the run.
type Event struct {
	DownAt, UpAt float64 // seconds
}

// Schedule precomputes a node's absences over a window that stands for
// windowRealHours of real time, with the global fit.
func (m *Model) Schedule(rng *rand.Rand, window float64, windowRealHours float64) []Event {
	return m.ScheduleTopic(rng, window, windowRealHours, &m.Global)
}

// ScheduleTopic is Schedule with a topic's fit. The topic's initial-presence
// and always-on fractions describe the crawl's whole span, so the share of
// nodes that arrive or first depart inside the run is scaled by how many of
// those hours the window stands for. A node that arrives does so at a
// uniform time in the window and follows the survival of a first session;
// a node that goes away has its first departure at a uniform time in the
// window (the fitted survival is right-censored, so a stationary age would
// put most of these nodes past the last observed departure and they would
// never leave), then follows the survival of sessions after a return. The
// hazard is applied once per crawl unit; each departure is for good with the
// topic's leave-forever probability or lasts a sampled gap.
func (m *Model) ScheduleTopic(rng *rand.Rand, window float64, windowRealHours float64, t *Topic) []Event {
	if windowRealHours <= 0 {
		windowRealHours = window / 3600
	}
	crawl := window * m.Cadence / windowRealHours
	span := math.Min(1, windowRealHours/m.Hours)
	var ev []Event
	start := crawl
	age := 0.0
	km := t.km
	leaveNow := false
	switch {
	case rng.Float64() < (1-t.InitialUp)*span:
		arrive := rng.Float64() * window
		ev = append(ev, Event{DownAt: 0, UpAt: arrive})
		start = arrive + crawl
	case rng.Float64() >= (1-t.AlwaysOn)*span:
		return nil
	default:
		km = t.kmReturn
		start = rng.Float64() * window
		leaveNow = true
	}
	for x := start; x < window; {
		if leaveNow || rng.Float64() < leaveProb(km, age, 1) {
			leaveNow = false
			if rng.Float64() < t.LeaveForever {
				ev = append(ev, Event{DownAt: x, UpAt: window})
				return ev
			}
			g := t.Gap(rng) * crawl
			ev = append(ev, Event{DownAt: x, UpAt: x + g})
			x += g
			age, km = 0, t.kmReturn
			continue
		}
		age++
		x += crawl
	}
	return ev
}
