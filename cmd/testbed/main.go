// testbed runs a scenario on the backend its testbed section names.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"

	"github.com/datahop/topdisc-testbed/pkg/assign"
	"github.com/datahop/topdisc-testbed/pkg/churn"
	"github.com/datahop/topdisc-testbed/pkg/coordinator"
	"github.com/datahop/topdisc-testbed/pkg/scenario"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: testbed <scenario.yaml> | testbed reference | testbed fleet <scenario.yaml> | testbed preview <scenario.yaml>")
		os.Exit(2)
	}
	if os.Args[1] == "reference" {
		scenario.PrintReference()
		return
	}
	if os.Args[1] == "preview" && len(os.Args) == 3 {
		cfg, err := scenario.Load(os.Args[2])
		if err != nil {
			fatal(err)
		}
		if err := preview(cfg, filepath.Dir(os.Args[2])); err != nil {
			fatal(err)
		}
		return
	}
	if os.Args[1] == "fleet" && len(os.Args) == 3 {
		// instance counts per region and the home region, for deploy/aws.sh
		cfg, err := scenario.Load(os.Args[2])
		if err != nil {
			fatal(err)
		}
		machines := 0
		if per := cfg.Testbed.G5k.VnodesPerMachine; per > 0 {
			machines = (cfg.Scenario.Population.Nodes + per - 1) / per
		}
		b, _ := json.Marshal(map[string]any{"regions": cfg.Fleet(), "home_region": cfg.Testbed.Cloud.HomeRegion, "g5k_machines": machines})
		fmt.Println(string(b))
		return
	}
	cfg, err := scenario.Load(os.Args[1])
	if err != nil {
		fatal(err)
	}
	switch cfg.Testbed.Backend {
	case "simnet":
		c := exec.Command("./simnet", os.Args[1])
		c.Stdout, c.Stderr = os.Stdout, os.Stderr
		if err := c.Run(); err != nil {
			fatal(err)
		}
	case "local", "cloud":
		runDir, err := scenario.PrepareRun(&cfg)
		if err != nil {
			fatal(err)
		}
		defer scenario.FlushLog()
		fmt.Println("run directory:", runDir)
		fmt.Println(cfg.ParamsLine())
		run := coordinator.RunLocal
		if cfg.Testbed.Backend == "cloud" {
			run = coordinator.RunCloud
		}
		if err := run(cfg, runDir); err != nil {
			fatal(err)
		}
	default:
		fatal(fmt.Errorf("unknown backend %q", cfg.Testbed.Backend))
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "fatal:", err)
	scenario.FlushLog()
	os.Exit(1)
}

// preview prints what the scenario's topic and churn models produce for its
// node count and search window: nodes per topic and the churn events per
// topic per real hour, next to the crawl model's own numbers.
func preview(cfg scenario.Config, dir string) error {
	sc := cfg.Scenario
	as, err := assign.Generate(cfg, func(i int) assign.Host { return assign.Host{IP: "10.0.0.1", BasePort: 30303, StatusOff: 1} }, 0, dir)
	if err != nil {
		return err
	}
	var model *churn.Model
	if sc.SessionChurn.Enabled && sc.SessionChurn.Model != "" {
		if model, err = churn.Load(filepath.Join(dir, sc.SessionChurn.Model)); err != nil {
			return err
		}
	}
	var topicModel *churn.Model
	if sc.Population.TopicModel != "" {
		if topicModel, err = churn.Load(filepath.Join(dir, sc.Population.TopicModel)); err != nil {
			return err
		}
	}
	window := sc.Phases.SearchTimeout.Seconds()
	hours := sc.SessionChurn.WindowHours
	if hours <= 0 {
		hours = window / 3600
	}
	type agg struct {
		nodes, arrivals, absences, forever int
		absence                            []float64
	}
	per := map[string]*agg{}
	for _, a := range as {
		t := a.Topics[len(a.Topics)-1]
		g := per[t]
		if g == nil {
			g = &agg{}
			per[t] = g
		}
		g.nodes++
		for _, e := range a.Churn {
			switch {
			case e.DownAt == 0:
				g.arrivals++
			case e.UpAt >= window:
				g.forever++
			default:
				g.absences++
				g.absence = append(g.absence, (e.UpAt-e.DownAt)/window*hours*60)
			}
		}
	}
	names := make([]string, 0, len(per))
	for t := range per {
		names = append(names, t)
	}
	sort.Slice(names, func(i, j int) bool { return per[names[i]].nodes > per[names[j]].nodes })
	fmt.Printf("%d nodes, %d topics, search window %s standing for %.1f real hours\n", len(as), len(names), sc.Phases.SearchTimeout, hours)
	if model == nil {
		fmt.Println("no session churn model: no churn events")
	}
	fmt.Printf("%-9s %6s %6s  %-34s %8s %8s %8s %9s   %s\n", "topic", "nodes", "share", "model chain", "arrive/h", "leave/h", "gone/h", "abs p50", "crawl: share arrive/h flick/h gone/h")
	for i, t := range names {
		g := per[t]
		idx := i
		fmt.Sscanf(t, "topic-%d", &idx)
		chain, mshare := "", 0.0
		var mt *churn.Topic
		if topicModel != nil {
			shares := topicModel.Shares(sc.Population.Topics)
			if idx < len(shares) {
				mshare = shares[idx]
			}
			mt = topicModel.TopicFor(idx, len(shares))
			if idx < len(topicModel.Topics) && (idx < len(shares)-1 || len(shares) == len(topicModel.Topics)) {
				chain = mt.ID + " " + mt.Name
			} else {
				chain = "remaining chains (" + mt.ID + ")"
			}
		}
		sort.Float64s(g.absence)
		p50 := 0.0
		if len(g.absence) > 0 {
			p50 = g.absence[len(g.absence)/2]
		}
		n := float64(g.nodes)
		ref := ""
		if mt != nil {
			ref = fmt.Sprintf("%.3f %7.2f%% %7.2f%% %6.2f%%", mshare, 100*mt.ArrivalRate, 100*mt.FlickerRate, 100*mt.GoneRate)
		}
		fmt.Printf("%-9s %6d %6.3f  %-34.34s %7.2f%% %7.2f%% %7.2f%% %6.0f min   %s\n", t, g.nodes, n/float64(len(as)), chain,
			100*float64(g.arrivals)/hours/n, 100*float64(g.absences)/hours/n, 100*float64(g.forever)/hours/n, p50, ref)
	}
	return nil
}
