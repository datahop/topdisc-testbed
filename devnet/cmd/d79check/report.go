package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// M3b writes these files (run dir or snapshots/<label>/), each a JSON object with its
// data and a top-level "check" of the same shape as ours.
var (
	m3bTableFiles = []string{"tables.json", "crawl-reach.json"}
	m3bTopicFiles = []string{"topic.json"}
)

// e2Series are compared between a run and its control; instantaneous procload is a
// whole-percent gauge, so the judged CPU series is the cumulative procload/total.
var e2Series = []string{
	"system/cpu/procload/total", "rss", "p2p/ingress.count", "p2p/egress.count",
	"discover/ingress.count", "discover/egress.count", "system/cpu/goroutines",
}

type runData struct {
	dir     string
	inv     *inventoryResult
	peers   map[string]*peersResult
	metrics map[string]*metricsResult
	heads   *headsResult
	txs     *txsResult
	logs    *logsResult
	m3b     map[string][]string // file name → paths found
	subdirs []string            // "." then snapshots/<label>, sorted
	// split relabels a single-kind run into A (OLD role) and B (NEW role).
	split       *inventory
	headSamples []headSample
}

func loadRun(dir string) (*runData, error) {
	if _, err := os.Stat(dir); err != nil {
		return nil, err
	}
	rd := &runData{dir: dir, peers: map[string]*peersResult{}, metrics: map[string]*metricsResult{}, m3b: map[string][]string{}}
	rd.subdirs = []string{"."}
	snaps, _ := filepath.Glob(filepath.Join(dir, "snapshots", "*"))
	sort.Strings(snaps)
	for _, s := range snaps {
		if fi, err := os.Stat(s); err == nil && fi.IsDir() {
			rel, _ := filepath.Rel(dir, s)
			rd.subdirs = append(rd.subdirs, rel)
		}
	}
	load := func(rel, name string, v any) (bool, error) {
		err := readJSON(filepath.Join(dir, rel, name), v)
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return err == nil, err
	}
	for _, rel := range rd.subdirs {
		var inv inventoryResult
		if ok, err := load(rel, "inventory-check.json", &inv); err != nil {
			return nil, err
		} else if ok && rd.inv == nil {
			rd.inv = &inv
		}
		var p peersResult
		if ok, err := load(rel, "peers.json", &p); err != nil {
			return nil, err
		} else if ok {
			rd.peers[rel] = &p
		}
		var m metricsResult
		if ok, err := load(rel, "metrics.json", &m); err != nil {
			return nil, err
		} else if ok {
			rd.metrics[rel] = &m
		}
		var h headsResult
		if ok, err := load(rel, "heads.json", &h); err != nil {
			return nil, err
		} else if ok && rd.heads == nil {
			rd.heads = &h
			if s, err := readSamples(filepath.Join(dir, rel, "heads.jsonl")); err == nil {
				rd.headSamples = s
			}
		}
		var t txsResult
		if ok, err := load(rel, "txs.json", &t); err != nil {
			return nil, err
		} else if ok && rd.txs == nil {
			rd.txs = &t
		}
		var l logsResult
		if ok, err := load(rel, "logs.json", &l); err != nil {
			return nil, err
		} else if ok && rd.logs == nil {
			rd.logs = &l
		}
		for _, name := range append(append([]string{}, m3bTableFiles...), m3bTopicFiles...) {
			p := filepath.Join(dir, rel, name)
			if _, err := os.Stat(p); err == nil {
				rd.m3b[name] = append(rd.m3b[name], p)
			}
		}
	}
	return rd, nil
}

// kinds returns the number of OLD and NEW ELs from whichever file lists them.
func (rd *runData) kinds() (nOld, nNew int, ok bool) {
	if rd.inv != nil {
		return rd.inv.Old, rd.inv.New, true
	}
	count := func(kinds []string) (int, int, bool) {
		o, n := 0, 0
		for _, k := range kinds {
			switch k {
			case kindOld:
				o++
			case kindNew:
				n++
			}
		}
		return o, n, len(kinds) > 0
	}
	var ks []string
	switch {
	case rd.logs != nil:
		for _, n := range rd.logs.Nodes {
			ks = append(ks, n.Kind)
		}
	case len(rd.metrics) > 0:
		for _, n := range rd.latestMetrics().Nodes {
			ks = append(ks, n.Kind)
		}
	}
	return count(ks)
}

// latestMetrics prefers the run-dir metrics.json, else the last snapshot that has one.
func (rd *runData) latestMetrics() *metricsResult {
	if m, ok := rd.metrics["."]; ok {
		return m
	}
	for i := len(rd.subdirs) - 1; i >= 0; i-- {
		if m, ok := rd.metrics[rd.subdirs[i]]; ok {
			return m
		}
	}
	return nil
}

type reportResult struct {
	Run     string       `json:"run"`
	Control string       `json:"control,omitempty"`
	Split   *splitGroups `json:"split,omitempty"`
	Checks  []check      `json:"checks"`
}

func init() {
	register("report", "merge a run dir's JSON into checks.json and summary.md", runReport)
}

func runReport(args []string) error {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	run := fs.String("run", "", "run directory with the d79check JSON files (and snapshots/*/)")
	control := fs.String("control", "", "control run directory (R0) for E2")
	out := fs.String("out", "", "output directory (default: -run)")
	fs.Parse(args)
	if *run == "" {
		return errors.New("-run is required")
	}
	if *out == "" {
		*out = *run
	}
	rd, err := loadRun(*run)
	if err != nil {
		return err
	}
	var ctl *runData
	if *control != "" {
		if ctl, err = loadRun(*control); err != nil {
			return fmt.Errorf("control: %v", err)
		}
	}
	res := reportResult{Run: *run, Control: *control, Checks: buildChecks(rd, ctl)}
	res.Split = rd.splitGroups()
	md := renderSummary(res)
	if res.Split != nil {
		fmt.Println(res.Split.line())
	}
	fmt.Print(renderTable(res.Checks))
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(*out, "checks.json"), res); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(*out, "summary.md"), []byte(md), 0o644)
}

func buildChecks(rd, ctl *runData) []check {
	nOld, nNew, known := rd.kinds()
	single := known && (nOld == 0 || nNew == 0)
	if single && rd.inv != nil {
		rd.split = splitInventory(rd.inv.Nodes)
	}
	return []check{
		e1Check(rd, single),
		e2Check(rd, ctl),
		e3Check(rd),
		e4Check(rd, single),
	}
}

func top(id, desc string, sub []check) check {
	return check{ID: id, Desc: desc, Status: overall(sub), Sub: sub}
}

// population lists the inventory nodes of a kind; participant 0 is left out when noBoot.
func (rd *runData) population(kind string, noBoot bool) ([]node, bool) {
	if rd.inv == nil {
		return nil, false
	}
	var out []node
	for i, n := range rd.inv.Nodes {
		if n.Kind == kind && !(noBoot && i == 0) {
			out = append(out, n)
		}
	}
	return out, true
}

// logsFor returns the usable log summaries of nodes and what is missing.
func (rd *runData) logsFor(nodes []node) (map[string]logsNode, []string) {
	if rd.logs == nil {
		return nil, []string{"no logs.json"}
	}
	got := make(map[string]logsNode)
	for _, l := range rd.logs.Nodes {
		got[l.Name] = l
	}
	out := make(map[string]logsNode)
	var missing []string
	for _, n := range nodes {
		l, ok := got[n.Name]
		switch {
		case !ok:
			missing = append(missing, n.Name+": not in logs.json")
		case l.Error != "":
			missing = append(missing, n.Name+": "+l.Error)
		case l.Lines == 0:
			missing = append(missing, n.Name+": empty log")
		default:
			out[n.Name] = l
		}
	}
	return out, missing
}

func metricsNodes(m *metricsResult) map[string]metricsNode {
	out := make(map[string]metricsNode)
	for _, n := range m.Nodes {
		out[n.Name] = n
	}
	return out
}

// m3bTop reports E1/E4 as "needs M3b" while an M3b file is missing; failures of the
// parts judged here stay visible in the detail.
func m3bTop(id, desc string, sub []check) check {
	c := top(id, desc, sub)
	var failed []string
	missing := false
	for _, s := range sub {
		switch s.Status {
		case statusNeedsM3b:
			missing = true
		case statusFail:
			failed = append(failed, s.ID)
		}
	}
	if missing {
		c.Status = statusNeedsM3b
		if len(failed) > 0 {
			c.Detail = "FAIL so far: " + strings.Join(failed, ", ")
		}
	}
	return c
}

var snapLabelRe = regexp.MustCompile(`^t(\d+)m$`)

// snapMinutes is the nominal age of a snapshot dir snapshots/t<m>m, -1 for others.
func snapMinutes(rel string) int {
	m := snapLabelRe.FindStringSubmatch(filepath.Base(rel))
	if m == nil {
		return -1
	}
	v, _ := strconv.Atoi(m[1])
	return v
}

// e1Check judges E1(a) tables only at labelled snapshots t<m>m with m ≥ 30, each of which
// needs a PASS or FAIL; crawl reach is read wherever it is.
func e1Check(rd *runData, single bool) check {
	const id = "E1"
	g := rd.names()
	desc := fmt.Sprintf("discv5 tables mix %s and %s; crawl reaches every node", upper(g.old), upper(g.new))
	if single && rd.split == nil {
		return check{ID: id, Desc: desc, Status: statusNA, Detail: "single-kind run (no inventory-check.json to split)"}
	}
	var tables []check
	have := make(map[string]bool)
	for _, p := range rd.m3b["tables.json"] {
		rel, _ := filepath.Rel(rd.dir, filepath.Dir(p))
		var c check
		if rd.split != nil {
			c = rd.splitTables(p, rel)
		} else {
			c = readM3b(p, "E1 "+filepath.Join(rel, "tables.json"))
		}
		switch m := snapMinutes(rel); {
		case m < 30:
			c.Detail = strings.TrimSpace(fmt.Sprintf("not judged before t30m or unlabelled (%s) %s", c.Status, c.Detail))
			c.Status = statusNA
		case c.Status == statusPass || c.Status == statusFail:
			have[rel] = true
		case c.Status == statusNA:
			c.Status, c.Detail = statusMissing, strings.TrimSpace("no verdict at "+rel+" "+c.Detail)
		}
		tables = append(tables, c)
	}
	if len(tables) == 0 {
		tables = []check{{ID: "E1 tables.json", Status: statusNeedsM3b}}
	} else {
		expected := 0
		for _, rel := range rd.subdirs {
			if snapMinutes(rel) >= 30 {
				expected++
				if _, ok := have[rel]; !ok {
					tables = append(tables, check{ID: "E1 " + rel, Status: statusMissing, Detail: "no tables.json verdict"})
				}
			}
		}
		if expected == 0 {
			tables = append(tables, check{ID: "E1 tables.json", Status: statusMissing, Detail: "no snapshot at t30m or later"})
		}
	}
	sub := append(tables, m3bFiles(rd, "E1", "crawl-reach.json")...)
	return m3bTop(id, desc, sub)
}

func m3bFiles(rd *runData, id, name string) []check {
	var out []check
	for _, p := range rd.m3b[name] {
		rel, _ := filepath.Rel(rd.dir, p)
		out = append(out, readM3b(p, id+" "+rel))
	}
	if len(out) == 0 {
		out = []check{{ID: id + " " + name, Status: statusNeedsM3b}}
	}
	return out
}

// readM3b reads the top-level "check" object M3b writes next to its data.
func readM3b(path, id string) check {
	var v struct {
		Check *check `json:"check"`
	}
	if err := readJSON(path, &v); err != nil {
		return check{ID: id, Status: statusMissing, Detail: err.Error()}
	}
	if v.Check == nil {
		return check{ID: id, Status: statusMissing, Detail: "no top-level check"}
	}
	c := *v.Check
	if c.Desc == "" {
		c.Desc = c.ID
	}
	c.ID = id
	switch c.Status {
	case statusPass, statusFail, statusMissing, statusNA:
	default:
		c.Detail = strings.TrimSpace(fmt.Sprintf("unknown status %q %s", c.Status, c.Detail))
		c.Status = statusMissing
	}
	return c
}

func e2Check(rd, ctl *runData) check {
	const id, desc = "E2", "old-node health vs control"
	sub := []check{badPacketCheck(rd, "E2a"), templatesCheck(rd, ctl), removalCheck(rd), mediansCheck(rd, ctl)}
	c := top(id, desc, sub)
	if ctl != nil {
		if why := controlProblem(rd, ctl); why != "" {
			var failed []string
			for _, x := range sub {
				if x.Status == statusFail {
					failed = append(failed, x.ID)
				}
			}
			c.Status, c.Detail = statusMissing, why
			if len(failed) > 0 {
				c.Detail += "; FAIL so far: " + strings.Join(failed, ", ")
			}
		}
	}
	return c
}

// badPacketCheck needs every OLD node's log and its discover/v5/bad reading in every
// metrics.json of the run.
func badPacketCheck(rd *runData, id string) check {
	c := check{ID: id, Desc: "zero Bad discv5 packet lines and discover/v5/bad on OLD nodes"}
	olds, ok := rd.population(kindOld, false)
	if !ok {
		c.Status, c.Detail = statusMissing, "no inventory-check.json"
		return c
	}
	logs, missing := rd.logsFor(olds)
	var fails []string
	for _, n := range olds {
		if l, ok := logs[n.Name]; ok && l.BadPacket > 0 {
			fails = append(fails, fmt.Sprintf("%s: %d lines %v", n.Name, l.BadPacket, l.BadPacketErrs))
		}
	}
	readings := 0
	if len(rd.metrics) == 0 {
		missing = append(missing, "no metrics.json")
	}
	for _, rel := range rd.subdirs {
		m, ok := rd.metrics[rel]
		if !ok {
			continue
		}
		mn := metricsNodes(m)
		for _, n := range olds {
			v, ok := mn[n.Name].Series["discover/v5/bad.count"]
			if !ok || mn[n.Name].Error != "" {
				missing = append(missing, fmt.Sprintf("%s %s: no discover/v5/bad", rel, n.Name))
				continue
			}
			readings++
			if v > 0 {
				fails = append(fails, fmt.Sprintf("%s %s: discover/v5/bad=%.0f", rel, n.Name, v))
			}
		}
	}
	judge(&c, fails, missing, fmt.Sprintf("%d old nodes, %d logs, %d meter readings", len(olds), len(logs), readings))
	return c
}

// controlProblem says why ctl cannot serve as the all-OLD control of rd.
func controlProblem(rd, ctl *runData) string {
	if rd.inv == nil || ctl.inv == nil {
		return "no inventory-check.json in run or control"
	}
	var why []string
	if len(ctl.inv.Nodes) != len(rd.inv.Nodes) {
		why = append(why, fmt.Sprintf("control has %d nodes, run %d", len(ctl.inv.Nodes), len(rd.inv.Nodes)))
	}
	if ctl.inv.New > 0 {
		why = append(why, fmt.Sprintf("control has %d NEW nodes", ctl.inv.New))
	}
	return strings.Join(why, "; ")
}

func templatesCheck(rd, ctl *runData) check {
	c := check{ID: "E2b", Desc: "OLD-node WARN/ERROR templates ⊆ control's"}
	if ctl == nil {
		c.Status = statusNeedsControl
		return c
	}
	if why := controlProblem(rd, ctl); why != "" {
		c.Status, c.Detail = statusMissing, why
		return c
	}
	// The control's participant 0 is OLD; compare it only when the run's is OLD too.
	olds, _ := rd.population(kindOld, false)
	base, _ := ctl.population(kindOld, rd.inv.BootKind != kindOld)
	logs, missing := rd.logsFor(olds)
	clogs, cmissing := ctl.logsFor(base)
	for _, m := range cmissing {
		missing = append(missing, "control "+m)
	}
	baseT := make(map[string]bool)
	for _, l := range clogs {
		for t := range l.Templates {
			baseT[t] = true
		}
	}
	extra := make(map[string][]string)
	have := make(map[string]bool)
	for _, l := range logs {
		for t := range l.Templates {
			have[t] = true
			if !baseT[t] {
				extra[t] = append(extra[t], l.Name)
			}
		}
	}
	var fails []string
	for t, names := range extra {
		sort.Strings(names)
		fails = append(fails, fmt.Sprintf("%q on %s", t, strings.Join(names, ",")))
	}
	sort.Strings(fails)
	judge(&c, fails, missing, fmt.Sprintf("%d templates on %d old nodes, control %d on %d", len(have), len(logs), len(baseT), len(clogs)))
	return c
}

// removalMin is the number of removals an OLD node needs before its rate is judged.
const removalMin = 5

// removalCheck compares, per OLD node (participant 0 excluded), removals of NEW peers per NEW
// node with 2× removals of OLD peers per other OLD node; A against B in a split run. It only
// informs (D117): every removal comes from geth's random eth-layer dropper, which never sees
// discovery. Incomplete data is still missing.
func removalCheck(rd *runData) check {
	g := rd.names()
	c := check{ID: "E2c", Desc: fmt.Sprintf("per %s node: %s↔%s removal rate vs 2× %s↔%s, info only: geth's random peer dropper, not discovery (participant 0 excluded, from %d removals)",
		upper(g.old), g.old, g.new, g.old, g.old, removalMin)}
	olds, ok := rd.groupNodes(kindOld, true)
	if !ok {
		c.Status, c.Detail = statusMissing, "no inventory-check.json"
		return c
	}
	news, _ := rd.groupNodes(kindNew, false)
	all, _ := rd.groupNodes(kindOld, false)
	nOld := len(all)
	logs, missing := rd.logsFor(olds)
	var above, info []string
	judged := 0
	for _, n := range olds {
		l, ok := logs[n.Name]
		if !ok {
			continue
		}
		if l.Levels["DEBUG"] == 0 {
			missing = append(missing, n.Name+": no DEBUG lines")
			continue
		}
		rm, why := rd.removed(l)
		if why != "" {
			missing = append(missing, n.Name+": "+why)
			continue
		}
		ro, rn := rm[kindOld], rm[kindNew]
		counts := fmt.Sprintf("%s %s %d %s %d unknown %d", n.Name, g.old, ro, g.new, rn, rm[kindUnknown])
		if ro+rn < removalMin {
			info = append(info, counts)
			continue
		}
		judged++
		rateNew := float64(rn) / float64(max(len(news), 1))
		rateOld := float64(ro) / float64(max(nOld-1, 1))
		if rateNew > 2*rateOld {
			above = append(above, fmt.Sprintf("%s (%.2f vs %.2f per node)", counts, rateNew, rateOld))
		}
	}
	c.Status, c.Detail = statusInfo, fmt.Sprintf("%d nodes compared, none above 2×", judged)
	switch {
	case len(above) > 0:
		c.Detail = fmt.Sprintf("%d nodes compared; above 2×: %s (info: random eth dropper, not affected by discovery)", judged, firstN(above, 4))
	case judged == 0:
		c.Detail = "too few removals to compare"
	}
	if len(missing) > 0 {
		c.Status, c.Detail = statusMissing, "incomplete: "+firstN(missing, 4)+"; "+c.Detail
	}
	if len(info) > 0 {
		c.Detail += fmt.Sprintf("; informational (< %d removals): %s", removalMin, firstN(info, 4))
	}
	return c
}

func median(sorted []float64) float64 {
	n := len(sorted)
	if n == 0 {
		return math.NaN()
	}
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

// snapTolerance is how far a snapshot's age after genesis may be from its nominal t<m>m,
// or from the control's age at the same location.
const snapTolerance = 60.0

// oldMedians takes the medians over every node of olds; any missing reading is reported.
func oldMedians(m *metricsResult, olds []node) (map[string]float64, []string) {
	mn := metricsNodes(m)
	med := make(map[string]float64)
	var missing []string
	for _, s := range e2Series {
		var vals []float64
		for _, n := range olds {
			x, ok := mn[n.Name]
			v, has := x.Series[s]
			if s == "rss" && x.RSSBytes != nil {
				v, has = *x.RSSBytes, true
			}
			if !ok || x.Error != "" || !has {
				missing = append(missing, n.Name+" "+s)
				continue
			}
			vals = append(vals, v)
		}
		if len(vals) == len(olds) && len(olds) > 0 {
			sort.Float64s(vals)
			med[s] = median(vals)
		}
	}
	return med, missing
}

func ageCheck(rel string, run, ctl *float64) string {
	if run == nil || ctl == nil {
		return "no elapsed_s (genesis.txt) in run or control"
	}
	if m := snapMinutes(rel); m >= 0 {
		for who, e := range map[string]float64{"run": *run, "control": *ctl} {
			if math.Abs(e-float64(m*60)) > snapTolerance {
				return fmt.Sprintf("%s age %.0fs, nominal %ds", who, e, m*60)
			}
		}
		return ""
	}
	if math.Abs(*run-*ctl) > snapTolerance {
		return fmt.Sprintf("ages %.0fs vs control %.0fs", *run, *ctl)
	}
	return ""
}

func mediansCheck(rd, ctl *runData) check {
	c := check{ID: "E2d", Desc: "OLD-node medians within ±25 % of control at the same snapshot (participant 0 excluded)"}
	if ctl == nil {
		c.Status = statusNeedsControl
		return c
	}
	if why := controlProblem(rd, ctl); why != "" {
		c.Status, c.Detail = statusMissing, why
		return c
	}
	olds, _ := rd.population(kindOld, true)
	base, _ := ctl.population(kindOld, true)
	locs := make(map[string]bool)
	for rel := range rd.metrics {
		locs[rel] = true
	}
	for rel := range ctl.metrics {
		locs[rel] = true
	}
	var rels []string
	for rel := range locs {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	var sub []check
	for _, rel := range rels {
		sc := check{ID: "E2d " + rel}
		m, ok1 := rd.metrics[rel]
		cm, ok2 := ctl.metrics[rel]
		switch {
		case !ok1:
			sc.Status, sc.Detail = statusMissing, "metrics.json only in the control"
		case !ok2:
			sc.Status, sc.Detail = statusMissing, "metrics.json only in the run"
		default:
			if why := ageCheck(rel, m.Elapsed, cm.Elapsed); why != "" {
				sc.Status, sc.Detail = statusMissing, "not comparable: "+why
				break
			}
			run, missing := oldMedians(m, olds)
			ctlMed, cmissing := oldMedians(cm, base)
			for _, x := range cmissing {
				missing = append(missing, "control "+x)
			}
			var parts, fails []string
			for _, s := range e2Series {
				r, okr := run[s]
				b, okb := ctlMed[s]
				if !okr || !okb {
					continue
				}
				parts = append(parts, fmt.Sprintf("%s %.4g/%.4g", s, r, b))
				if math.Abs(r-b) > 0.25*math.Abs(b) {
					fails = append(fails, fmt.Sprintf("%s %.4g vs %.4g", s, r, b))
				}
			}
			judge(&sc, fails, missing, fmt.Sprintf("%d vs %d old nodes: %s", len(olds), len(base), strings.Join(parts, ", ")))
		}
		sub = append(sub, sc)
	}
	if len(sub) == 0 {
		c.Status, c.Detail = statusMissing, "no metrics.json"
		return c
	}
	c.Status, c.Sub = overall(sub), sub
	return c
}

func e3Check(rd *runData) check {
	var peers []check
	for _, rel := range rd.subdirs {
		if p, ok := rd.peers[rel]; ok {
			pc := p.Check
			if rd.split != nil {
				pc = rd.splitPeers(p)
			}
			pc.ID = "E3a " + rel
			peers = append(peers, pc)
		} else if rel != "." {
			peers = append(peers, check{ID: "E3a " + rel, Status: statusMissing, Detail: "no peers.json"})
		}
	}
	g := rd.names()
	pc := check{ID: "E3a", Desc: fmt.Sprintf("every %s node has a %s peer and vice versa", upper(g.old), upper(g.new)),
		Status: statusMissing, Detail: "peers.json"}
	if len(peers) > 0 {
		pc.Status, pc.Detail, pc.Sub = overall(peers), "", peers
	}
	tc := check{ID: "E3b", Desc: "tx gossip", Status: statusMissing, Detail: "txs.json"}
	if rd.txs != nil {
		tc = rd.txs.Check
	}
	hc := check{ID: "E3c", Desc: "heads", Status: statusMissing, Detail: "heads.json"}
	if len(rd.headSamples) > 0 && rd.inv != nil {
		hc, _ = evalHeads(rd.headSamples, len(rd.inv.Nodes))
	} else if rd.heads != nil {
		hc = rd.heads.Check
	}
	rc := check{ID: "E3d", Desc: "zero Chain reorg detected after warm-up"}
	if rd.inv == nil {
		rc.Status, rc.Detail = statusMissing, "no inventory-check.json"
	} else {
		logs, missing := rd.logsFor(rd.inv.Nodes)
		var fails []string
		total := 0
		for _, n := range rd.inv.Nodes {
			l := logs[n.Name]
			total += l.Reorgs
			if l.ReorgsAfterWarmup > 0 {
				fails = append(fails, fmt.Sprintf("%s: %d after warm-up", n.Name, l.ReorgsAfterWarmup))
			}
		}
		judge(&rc, fails, missing, fmt.Sprintf("0 after warm-up, %d in total on %d ELs", total, len(logs)))
	}
	return top("E3", "peering, tx gossip, heads", []check{pc, tc, hc, rc})
}

var e4Meters = []string{
	"discover/ingress/REGCONFIRMATION/v5.count", "discover/ingress/TOPICNODES/v5.count",
	"discover/ingress/REGTOPIC/v5.count", "discover/ingress/TOPICQUERY/v5.count",
}

func e4Check(rd *runData, single bool) check {
	const id, desc = "E4", "topic search and registration"
	if single {
		return check{ID: id, Desc: desc, Status: statusNA, Detail: "single-kind run"}
	}
	topic := m3bTop("E4ab", "topic search from a NEW and an OLD seed (topic.json)", m3bFiles(rd, "E4ab", "topic.json"))
	news, ok := rd.population(kindNew, false)
	mc := check{ID: "E4c", Desc: "NEW nodes: topic meters > 0, discover/regtopic/* == 0"}
	lc := check{ID: "E4d", Desc: "NEW-node logs: ≥ 1 registration ok, 0 failed, ≥ 1 query answered n>0"}
	if !ok {
		mc.Status, mc.Detail = statusMissing, "no inventory-check.json"
		lc.Status, lc.Detail = statusMissing, "no inventory-check.json"
		return m3bTop(id, desc, []check{topic, mc, lc, badPacketCheck(rd, "E4e")})
	}
	if m := rd.latestMetrics(); m == nil {
		mc.Status, mc.Detail = statusMissing, "no metrics.json"
	} else {
		mn := metricsNodes(m)
		var fails, missing []string
		for _, n := range news {
			x, ok := mn[n.Name]
			if !ok || x.Error != "" {
				missing = append(missing, n.Name+": no metrics "+x.Error)
				continue
			}
			for _, s := range e4Meters {
				short := strings.TrimSuffix(strings.TrimPrefix(s, "discover/ingress/"), "/v5.count")
				switch v, has := x.Series[s]; {
				case !has:
					missing = append(missing, n.Name+": no "+short)
				case v <= 0:
					fails = append(fails, fmt.Sprintf("%s: %s=%v", n.Name, short, v))
				}
			}
			counters := 0
			for k, v := range x.Series {
				if strings.HasPrefix(k, "discover/regtopic/") {
					counters++
					if v != 0 {
						fails = append(fails, fmt.Sprintf("%s: %s=%v", n.Name, k, v))
					}
				}
			}
			if counters == 0 {
				missing = append(missing, n.Name+": no discover/regtopic/* counters")
			}
		}
		sort.Strings(fails)
		judge(&mc, fails, missing, fmt.Sprintf("%d new nodes", len(news)))
	}
	logs, missing := rd.logsFor(news)
	var fails []string
	for _, n := range news {
		l, ok := logs[n.Name]
		if ok && (l.TopicRegOK == 0 || l.TopicRegFailed > 0 || l.TopicQueryNonzero == 0) {
			fails = append(fails, fmt.Sprintf("%s: ok=%d failed=%d answered n>0=%d/%d", n.Name,
				l.TopicRegOK, l.TopicRegFailed, l.TopicQueryNonzero, l.TopicQuery))
		}
	}
	judge(&lc, fails, missing, fmt.Sprintf("%d new nodes", len(news)))
	return m3bTop(id, desc, []check{topic, mc, lc, badPacketCheck(rd, "E4e")})
}

func flatten(cs []check, depth int, f func(c check, depth int)) {
	for _, c := range cs {
		f(c, depth)
		flatten(c.Sub, depth+1, f)
	}
}

func renderTable(cs []check) string {
	var b strings.Builder
	flatten(cs, 0, func(c check, depth int) {
		label := strings.Repeat("  ", depth) + c.ID
		fmt.Fprintf(&b, "%-20s %-13s %s\n", label, c.Status, checkText(c, " — "))
	})
	return b.String()
}

func checkText(c check, sep string) string {
	if c.Desc == "" || c.Detail == "" {
		return c.Desc + c.Detail
	}
	return c.Desc + sep + c.Detail
}

func renderSummary(r reportResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# d79check report\n\nRun: `%s`", r.Run)
	if r.Control != "" {
		fmt.Fprintf(&b, ", control: `%s`", r.Control)
	}
	if r.Split != nil {
		b.WriteString("\n\n" + r.Split.line())
	}
	b.WriteString("\n\n| check | status | detail |\n|---|---|---|\n")
	flatten(r.Checks, 0, func(c check, depth int) {
		text := checkText(c, ": ")
		id := c.ID
		if depth == 0 {
			id = "**" + id + "**"
		}
		fmt.Fprintf(&b, "| %s%s | %s | %s |\n", strings.Repeat("&nbsp;&nbsp;", depth), id, c.Status, mdEscape(text))
	})
	return b.String()
}

func mdEscape(s string) string {
	return strings.ReplaceAll(s, "|", `\|`)
}
