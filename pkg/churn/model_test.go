package churn

import (
	"math/rand"
	"testing"
)

// TestScheduleTopicWindows checks that a schedule produces the same churn
// per real hour whether the window is real time or compressed: the same
// crawl model over 24 real hours must give the same arrivals, absences and
// permanent leaves per node for a 24 h window and for a 10 min window that
// stands for 24 h.
func TestScheduleTopicWindows(t *testing.T) {
	m, err := Load("../../scenarios/models/crawl-2026-09-18.json")
	if err != nil {
		t.Skip("model not present:", err)
	}
	topic := m.Topic(0)
	count := func(window float64) (arr, abs, gone float64) {
		const n = 4000
		for i := 0; i < n; i++ {
			for _, e := range m.ScheduleTopic(rand.New(rand.NewSource(NodeSeed(1, i))), window, 24, topic) {
				switch {
				case e.DownAt == 0:
					arr++
				case e.UpAt >= window:
					gone++
				default:
					abs++
				}
			}
		}
		return arr / n, abs / n, gone / n
	}
	a1, b1, g1 := count(86400)
	a2, b2, g2 := count(600)
	// A 1 h real-time window must produce about 1/24 of the day's arrivals.
	arr1h := 0.0
	for i := 0; i < 4000; i++ {
		for _, e := range m.ScheduleTopic(rand.New(rand.NewSource(NodeSeed(1, i))), 3600, 1, topic) {
			if e.DownAt == 0 {
				arr1h++
			}
		}
	}
	arr1h /= 4000
	if arr1h > a1/24*1.6+0.002 || arr1h < a1/24/1.6-0.002 {
		t.Fatalf("1 h window arrivals %.4f per node, want about %.4f (a 24th of the day's)", arr1h, a1/24)
	}
	t.Logf("24h window: arrivals %.3f absences %.3f gone %.3f per node; 10min window: %.3f %.3f %.3f", a1, b1, g1, a2, b2, g2)
	if a1 < 0.05 || a2 < 0.05 {
		t.Fatalf("arrivals missing: %.3f vs %.3f per node (model initial_up %.2f)", a1, a2, topic.InitialUp)
	}
	for _, p := range [][2]float64{{a1, a2}, {b1, b2}, {g1, g2}} {
		if p[0] > 1.5*p[1]+0.02 || p[1] > 1.5*p[0]+0.02 {
			t.Fatalf("window length changes the churn: %.3f vs %.3f per node", p[0], p[1])
		}
	}
}
