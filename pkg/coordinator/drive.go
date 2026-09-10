package coordinator

import (
	"fmt"
	"sync"
	"time"

	"github.com/datahop/topdisc-testbed/pkg/assign"
	"github.com/datahop/topdisc-testbed/pkg/churn"
)

// hostControl is what a backend must offer to run one node: the local backend
// binds these to host.Runner, the cloud backend to hostagent HTTP calls.
type hostControl struct {
	start func(idx int) error
	kill  func(idx int) bool
}

func printPhases(backend string, as []assign.Assignment, t0 int64) {
	ms := func(t int64) time.Duration { return time.Duration(t-t0) * time.Millisecond }
	fmt.Printf("%s backend: %d nodes, bootnode %s, phases register@+%s search@+%s stop@+%s\n", backend, len(as), as[1].Bootnodes[0][:24]+"…",
		ms(as[0].Phases.RegisterAt), ms(as[0].Phases.SearchAt), ms(as[0].Phases.StopAt))
}

// drive starts every node (bootnode first), applies the churn schedule and
// returns once StopAt+grace has passed. Churn: kill at DownAt, restart the
// same identity at UpAt; a restarted node re-registers and re-searches at
// once because its phase times are past.
func drive(as []assign.Assignment, ctl hostControl, grace time.Duration) error {
	for _, a := range as {
		if err := ctl.start(a.Idx); err != nil {
			return err
		}
		if a.Idx == 0 {
			time.Sleep(time.Second)
		}
	}
	searchAt := time.UnixMilli(as[0].Phases.SearchAt)
	var mu sync.Mutex
	departs, rejoins := 0, 0
	for _, a := range as {
		if len(a.Churn) == 0 {
			continue
		}
		go func(idx int, events []churn.Event) {
			for _, ev := range events {
				time.Sleep(time.Until(searchAt.Add(time.Duration(ev.DownAt * float64(time.Second)))))
				if ctl.kill(idx) {
					mu.Lock()
					departs++
					mu.Unlock()
				}
				time.Sleep(time.Until(searchAt.Add(time.Duration(ev.UpAt * float64(time.Second)))))
				if ctl.start(idx) == nil {
					mu.Lock()
					rejoins++
					mu.Unlock()
				}
			}
		}(a.Idx, a.Churn)
	}
	stop := time.UnixMilli(as[0].Phases.StopAt).Add(grace)
	fmt.Printf("nodes started; collecting at %s\n", stop.Format(time.TimeOnly))
	for time.Now().Before(stop) {
		time.Sleep(min(30*time.Second, time.Until(stop)+time.Millisecond))
		mu.Lock()
		if departs > 0 {
			fmt.Printf("[churn] departs=%d rejoins=%d\n", departs, rejoins)
		}
		mu.Unlock()
	}
	mu.Lock()
	defer mu.Unlock()
	if p := planned(as); p > 0 {
		fmt.Printf("churn applied: departs=%d rejoins=%d (planned %d)\n", departs, rejoins, p)
	}
	return nil
}
