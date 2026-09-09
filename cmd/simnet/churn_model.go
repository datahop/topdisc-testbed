package main

import (
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/datahop/topdisc-testbed/pkg/churn"
)

// runModelChurn drives session churn from a fitted model. The search window
// stands for windowRealHours of real time, which sets how long one crawl unit
// is in the run; each online node is re-evaluated once per crawl unit.
func runModelChurn(c *connTable, m *churn.Model, window time.Duration, windowRealHours float64, seed int64, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	if !waitForTopology(c, stop) {
		return
	}
	crawl := time.Duration(float64(window) * m.Cadence / windowRealHours)
	fmt.Printf("[session-churn] model %s..%s: window=%s stands for %.0f real hours, 1 crawl (%.0fh) = %s\n",
		m.Window[0][:10], m.Window[1][:10], window, windowRealHours, m.Cadence, crawl.Round(time.Millisecond))
	go logChurnProgress(c, stop)

	var wg sync.WaitGroup
	for i := range c.out {
		wg.Add(1)
		go func(idx int, r *rand.Rand) {
			defer wg.Done()
			age := m.InitialAge(r)
			t := time.NewTimer(crawl)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
				}
				if r.Float64() < m.LeaveProb(age, 1) {
					c.depart(idx)
					g := time.NewTimer(time.Duration(m.Gap(r) * float64(crawl)))
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
