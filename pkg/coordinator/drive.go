package coordinator

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/datahop/topdisc-testbed/pkg/assign"
	"github.com/datahop/topdisc-testbed/pkg/churn"
)

// hostControl is what a backend must offer to run one node: the local backend
// binds these to host.Runner, the cloud backend to hostagent HTTP calls.
type hostControl struct {
	start func(idx int) error
	kill  func(idx int) error
}

// churnFailShown bounds the failed churn calls printed in full: a 25k run
// schedules about 12k churn events.
const churnFailShown = 20

// churnLog counts the churn calls applied and reports the failed ones.
type churnLog struct {
	out                                       io.Writer
	mu                                        sync.Mutex
	departs, rejoins, killFailed, startFailed int
}

func (c *churnLog) kill(idx int, err error)  { c.record("kill", idx, err, &c.departs, &c.killFailed) }
func (c *churnLog) start(idx int, err error) { c.record("start", idx, err, &c.rejoins, &c.startFailed) }

func (c *churnLog) record(op string, idx int, err error, ok, failed *int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil {
		*ok++
		return
	}
	*failed++
	switch n := c.killFailed + c.startFailed; {
	case n <= churnFailShown:
		fmt.Fprintf(c.out, "[churn] %s node %d: %v\n", op, idx, err)
	case n == churnFailShown+1:
		fmt.Fprintln(c.out, "[churn] further failures counted only")
	}
}

func (c *churnLog) active() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.departs > 0 || c.killFailed+c.startFailed > 0
}

func (c *churnLog) summary() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := fmt.Sprintf("departs=%d rejoins=%d", c.departs, c.rejoins)
	if c.killFailed+c.startFailed > 0 {
		s += fmt.Sprintf(" failed kills=%d starts=%d", c.killFailed, c.startFailed)
	}
	return s
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
	if err := ctl.start(as[0].Idx); err != nil {
		return err
	}
	time.Sleep(time.Second) // bootnode first
	if err := each(len(as)-1, func(i int) error { return ctl.start(as[i+1].Idx) }); err != nil {
		return err
	}
	searchAt := time.UnixMilli(as[0].Phases.SearchAt)
	stop := time.UnixMilli(as[0].Phases.StopAt).Add(grace)
	cl := &churnLog{out: os.Stdout}
	for _, a := range as {
		if len(a.Churn) == 0 {
			continue
		}
		go func(idx int, events []churn.Event) {
			for _, ev := range events {
				down, up := searchAt.Add(time.Duration(ev.DownAt*float64(time.Second))), searchAt.Add(time.Duration(ev.UpAt*float64(time.Second)))
				// Nothing past the stop: a departure then is the end of the
				// run, and a return would start a node after collection.
				if !down.Before(stop) {
					return
				}
				time.Sleep(time.Until(down))
				cl.kill(idx, ctl.kill(idx))
				if !up.Before(stop) {
					return
				}
				time.Sleep(time.Until(up))
				cl.start(idx, ctl.start(idx))
			}
		}(a.Idx, a.Churn)
	}
	fmt.Printf("nodes started; collecting at %s\n", stop.Format(time.TimeOnly))
	for time.Now().Before(stop) {
		time.Sleep(min(30*time.Second, time.Until(stop)+time.Millisecond))
		if cl.active() {
			fmt.Printf("[churn] %s\n", cl.summary())
		}
	}
	if p := planned(as); p > 0 {
		fmt.Printf("churn applied: %s (planned %d)\n", cl.summary(), p)
	}
	return nil
}
