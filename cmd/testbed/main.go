// testbed runs a scenario on the backend its testbed section names.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"

	"github.com/datahop/topdisc-testbed/pkg/coordinator"
	"github.com/datahop/topdisc-testbed/pkg/scenario"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: testbed <scenario.yaml> | testbed reference | testbed fleet <scenario.yaml>")
		os.Exit(2)
	}
	if os.Args[1] == "reference" {
		scenario.PrintReference()
		return
	}
	if os.Args[1] == "fleet" && len(os.Args) == 3 {
		// instance counts per region and the home region, for deploy/aws.sh
		cfg, err := scenario.Load(os.Args[2])
		if err != nil {
			fatal(err)
		}
		b, _ := json.Marshal(map[string]any{"regions": cfg.Fleet(), "home_region": cfg.Testbed.Cloud.HomeRegion})
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
