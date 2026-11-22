package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
)

func init() {
	register("tables", "E1(a,b): read every node's discv5 table with cmd/crawl, classify entries by kind", runTables)
}

// crawlTables is the part of tools/crawl_tables.py's output that is used here.
type crawlTables struct {
	TEnd     int64                `json:"t_end"`
	Read     int                  `json:"peers_read"`
	Partial  int                  `json:"peers_partial"`
	AliveIDs int                  `json:"alive_ids"`
	Nodes    map[string]crawlRead `json:"nodes"`
}

type crawlRead struct {
	T       int64                   `json:"t"`
	Buckets map[string][]crawlEntry `json:"buckets"`
}

type crawlEntry struct {
	ID    string `json:"id"`
	Alive bool   `json:"alive"`
}

type tableEntry struct {
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`
	Kind   string `json:"kind"` // old, new or other (not in the inventory: host probes, strangers)
	Alive  bool   `json:"alive"`
	Bucket int    `json:"bucket"`
}

type nodeTable struct {
	Name     string       `json:"name"`
	Kind     string       `json:"kind"`
	ID       string       `json:"id"`
	Read     bool         `json:"read"`
	T        int64        `json:"t,omitempty"`
	Entries  int          `json:"entries"`
	Dead     int          `json:"dead"`
	New      int          `json:"new"`
	Old      int          `json:"old"`
	Other    int          `json:"other"`
	NewShare float64      `json:"new_share"`
	P        float64      `json:"p"`
	Band     [2]int       `json:"band"`
	Outside  bool         `json:"outside"`
	Table    []tableEntry `json:"table"`
}

type tablesResult struct {
	Check   check       `json:"check"`
	Time    time.Time   `json:"time"`
	N       int         `json:"n"`
	NNew    int         `json:"n_new"`
	NOld    int         `json:"n_old"`
	Nodes   []nodeTable `json:"nodes"`
	Unread  []string    `json:"unread"`
	E1a     e1a         `json:"e1a"`
	E1b     e1b         `json:"e1b"`
	Crawl   any         `json:"crawl,omitempty"`
	Partial int         `json:"peers_partial"`
}

type e1a struct {
	Applicable bool     `json:"applicable"`
	Pass       bool     `json:"pass"`
	Failing    []string `json:"failing"`
}

type e1b struct {
	Conf    float64  `json:"conf"`
	Outside []string `json:"outside"`
	Empty   []string `json:"empty"` // read, but no live OLD or NEW entry
}

const bandConf = 0.99

func runTables(args []string) error {
	var rf probeFlags
	fs := flag.NewFlagSet("tables", flag.ExitOnError)
	rf.register(fs)
	port := fs.Int("port", 31400, "UDP port of the crawler on the bridge IP")
	sample := fs.Duration("sample", 2*time.Minute, "cmd/crawl -sample")
	duration := fs.Duration("duration", 6*time.Minute, "cmd/crawl -duration")
	events := fs.String("events", "", "analyse this events.jsonl instead of crawling")
	python := fs.String("python", "python3", "python interpreter for tools/crawl_tables.py")
	fs.Parse(args)

	inv, ip, err := rf.setup()
	if err != nil {
		return err
	}
	dir := filepath.Join(rf.out, "tables-crawl")
	crawlInfo := map[string]any{}
	if *events == "" {
		if err := checkPort(*port); err != nil {
			return err
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		*events = filepath.Join(dir, "events.jsonl")
		if err := freshFile(*events); err != nil {
			return err
		}
		listen := net.JoinHostPort(ip.String(), strconv.Itoa(*port))
		crawlArgs, err := prepareCrawl(dir, inv, listen, ip.String(), *sample, *duration)
		if err != nil {
			return err
		}
		crawlInfo["args"] = crawlArgs
		crawlInfo["start"] = time.Now().UTC()
		fmt.Printf("crawling tables from %s for %v\n", listen, *duration)
		stopped, err := recordProbe(rf.run, "tables", fixedKey("tables"))
		if err != nil {
			return err
		}
		err = runLogged(filepath.Join(dir, "crawl.log"), filepath.Join(rf.devnet, "bin", "crawl"), crawlArgs...)
		stopped()
		if err != nil {
			return fmt.Errorf("crawl: %v (see %s/crawl.log)", err, dir)
		}
		crawlInfo["end"] = time.Now().UTC()
	} else if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	crawlInfo["events"] = *events

	ctPath := filepath.Join(dir, "crawl_tables.json")
	script := filepath.Join(filepath.Dir(rf.devnet), "tools", "crawl_tables.py")
	out, err := exec.Command(*python, "-I", script, *events, "--exclude-last", "0m", "--out", ctPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("crawl_tables.py: %v\n%s", err, out)
	}
	var ct crawlTables
	data, err := os.ReadFile(ctPath)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &ct); err != nil {
		return fmt.Errorf("%s: %v", ctPath, err)
	}

	res := classifyTables(inv, &ct)
	res.Check = tablesCheck(res, rf.out, oldNew)
	res.Crawl = crawlInfo
	if err := writeJSON(filepath.Join(rf.out, "tables.json"), res); err != nil {
		return err
	}
	printTables(res)
	return nil
}

// prepareCrawl writes the crawler's fixed key and bootnode file and returns its arguments.
func prepareCrawl(dir string, inv *inventory, listen, extip string, sample, duration time.Duration) ([]string, error) {
	if err := crypto.SaveECDSA(filepath.Join(dir, "key"), fixedKey("tables")); err != nil {
		return nil, err
	}
	var b strings.Builder
	for _, n := range inv.Nodes {
		b.WriteString(n.ENR + "\n")
	}
	bn := filepath.Join(dir, "bootnodes.txt")
	if err := os.WriteFile(bn, []byte(b.String()), 0o644); err != nil {
		return nil, err
	}
	return []string{
		"-sample-full", "-no-td", "-listen", listen, "-extip", extip, "-bootnodes", bn,
		"-sample", sample.String(), "-duration", duration.String(), "-out", dir,
	}, nil
}

func runLogged(logPath, bin string, args ...string) error {
	f, err := os.Create(logPath)
	if err != nil {
		return err
	}
	defer f.Close()
	cmd := exec.Command(bin, args...)
	cmd.Stdout, cmd.Stderr = f, f
	return cmd.Run()
}

// classifyTables puts each inventory node's live table entries into old, new and other
// and evaluates E1(a) and E1(b).
func classifyTables(inv *inventory, ct *crawlTables) *tablesResult {
	kinds := inv.nodesByID()
	res := &tablesResult{
		Time:    time.UnixMilli(ct.TEnd).UTC(),
		N:       len(inv.Nodes),
		NNew:    inv.count("new"),
		NOld:    inv.count("old"),
		Partial: ct.Partial,
		Unread:  []string{},
		E1a:     e1a{Failing: []string{}},
		E1b:     e1b{Conf: bandConf, Outside: []string{}, Empty: []string{}},
	}
	res.E1a.Applicable = res.NNew > 0 && res.NOld > 0
	for _, n := range inv.Nodes {
		nt := nodeTable{Name: n.Name, Kind: n.Kind, ID: n.ID, Table: []tableEntry{}}
		read, ok := ct.Nodes[n.ID]
		if !ok {
			res.Unread = append(res.Unread, n.Name)
			res.Nodes = append(res.Nodes, nt)
			continue
		}
		nt.Read, nt.T = true, read.T
		for b, entries := range read.Buckets {
			bucket, _ := strconv.Atoi(b)
			for _, e := range entries {
				te := tableEntry{ID: e.ID, Kind: "other", Alive: e.Alive, Bucket: bucket}
				if k, ok := kinds[e.ID]; ok {
					te.Name, te.Kind = k.Name, k.Kind
				}
				nt.Table = append(nt.Table, te)
				nt.Entries++
				switch {
				case !e.Alive:
					nt.Dead++
				case te.Kind == "new":
					nt.New++
				case te.Kind == "old":
					nt.Old++
				default:
					nt.Other++
				}
			}
		}
		sort.Slice(nt.Table, func(i, j int) bool {
			if nt.Table[i].Bucket != nt.Table[j].Bucket {
				return nt.Table[i].Bucket > nt.Table[j].Bucket
			}
			return nt.Table[i].ID < nt.Table[j].ID
		})
		// Another node of the same kind is one fewer candidate: a NEW node can hold N_new-1 NEW nodes.
		others := res.N - 1
		newOthers := res.NNew
		if n.Kind == "new" {
			newOthers--
		}
		if others > 0 {
			nt.P = float64(newOthers) / float64(others)
		}
		if live := nt.New + nt.Old; live > 0 {
			nt.NewShare = float64(nt.New) / float64(live)
			lo, hi := binomialBand(live, nt.P, bandConf)
			nt.Band = [2]int{lo, hi}
			nt.Outside = nt.New < lo || nt.New > hi
		} else {
			res.E1b.Empty = append(res.E1b.Empty, n.Name)
		}
		if nt.Outside {
			res.E1b.Outside = append(res.E1b.Outside, n.Name)
		}
		if res.E1a.Applicable && ((n.Kind == "old" && nt.New == 0) || (n.Kind == "new" && nt.Old == 0)) {
			res.E1a.Failing = append(res.E1a.Failing, n.Name)
		}
		res.Nodes = append(res.Nodes, nt)
	}
	if res.E1a.Applicable {
		res.E1a.Pass = len(res.E1a.Failing) == 0 && len(res.Unread) == 0
	}
	return res
}

var snapLabel = regexp.MustCompile(`^t(\d+)m$`)

// tablesCheck judges E1(a) and E1(b); snapDir is the output directory, whose name
// is the snapshot label (t<m>m) in a run. E1(a) is judged only from t30m on.
func tablesCheck(res *tablesResult, snapDir string, g groupNames) check {
	o, n := upper(g.old), upper(g.new)
	a := check{ID: "E1a", Desc: fmt.Sprintf("every %s table holds a %s node and every %s table an %s node", o, n, n, o)}
	b := check{ID: "E1b", Desc: fmt.Sprintf("per-node %s count inside the %.0f %% binomial band", n, bandConf*100)}
	unread := ""
	if len(res.Unread) > 0 {
		unread = "not read: " + strings.Join(res.Unread, ", ")
	}
	judge := func(c *check, fails []string, ok string) {
		switch {
		case len(fails) > 0:
			c.Status, c.Detail = statusFail, strings.Join(fails, ", ")
			if unread != "" {
				c.Detail += "; " + unread
			}
		case unread != "":
			c.Status, c.Detail = statusMissing, unread
		default:
			c.Status, c.Detail = statusPass, ok
		}
	}
	if !res.E1a.Applicable {
		a.Status, a.Detail = statusNA, "single-kind run"
		b.Status, b.Detail = statusNA, "single-kind run"
	} else {
		judge(&a, res.E1a.Failing, fmt.Sprintf("%d tables", res.N))
		// E1(b) is reported, not judged, unless a table is missing or empty.
		var gaps []string
		if unread != "" {
			gaps = append(gaps, unread)
		}
		if len(res.E1b.Empty) > 0 {
			gaps = append(gaps, "no live entries: "+strings.Join(res.E1b.Empty, ", "))
		}
		switch {
		case len(gaps) > 0:
			b.Status, b.Detail = statusMissing, strings.Join(gaps, "; ")
		case len(res.E1b.Outside) > 0:
			b.Status, b.Detail = statusInfo, "outside: "+strings.Join(res.E1b.Outside, ", ")
		default:
			b.Status, b.Detail = statusInfo, fmt.Sprintf("all %d inside", res.N)
		}
		if m := snapLabel.FindStringSubmatch(filepath.Base(filepath.Clean(snapDir))); m != nil {
			if min, _ := strconv.Atoi(m[1]); min < 30 {
				a.Detail = strings.TrimSpace(fmt.Sprintf("before t30m, not judged (%s) %s", a.Status, a.Detail))
				a.Status = statusNA
			}
		}
	}
	return check{ID: "E1ab", Desc: fmt.Sprintf("discv5 tables mix %s and %s (judged on E1a)", o, n), Status: a.Status, Detail: a.Detail, Sub: []check{a, b}}
}

// binomialBand returns the central conf interval [lo, hi] of Binomial(n, p): the
// (1-conf)/2 and (1+conf)/2 quantiles.
func binomialBand(n int, p, conf float64) (lo, hi int) {
	tail := (1 - conf) / 2
	cdf := 0.0
	lo, hi = -1, n
	for k := 0; k <= n; k++ {
		cdf += binomialPMF(n, k, p)
		if lo < 0 && cdf >= tail-1e-12 {
			lo = k
		}
		if cdf >= 1-tail-1e-12 {
			hi = k
			break
		}
	}
	return lo, hi
}

func binomialPMF(n, k int, p float64) float64 {
	switch {
	case p <= 0:
		if k == 0 {
			return 1
		}
		return 0
	case p >= 1:
		if k == n {
			return 1
		}
		return 0
	}
	lg := func(x int) float64 { v, _ := math.Lgamma(float64(x + 1)); return v }
	return math.Exp(lg(n) - lg(k) - lg(n-k) + float64(k)*math.Log(p) + float64(n-k)*math.Log1p(-p))
}

func printTables(res *tablesResult) {
	fmt.Printf("tables at %s: %d nodes (%d new, %d old), %d unread\n", res.Time.Format(time.RFC3339), res.N, res.NNew, res.NOld, len(res.Unread))
	for _, n := range res.Nodes {
		if !n.Read {
			fmt.Printf("  %-24s %-3s not read\n", n.Name, n.Kind)
			continue
		}
		flag := ""
		if n.Outside {
			flag = "  OUTSIDE"
		}
		fmt.Printf("  %-24s %-3s entries %2d  new %2d  old %2d  other %d  dead %d  new share %.2f  band [%d,%d] (p %.3f)%s\n",
			n.Name, n.Kind, n.Entries, n.New, n.Old, n.Other, n.Dead, n.NewShare, n.Band[0], n.Band[1], n.P, flag)
	}
	switch {
	case !res.E1a.Applicable:
		fmt.Println("E1(a) n/a (one kind only)")
	case res.E1a.Pass:
		fmt.Println("E1(a) PASS")
	default:
		fmt.Printf("E1(a) FAIL: %s\n", strings.Join(res.E1a.Failing, ", "))
	}
	fmt.Printf("E1(b) outside the %.0f%% band: %d %v\n", res.E1b.Conf*100, len(res.E1b.Outside), res.E1b.Outside)
	fmt.Print(renderTable([]check{res.Check}))
}
