// recollect rebuilds a run's metrics from per-node traces that were already
// pulled off the hosts. The cloud coordinator normally does this at the end of
// a run, but it fetches every trace onto its own root filesystem first, and a
// run large enough to fill that disk loses the aggregation while the traces
// themselves survive on the hosts.
//
//	recollect <assignments-dir> <traces-dir> <run-dir>
//
// It writes metrics.json, series.json, nodes.json and oh.json into <run-dir>,
// the same four files figures/ reads. Traces missing from <traces-dir> are
// skipped, so a partial salvage still produces a run over what did survive.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/datahop/topdisc-testbed/pkg/assign"
	"github.com/datahop/topdisc-testbed/pkg/coordinator"
)

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: recollect <assignments-dir> <traces-dir> <run-dir>")
		os.Exit(2)
	}
	asgDir, trDir, runDir := os.Args[1], os.Args[2], os.Args[3]

	as, err := readAssignments(asgDir)
	if err != nil {
		fatal(err)
	}
	if len(as) == 0 {
		fatal(fmt.Errorf("no assignments in %s", asgDir))
	}
	// Collect indexes traces by assignment, so the assignments have to be in
	// index order the way the coordinator held them.
	sort.Slice(as, func(i, j int) bool { return as[i].Idx < as[j].Idx })

	present := 0
	for _, a := range as {
		if _, err := os.Stat(filepath.Join(trDir, fmt.Sprintf("node%d.json", a.Idx))); err == nil {
			present++
		}
	}
	fmt.Printf("assignments: %d; traces present: %d (%.1f%%)\n",
		len(as), present, 100*float64(present)/float64(len(as)))

	if err := os.MkdirAll(runDir, 0o755); err != nil {
		fatal(err)
	}
	if err := coordinator.Collect(trDir, runDir, as); err != nil {
		fatal(err)
	}
}

func readAssignments(dir string) ([]assign.Assignment, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var as []assign.Assignment
	for _, e := range ents {
		name := e.Name()
		if !strings.HasPrefix(name, "node") || !strings.HasSuffix(name, ".json") {
			continue
		}
		if _, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "node"), ".json")); err != nil {
			continue
		}
		a, err := assign.Read(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		as = append(as, a)
	}
	return as, nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "recollect:", err)
	os.Exit(1)
}
