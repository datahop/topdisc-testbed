package main

import (
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

func init() {
	register("crawl-reach", "E1(c): stock devp2p crawls from one OLD and one NEW ENR reach every node", runCrawlReach)
}

type reachResult struct {
	Tool      string    `json:"tool"`
	Seed      node      `json:"seed"`
	Start     time.Time `json:"start"`
	Timeout   string    `json:"timeout"`
	Reached   int       `json:"reached"`
	Of        int       `json:"of"`
	Missing   []string  `json:"missing"`
	Others    int       `json:"others"` // records not in the inventory (host probes)
	TFullS    *float64  `json:"t_full_s"`
	Pass      bool      `json:"pass"`
	Err       string    `json:"err,omitempty"`
	NodesFile string    `json:"nodes_file"`
}

// crawlNode is one record of devp2p's nodes.json.
type crawlNode struct {
	N             *enode.Node `json:"record"`
	FirstResponse time.Time   `json:"firstResponse"`
}

func runCrawlReach(args []string) error {
	var rf probeFlags
	fs := flag.NewFlagSet("crawl-reach", flag.ExitOnError)
	rf.register(fs)
	portOld := fs.Int("port-old", 31410, "UDP port of the crawler seeded with an OLD node")
	portNew := fs.Int("port-new", 31411, "UDP port of the crawler seeded with a NEW node")
	timeout := fs.Duration("timeout", 5*time.Minute, "crawl time limit, also the reach deadline")
	fs.Parse(args)

	inv, ip, err := rf.setup()
	if err != nil {
		return err
	}
	dir := filepath.Join(rf.out, "crawl-reach")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	type job struct {
		kind, tool, role string
		port             int
	}
	jobs := []job{
		{"old", "devp2p-" + oldDevp2pVersion, "crawl-old", *portOld},
		{"new", "devp2p-" + oldDevp2pVersion, "crawl-new", *portNew},
	}
	var (
		wg      sync.WaitGroup
		results = make([]*reachResult, len(jobs))
	)
	for i, j := range jobs {
		seed, ok := inv.first(j.kind)
		if !ok {
			fmt.Printf("no %s node, skipping %s\n", j.kind, j.tool)
			continue
		}
		if err := checkPort(j.port); err != nil {
			return err
		}
		nodesFile := filepath.Join(dir, j.kind+".nodes.json")
		if err := freshFile(nodesFile); err != nil {
			return err
		}
		r := &reachResult{Tool: j.tool, Seed: seed, Timeout: timeout.String(), Of: len(inv.Nodes), NodesFile: nodesFile}
		results[i] = r
		wg.Add(1)
		go func() {
			defer wg.Done()
			addr := net.JoinHostPort(ip.String(), strconv.Itoa(j.port))
			key := hex.EncodeToString(crypto.FromECDSA(fixedKey(j.role)))
			stopped, err := recordProbe(rf.run, j.role, fixedKey(j.role))
			if err != nil {
				r.Err = err.Error()
				return
			}
			r.Start = time.Now().UTC()
			err = runLogged(filepath.Join(dir, j.kind+".log"), filepath.Join(rf.devnet, "bin", j.tool),
				"--verbosity", "3", "discv5", "crawl", "--addr", addr, "--nodekey", key,
				"--bootnodes", seed.ENR, "--timeout", timeout.String(), nodesFile)
			stopped()
			if err != nil {
				r.Err = err.Error()
				return
			}
			if err := evalReach(r, inv, *timeout); err != nil {
				r.Err = err.Error()
			}
		}()
	}
	fmt.Printf("crawling for %v from %s\n", *timeout, ip)
	wg.Wait()

	out := struct {
		Check  check                   `json:"check"`
		Crawls map[string]*reachResult `json:"crawls"`
	}{Crawls: map[string]*reachResult{}}
	var sub []check
	for i, j := range jobs {
		sub = append(sub, reachCheck(j.kind, results[i]))
		if r := results[i]; r != nil {
			out.Crawls[j.kind] = r
			t := "never"
			if r.TFullS != nil {
				t = fmt.Sprintf("%.0f s", *r.TFullS)
			}
			fmt.Printf("%-18s seed %-3s %-24s reached %d/%d  full at %s  others %d  pass %v %s\n",
				r.Tool, r.Seed.Kind, r.Seed.Name, r.Reached, r.Of, t, r.Others, r.Pass, r.Err)
		}
	}
	out.Check = check{ID: "E1c", Desc: "a crawl from one OLD and one NEW node reaches every node", Status: overall(sub), Sub: sub}
	fmt.Print(renderTable([]check{out.Check}))
	return writeJSON(filepath.Join(rf.out, "crawl-reach.json"), out)
}

func reachCheck(kind string, r *reachResult) check {
	c := check{ID: "E1c " + kind, Desc: "crawl seeded with one " + strings.ToUpper(kind) + " node"}
	switch {
	case r == nil:
		c.Status, c.Detail = statusNA, "no "+strings.ToUpper(kind)+" node"
	case r.Err != "":
		c.Status, c.Detail = statusMissing, r.Tool+": "+r.Err
	case r.Pass:
		c.Status, c.Detail = statusPass, fmt.Sprintf("%s reached %d/%d in %.0f s", r.Tool, r.Reached, r.Of, *r.TFullS)
	default:
		c.Status, c.Detail = statusFail, fmt.Sprintf("%s reached %d/%d within %s", r.Tool, r.Reached, r.Of, r.Timeout)
		if len(r.Missing) > 0 {
			c.Detail += ", missing " + strings.Join(r.Missing, ", ")
		}
	}
	return c
}

// evalReach reads the crawl's nodes.json and counts the inventory nodes it reached.
func evalReach(r *reachResult, inv *inventory, timeout time.Duration) error {
	data, err := os.ReadFile(r.NodesFile)
	if err != nil {
		return err
	}
	var nodes map[string]crawlNode
	if err := json.Unmarshal(data, &nodes); err != nil {
		return fmt.Errorf("%s: %v", r.NodesFile, err)
	}
	reach := map[string]time.Time{}
	for _, n := range nodes {
		if n.N != nil {
			reach[n.N.ID().String()] = n.FirstResponse
		}
	}
	scoreReach(r, inv, reach, timeout)
	return nil
}

func scoreReach(r *reachResult, inv *inventory, reach map[string]time.Time, timeout time.Duration) {
	r.Missing = []string{}
	known := inv.nodesByID()
	var last time.Time
	for _, n := range inv.Nodes {
		t, ok := reach[n.ID]
		if !ok {
			r.Missing = append(r.Missing, n.Name)
			continue
		}
		r.Reached++
		if t.After(last) {
			last = t
		}
	}
	for id := range reach {
		if _, ok := known[id]; !ok {
			r.Others++
		}
	}
	if len(r.Missing) == 0 && r.Reached > 0 {
		// firstResponse is truncated to the second, so the start is too.
		d := max(0, last.Sub(r.Start.Truncate(time.Second)).Seconds())
		r.TFullS = &d
		r.Pass = d <= timeout.Seconds()
	}
}
