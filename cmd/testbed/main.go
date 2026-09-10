// testbed runs a scenario on the backend its testbed section names.
package main

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/datahop/topdisc-testbed/pkg/coordinator"
	"github.com/datahop/topdisc-testbed/pkg/scenario"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: testbed <scenario.yaml> | testbed reference")
		os.Exit(2)
	}
	if os.Args[1] == "reference" {
		scenario.PrintReference()
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
	case "local":
		runDir, err := scenario.PrepareRun(&cfg)
		if err != nil {
			fatal(err)
		}
		defer scenario.FlushLog()
		fmt.Println("run directory:", runDir)
		fmt.Println(cfg.ParamsLine())
		if err := coordinator.RunLocal(cfg, runDir); err != nil {
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
