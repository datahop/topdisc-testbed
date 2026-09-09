package main

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"
)

// churnModel is an age-conditioned hazard fitted from crawl data: how likely a
// peer that has been up for a given time is to leave within the next interval,
// and how long it then stays away. Time inside the model is in crawl units.
type churnModel struct {
	km      [][2]float64 // (crawls, survival), ascending
	gapCDF  [][2]float64 // (crawls, cumulative probability)
	window  [2]string
	cadence float64 // real hours per crawl
}

func loadChurnModel(path string) (*churnModel, error) {
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
	m := &churnModel{km: raw.KM, window: raw.Window, cadence: raw.CadenceHours}
	if m.cadence == 0 {
		m.cadence = 2
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

// survival is S(t) for t in crawls, log-linear between fitted points.
func (m *churnModel) survival(t float64) float64 {
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

// leaveProb is P(leave within dt | up for age), both in crawls.
func (m *churnModel) leaveProb(age, dt float64) float64 {
	s := m.survival(age)
	if s <= 0 {
		return 1
	}
	return 1 - m.survival(age+dt)/s
}

// gap samples an absence length in crawls.
func (m *churnModel) gap(rng *rand.Rand) float64 {
	u := rng.Float64()
	for _, e := range m.gapCDF {
		if u <= e[1] {
			return e[0]
		}
	}
	return m.gapCDF[len(m.gapCDF)-1][0]
}

// initialAge samples how long a node has already been up when the run starts,
// from the stationary age distribution (proportional to survival), so early
// churn is not overstated by starting every node at age zero.
func (m *churnModel) initialAge(rng *rand.Rand) float64 {
	if len(m.km) == 0 {
		return 0
	}
	maxT := m.km[len(m.km)-1][0]
	total := 0.0
	for t := 0.0; t < maxT; t++ {
		total += m.survival(t)
	}
	u := rng.Float64() * total
	for t := 0.0; t < maxT; t++ {
		u -= m.survival(t)
		if u <= 0 {
			return t
		}
	}
	return maxT
}

// runModelChurn drives session churn from a fitted model. The search window
// stands for windowRealHours of real time, which sets how long one crawl unit
// is in the run; each online node is re-evaluated once per crawl unit.
func runModelChurn(c *connTable, m *churnModel, window time.Duration, windowRealHours float64, seed int64, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	if !waitForTopology(c, stop) {
		return
	}
	crawl := time.Duration(float64(window) * m.cadence / windowRealHours)
	fmt.Printf("[session-churn] model %s..%s: window=%s stands for %.0f real hours, 1 crawl (%.0fh) = %s\n",
		m.window[0][:10], m.window[1][:10], window, windowRealHours, m.cadence, crawl.Round(time.Millisecond))
	go logChurnProgress(c, stop)

	var wg sync.WaitGroup
	for i := range c.out {
		wg.Add(1)
		go func(idx int, r *rand.Rand) {
			defer wg.Done()
			age := m.initialAge(r)
			t := time.NewTimer(crawl)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
				}
				if r.Float64() < m.leaveProb(age, 1) {
					c.depart(idx)
					g := time.NewTimer(time.Duration(m.gap(r) * float64(crawl)))
					select {
					case <-stop:
						g.Stop()
						return
					case <-g.C:
					}
					c.rejoin(idx)
					age = 0
				} else {
					age++
				}
				t.Reset(crawl)
			}
		}(i, rand.New(rand.NewSource(seed+int64(i)*2654435761)))
	}
	<-stop
	wg.Wait()
}
