// Package churn is the fitted session-churn model: an age-conditioned hazard
// and an absence-length distribution derived from crawl data.
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

// Model is an age-conditioned hazard fitted from crawl data: how likely a
// peer that has been up for a given time is to leave within the next interval,
// and how long it then stays away. Time inside the model is in crawl units.
type Model struct {
	km      [][2]float64 // (crawls, Survival), ascending
	gapCDF  [][2]float64 // (crawls, cumulative probability)
	Window  [2]string
	Cadence float64 // real hours per crawl
}

func Load(path string) (*Model, error) {
	var raw struct {
		Window       [2]string      `json:"window"`
		CadenceHours float64        `json:"cadence_hours"`
		KM           [][2]float64   `json:"km_survival"`
		GapHist      map[string]int `json:"gap_hist_crawls"`
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	m := &Model{km: raw.KM, Window: raw.Window, Cadence: raw.CadenceHours}
	if m.Cadence == 0 {
		m.Cadence = 2
	}
	type kv struct {
		g int
		n int
	}
	var gs []kv
	total := 0
	for k, n := range raw.GapHist {
		g, _ := strconv.Atoi(k)
		gs = append(gs, kv{g, n})
		total += n
	}
	sort.Slice(gs, func(i, j int) bool { return gs[i].g < gs[j].g })
	cum := 0
	for _, e := range gs {
		cum += e.n
		m.gapCDF = append(m.gapCDF, [2]float64{float64(e.g), float64(cum) / float64(total)})
	}
	return m, nil
}

// Survival is S(t) for t in crawls, log-linear between fitted points.
func (m *Model) Survival(t float64) float64 {
	if t <= 0 || len(m.km) == 0 {
		return 1
	}
	i := sort.Search(len(m.km), func(i int) bool { return m.km[i][0] >= t })
	if i == len(m.km) {
		return m.km[len(m.km)-1][1]
	}
	if i == 0 || m.km[i][0] == t {
		return m.km[i][1]
	}
	t0, s0 := m.km[i-1][0], m.km[i-1][1]
	t1, s1 := m.km[i][0], m.km[i][1]
	if s0 <= 0 || s1 <= 0 {
		return s1
	}
	f := (t - t0) / (t1 - t0)
	return s0 * pow(s1/s0, f)
}

func pow(b, e float64) float64 { return math.Pow(b, e) }

// LeaveProb is P(leave within dt | up for age), both in crawls.
func (m *Model) LeaveProb(age, dt float64) float64 {
	s := m.Survival(age)
	if s <= 0 {
		return 1
	}
	return 1 - m.Survival(age+dt)/s
}

// gap samples an absence length in crawls.
func (m *Model) Gap(rng *rand.Rand) float64 {
	u := rng.Float64()
	for _, e := range m.gapCDF {
		if u <= e[1] {
			return e[0]
		}
	}
	return m.gapCDF[len(m.gapCDF)-1][0]
}

// InitialAge samples how long a node has already been up when the run starts,
// from the stationary age distribution (proportional to Survival), so early
// churn is not overstated by starting every node at age zero.
func (m *Model) InitialAge(rng *rand.Rand) float64 {
	if len(m.km) == 0 {
		return 0
	}
	maxT := m.km[len(m.km)-1][0]
	total := 0.0
	for t := 0.0; t < maxT; t++ {
		total += m.Survival(t)
	}
	u := rng.Float64() * total
	for t := 0.0; t < maxT; t++ {
		u -= m.Survival(t)
		if u <= 0 {
			return t
		}
	}
	return maxT
}

// Event is one planned absence of a node, in offsets from the window start.
type Event struct {
	DownAt, UpAt float64 // seconds
}

// Schedule precomputes a node's absences over a window that stands for
// windowRealHours of real time, using the same per-crawl draw the live driver
// makes, so a run is reproducible from its seed.
func (m *Model) Schedule(rng *rand.Rand, window float64, windowRealHours float64) []Event {
	crawl := window * m.Cadence / windowRealHours
	age := m.InitialAge(rng)
	var ev []Event
	for t := crawl; t < window; {
		if rng.Float64() < m.LeaveProb(age, 1) {
			g := m.Gap(rng) * crawl
			ev = append(ev, Event{DownAt: t, UpAt: t + g})
			t += g
			age = 0
			continue
		}
		age++
		t += crawl
	}
	return ev
}
