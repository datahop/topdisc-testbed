package coordinator

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/datahop/topdisc-testbed/pkg/assign"
	"github.com/datahop/topdisc-testbed/pkg/host"
	"github.com/datahop/topdisc-testbed/pkg/scenario"
	"github.com/datahop/topdisc-testbed/pkg/wan"
)

// Inventory is `terraform output -json inventory` of a deploy/terraform module.
type Inventory struct {
	Coordinator string `json:"coordinator"`
	Hosts       []struct {
		Index  int    `json:"index"`
		IP     string `json:"ip"`
		Nodes  int    `json:"nodes"`
		Region string `json:"region"`
	} `json:"hosts"`
}

// RunCloud executes the scenario on the hosts of an inventory, each running a
// hostagent. Nodes are packed onto hosts in index order (node 0, the bootnode,
// on host 0); phases are absolute times so hosts only need NTP.
func RunCloud(cfg scenario.Config, runDir string) error {
	sc, cc := cfg.Scenario, cfg.Testbed.Cloud
	b, err := os.ReadFile(filepath.Join(filepath.Dir(cfg.SourcePath), cc.Inventory))
	if err != nil {
		return err
	}
	var inv Inventory
	if err := json.Unmarshal(b, &inv); err != nil {
		return err
	}
	sort.Slice(inv.Hosts, func(i, j int) bool { return inv.Hosts[i].Index < inv.Hosts[j].Index })
	capacity := 0
	for _, h := range inv.Hosts {
		capacity += h.Nodes
	}
	if n := sc.Population.Nodes; n > capacity {
		return fmt.Errorf("%d nodes but the inventory holds %d", n, capacity)
	}
	// Placement: the fleet was sized from scenario.network.regions; the first
	// host of the home region gets the bootnode, the rest are packed in
	// inventory order (region-sorted), which is fine because node indices are
	// random keys.
	if len(inv.Hosts) > 0 && inv.Hosts[0].Region != "" {
		want, have := cfg.Fleet(), map[string]int{}
		for _, h := range inv.Hosts {
			have[h.Region] += h.Nodes
		}
		for r, n := range want {
			if have[r] < n {
				return fmt.Errorf("region %s: scenario wants %d nodes, inventory has %d", r, n, have[r])
			}
		}
		home := cc.HomeRegion
		sort.SliceStable(inv.Hosts, func(i, j int) bool { return (inv.Hosts[i].Region == home) && (inv.Hosts[j].Region != home) })
	}
	hostOf, local := make([]int, sc.Population.Nodes), make([]int, sc.Population.Nodes)
	for i, h, k := 0, 0, 0; i < len(hostOf); i++ {
		if k == inv.Hosts[h].Nodes {
			h, k = h+1, 0
		}
		hostOf[i], local[i] = h, k
		k++
	}
	star := cfg.Testbed.Wan.Star()
	t0 := time.Now().Add(20 * time.Second).UnixMilli()
	as, err := assign.Generate(cfg, func(i int) assign.Host {
		ip := inv.Hosts[hostOf[i]].IP
		if star != nil {
			ip = host.NodeIP(hostOf[i], local[i])
		}
		return assign.Host{IP: ip, BasePort: 30300 + local[i], StatusOff: 10000} // TraceFile: the hostagent fills its own path
	}, t0, filepath.Dir(cfg.SourcePath))
	if err != nil {
		return err
	}
	if err := assign.Write(filepath.Join(runDir, "assignments"), as); err != nil {
		return err
	}
	placement := make([]map[string]any, len(as))
	for i := range as {
		h := inv.Hosts[hostOf[i]]
		placement[i] = map[string]any{"idx": i, "host": h.Index, "ip": h.IP, "region": h.Region}
	}
	b, _ = json.Marshal(placement)
	os.WriteFile(filepath.Join(runDir, "placement.json"), b, 0o644)
	agents := make([]agent, len(inv.Hosts))
	peers := map[int]string{}
	for i, h := range inv.Hosts {
		agents[i] = agent{fmt.Sprintf("http://%s:%d", h.IP, cc.AgentPort)}
		peers[h.Index] = h.IP
	}
	mine := make([][]assign.Assignment, len(agents))
	for _, a := range as {
		mine[hostOf[a.Idx]] = append(mine[hostOf[a.Idx]], a)
	}
	if err := each(len(agents), func(h int) error {
		if err := agents[h].prepare(prepareRequest{Host: h, Peers: peers, Wan: star, Seed: sc.Population.Seed, Verbosity: cc.Verbosity, Assignments: mine[h]}); err != nil {
			return fmt.Errorf("host %d (%s): %w", h, inv.Hosts[h].IP, err)
		}
		return nil
	}); err != nil {
		return err
	}
	printPhases("cloud", as, t0)
	byRegion := map[string]int{}
	for i := range as {
		byRegion[inv.Hosts[hostOf[i]].Region]++
	}
	fmt.Printf("hosts: %d; nodes by region: %v\n", len(agents), byRegion)
	ctl := hostControl{
		start: func(idx int) error { return agents[hostOf[idx]].call("start", idx) },
		kill:  func(idx int) bool { return agents[hostOf[idx]].call("kill", idx) == nil },
	}
	defer func() {
		for _, a := range agents {
			a.call("stop", 0)
		}
	}()
	if err := drive(as, ctl, cc.Grace); err != nil {
		return err
	}
	trDir := filepath.Join(runDir, "traces")
	os.MkdirAll(trDir, 0o755)
	var got atomic.Int32
	each(len(as), func(i int) error {
		if agents[hostOf[i]].fetch("trace", i, filepath.Join(trDir, fmt.Sprintf("node%d.json", i))) == nil {
			got.Add(1)
		}
		return nil
	})
	fmt.Printf("fetched %d/%d traces\n", got.Load(), len(as))
	return Collect(trDir, runDir, len(as))
}

type prepareRequest struct {
	Host        int                 `json:"host"`
	Peers       map[int]string      `json:"peers"`
	Wan         *wan.Star           `json:"wan"`
	Seed        int64               `json:"seed"`
	Verbosity   int                 `json:"verbosity"`
	Assignments []assign.Assignment `json:"assignments"`
}

type agent struct{ base string }

var agentClient = &http.Client{Timeout: 60 * time.Second}

func (a agent) prepare(p prepareRequest) error {
	b, _ := json.Marshal(p)
	resp, err := agentClient.Post(a.base+"/prepare", "application/json", bytes.NewReader(b))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("prepare: %s", bytes.TrimSpace(msg))
	}
	return nil
}

func (a agent) call(op string, idx int) error {
	resp, err := agentClient.Post(fmt.Sprintf("%s/%s?idx=%d", a.base, op, idx), "", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s %d: %s", op, idx, bytes.TrimSpace(msg))
	}
	return nil
}

func (a agent) fetch(op string, idx int, dst string) error {
	resp, err := agentClient.Get(fmt.Sprintf("%s/%s?idx=%d", a.base, op, idx))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s %d: %s", op, idx, resp.Status)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

// each runs f over 0..n-1 with bounded concurrency; the first error wins.
func each(n int, f func(i int) error) error {
	var wg sync.WaitGroup
	sem := make(chan struct{}, 64)
	var mu sync.Mutex
	var first error
	for i := 0; i < n; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := f(i); err != nil {
				mu.Lock()
				if first == nil {
					first = err
				}
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	return first
}
