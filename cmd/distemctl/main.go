// distemctl turns reserved physical nodes running Distem into one virtual node
// per TopDisc node and writes the coordinator's inventory. deploy/g5k.py calls
// it after reservation and bootstrap; it also drives a single validation host.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/datahop/topdisc-testbed/pkg/distem"
	"github.com/datahop/topdisc-testbed/pkg/scenario"
	"github.com/datahop/topdisc-testbed/pkg/wan"
)

func main() {
	coord := flag.String("coordinator", "localhost:4567", "Distem coordinator address")
	sc := flag.String("scenario", "", "scenario YAML (fleet, regions, pins, rtt table)")
	pnodes := flag.String("pnodes", "", "comma-separated physical nodes (as Distem knows them)")
	subnet := flag.String("subnet", "", "vnetwork CIDR, e.g. the reserved Grid'5000 /22")
	image := flag.String("image", "", "vnode filesystem image, e.g. file:///home/user/topdisc-vnode.tar.gz")
	out := flag.String("inventory", "inventory.json", "where to write the inventory")
	down := flag.Bool("down", false, "remove the vnodes and the vnetwork instead")
	flag.Parse()
	logf := func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) }
	fail := func(err error) { fmt.Fprintln(os.Stderr, "distemctl:", err); os.Exit(1) }

	c := distem.New(*coord)
	if *down {
		if err := c.VnodesRemove(nil); err != nil {
			fail(err)
		}
		if err := c.VnetworksRemove(); err != nil {
			fail(err)
		}
		logf("all vnodes and vnetworks removed")
		return
	}
	if *sc == "" || *pnodes == "" || *subnet == "" || *image == "" {
		fail(fmt.Errorf("need -scenario, -pnodes, -subnet and -image"))
	}
	cfg, err := scenario.Load(*sc)
	if err != nil {
		fail(err)
	}
	tablePath := cfg.Scenario.Network.RTTTable
	if !filepath.IsAbs(tablePath) {
		tablePath = filepath.Join(filepath.Dir(*sc), tablePath)
	}
	table, err := wan.LoadRTTTable(tablePath)
	if err != nil {
		fail(err)
	}
	plan, err := distem.Layout(cfg, strings.Split(*pnodes, ","), table)
	if err != nil {
		fail(err)
	}
	logf("%d vnodes over %d pnodes, regions %v", len(plan.Vnodes), len(strings.Split(*pnodes, ",")), plan.Regions)
	if err := plan.Apply(c, *subnet, *image, cfg.Testbed.Wan.RateKbps, logf); err != nil {
		fail(err)
	}
	if err := plan.WriteInventory(*out); err != nil {
		fail(err)
	}
	logf("inventory written to %s", *out)
}
