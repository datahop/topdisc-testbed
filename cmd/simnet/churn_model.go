package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"sync"
	"time"

	"github.com/datahop/topdisc-testbed/pkg/churn"
)

// runModelChurn drives session churn from a fitted model: every node gets the
// schedule the assignment generator would give it on a real backend (same
// seed, same draws), with its topic's fit when the model has one, and the
// driver takes it offline and back on time. The search window stands for
// windowRealHours of real time; 0 means the window is real time.
func runModelChurn(c *connTable, m *churn.Model, window time.Duration, windowRealHours float64, seed int64, topicOf func(i int) *churn.Topic, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	if !waitForTopology(c, stop) {
		return
	}
	realHours := windowRealHours
	if realHours <= 0 {
		realHours = window.Hours()
	}
	crawl := time.Duration(float64(window) * m.Cadence / realHours)
	fmt.Printf("[session-churn] model %s..%s: window=%s stands for %.1f real hours, 1 crawl (%.3fh) = %s, %d topic fits\n",
		m.Window[0], m.Window[1], window, realHours, m.Cadence, crawl.Round(time.Millisecond), len(m.Topics))
	go logChurnProgress(c, stop)

	sched := make(map[int][]churn.Event, len(c.out))
	for i := range c.out {
		if ev := m.ScheduleTopic(rand.New(rand.NewSource(churn.NodeSeed(seed, i))), window.Seconds(), windowRealHours, topicOf(i)); len(ev) > 0 {
			sched[i] = ev
		}
	}
	dumpChurnSchedule(window.Seconds(), sched)

	t0 := time.Now()
	var wg sync.WaitGroup
	var arrivals, leaves, forever int
	var mu sync.Mutex
	for i, ev := range sched {
		wg.Add(1)
		go func(idx int, ev []churn.Event) {
			defer wg.Done()
			for _, e := range ev {
				if !sleepUntil(t0, e.DownAt, stop) {
					return
				}
				c.depart(idx)
				if h := hostOf(idx); h != nil {
					h.stop()
				}
				mu.Lock()
				if e.DownAt == 0 {
					arrivals++
				} else if e.UpAt >= window.Seconds() {
					forever++
				} else {
					leaves++
				}
				mu.Unlock()
				if e.UpAt >= window.Seconds() {
					return
				}
				if !sleepUntil(t0, e.UpAt, stop) {
					return
				}
				if h := hostOf(idx); h != nil {
					h.start()
				}
				c.rejoin(idx)
			}
		}(i, ev)
	}
	<-stop
	wg.Wait()
	mu.Lock()
	fmt.Printf("[session-churn] schedule applied: %d arrivals, %d absences, %d left for good; %d discovery services stopped\n", arrivals, leaves, forever, hostRestarts())
	mu.Unlock()
}

// churnDumpPath is where the applied schedule is written (churn.json in the
// run directory), so the report can tell a node's absence from search latency.
var churnDumpPath string

func dumpChurnSchedule(window float64, sched map[int][]churn.Event) {
	if churnDumpPath == "" {
		return
	}
	f, err := os.Create(churnDumpPath)
	if err != nil {
		fmt.Printf("[session-churn] schedule not written: %v\n", err)
		return
	}
	defer f.Close()
	json.NewEncoder(f).Encode(struct {
		Window float64               `json:"window"`
		Nodes  map[int][]churn.Event `json:"nodes"`
	}{window, sched})
}

// sleepUntil waits until offset seconds after t0; false when stopped first.
func sleepUntil(t0 time.Time, offset float64, stop <-chan struct{}) bool {
	d := time.Until(t0.Add(time.Duration(offset * float64(time.Second))))
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-stop:
		return false
	case <-t.C:
		return true
	}
}
