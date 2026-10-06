package coordinator

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/datahop/topdisc-testbed/pkg/addr"
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
		Public string `json:"public_ip"` // advertised by the nodes when set; IP stays the control address
		Nodes  int    `json:"nodes"`
		Region string `json:"region"`
		Port   int    `json:"port"` // hostagent port; 0 = testbed.cloud.agent_port
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
	star := cfg.Testbed.Wan.Star()
	var pool *addr.Pool
	if sc.Population.Addresses == "crawl" {
		if star == nil {
			return fmt.Errorf("population.addresses crawl needs testbed.wan enabled: without network namespaces every node shares its machine's address")
		}
		topicIdx, model, err := assign.Topics(cfg, filepath.Dir(cfg.SourcePath))
		if err != nil {
			return err
		}
		pool, err = addr.Draw(sc.Population.Nodes, model, sc.Population.Topics, sc.Population.Seed, func(i int) int {
			if i < len(topicIdx) && len(topicIdx[i]) > 0 {
				return topicIdx[i][len(topicIdx[i])-1]
			}
			return -1
		})
		if err != nil {
			return err
		}
		n, pfx, largest := pool.Summary()
		fmt.Printf("addresses: crawl model, %d nodes over %d /24s, largest /24 holds %d\n", n, pfx, largest)
	}
	hostOf, local, err := place(cfg, inv, pool)
	if err != nil {
		return err
	}
	agents := make([]agent, len(inv.Hosts))
	peers := map[int]string{}
	for i, h := range inv.Hosts {
		port := cc.AgentPort
		if h.Port != 0 {
			port = h.Port
		}
		agents[i] = agent{fmt.Sprintf("http://%s:%d", h.IP, port)}
		peers[h.Index] = h.IP
	}
	// Instances finish cloud-init at their own pace; the phases are absolute
	// from t0, so every agent must answer before t0 is fixed.
	regions := make([]string, len(inv.Hosts))
	for i, h := range inv.Hosts {
		regions[i] = h.Region
	}
	if err := waitAgents(agents, regions, 15*time.Minute); err != nil {
		return err
	}
	t0 := time.Now().Add(20 * time.Second).UnixMilli()
	as, err := assign.Generate(cfg, func(i int) assign.Host {
		ip := inv.Hosts[hostOf[i]].IP
		switch {
		case pool != nil:
			ip = pool.IP(i).String()
		case star != nil:
			ip = host.NodeIP(hostOf[i], local[i])
		}
		h := assign.Host{IP: ip, BasePort: 30300 + local[i], StatusOff: 10000} // TraceFile: the hostagent fills its own path
		if star == nil {
			h.ExtIP = inv.Hosts[hostOf[i]].Public
		}
		return h
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
		placement[i] = map[string]any{"idx": i, "host": h.Index, "ip": h.IP, "public_ip": h.Public, "region": h.Region}
	}
	b, _ = json.Marshal(placement)
	os.WriteFile(filepath.Join(runDir, "placement.json"), b, 0o644)
	mine := make([][]assign.Assignment, len(agents))
	for _, a := range as {
		mine[hostOf[a.Idx]] = append(mine[hostOf[a.Idx]], a)
	}
	routes := prefixRoutes(pool, hostOf, inv)
	if err := each(len(agents), func(h int) error {
		req := prepareRequest{Host: h, Peers: peers, Wan: star, Seed: sc.Population.Seed, Verbosity: cc.Verbosity, SamplePeriodMs: cfg.Testbed.Traces.HostSamplePeriod.Milliseconds(), Assignments: mine[h]}
		if pool != nil {
			req.RealAddrs, req.Routes = true, routes[h]
		}
		if err := agents[h].prepare(req); err != nil {
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
	// A node writes its trace at StopAt and the agent serves it from disk;
	// a fetch that lands before the write is retried, then the node is given
	// up (a reclaimed spot instance never answers: the dial fails fast).
	var got atomic.Int32
	var failMu sync.Mutex
	failed := map[int]error{}
	each(len(as), func(i int) error {
		var err error
		for attempt := 0; attempt < 3; attempt++ {
			if err = agents[hostOf[i]].fetch("trace", i, filepath.Join(trDir, fmt.Sprintf("node%d.json", i))); err == nil {
				got.Add(1)
				return nil
			}
			time.Sleep(time.Duration(5<<attempt) * time.Second)
		}
		failMu.Lock()
		failed[i] = err
		failMu.Unlock()
		return nil
	})
	fmt.Printf("fetched %d/%d traces\n", got.Load(), len(as))
	if len(failed) > 0 {
		byHost := map[string]int{}
		var sample string
		for i, err := range failed {
			byHost[inv.Hosts[hostOf[i]].Region]++
			if sample == "" {
				sample = fmt.Sprintf("node %d: %v", i, err)
			}
		}
		fmt.Printf("traces missing: %d, by region %v; e.g. %s\n", len(failed), byHost, sample)
	}
	if cfg.Testbed.Traces.HostSamplePeriod > 0 {
		hmDir := filepath.Join(runDir, "hostmetrics")
		os.MkdirAll(hmDir, 0o755)
		all := make([][]host.Sample, len(agents))
		each(len(agents), func(h int) error {
			path := filepath.Join(hmDir, fmt.Sprintf("host%d.json", h))
			if agents[h].fetch("hostmetrics", 0, path) == nil {
				if b, err := os.ReadFile(path); err == nil {
					json.Unmarshal(b, &all[h])
				}
			}
			return nil
		})
		hostSummary(all)
	}
	return Collect(trDir, runDir, as)
}

type prepareRequest struct {
	Host           int                 `json:"host"`
	Peers          map[int]string      `json:"peers"`
	Wan            *wan.Star           `json:"wan"`
	Seed           int64               `json:"seed"`
	Verbosity      int                 `json:"verbosity"`
	SamplePeriodMs int64               `json:"sample_period_ms"`
	Assignments    []assign.Assignment `json:"assignments"`
	// Routes are the /24s that live on other machines, each via that
	// machine's address. Set only for a run on crawl addresses, where the
	// nodes' /24s are scattered across the address space and cannot be
	// summarised by one prefix per machine the way the synthetic 10.x
	// addressing is.
	Routes    []host.Route `json:"routes,omitempty"`
	RealAddrs bool         `json:"real_addrs,omitempty"`
}

type agent struct{ base string }

var healthClient = &http.Client{Timeout: 5 * time.Second}

var agentClient = &http.Client{
	Timeout:   60 * time.Second,
	Transport: &http.Transport{DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext},
}

// Prepare creates a netns, a veth pair and a netem qdisc per node, each with
// its own `ip` invocation, so it costs on the order of a minute per 300
// nodes and reports no progress until it is done. It gets its own deadline
// rather than stretching the one every other call shares.
var prepareClient = &http.Client{Timeout: 30 * time.Minute}

func (a agent) prepare(p prepareRequest) error {
	b, _ := json.Marshal(p)
	resp, err := prepareClient.Post(a.base+"/prepare", "application/json", bytes.NewReader(b))
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

// waitAgents polls every agent's health endpoint until all answer or the
// deadline passes, reporting the stragglers by region on the way.
func waitAgents(agents []agent, regions []string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	pending := make([]bool, len(agents))
	for i := range pending {
		pending[i] = true
	}
	lastReport := time.Time{}
	for {
		var mu sync.Mutex
		left := 0
		each(len(agents), func(h int) error {
			if !pending[h] {
				return nil
			}
			resp, err := healthClient.Get(agents[h].base + "/health")
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode == 200 {
					mu.Lock()
					pending[h] = false
					mu.Unlock()
					return nil
				}
			}
			mu.Lock()
			left++
			mu.Unlock()
			return nil
		})
		if left == 0 {
			return nil
		}
		byRegion := map[string]int{}
		for h, p := range pending {
			if p {
				byRegion[regions[h]]++
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%d agents never answered: %v", left, byRegion)
		}
		if time.Since(lastReport) > 30*time.Second {
			fmt.Printf("waiting for %d agents: %v\n", left, byRegion)
			lastReport = time.Now()
		}
		time.Sleep(5 * time.Second)
	}
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

// place maps every node to a host slot. Pinned nodes (scenario.network.node_regions)
// go to a host in their region; the bootnode (node 0) to the home region
// unless pinned; the rest fill the remaining slots in inventory order, which
// is region-sorted, and that is fine because node indices are random keys.
// Inventories without regions are packed in order.
// prefixRoutes gives each machine the routes to the /24s that live on other
// machines. With crawl addresses a machine's nodes are scattered across the
// address space, so there is no per-machine prefix to summarise them with and
// every other /24 needs its own route -- about 13k of them per machine, which
// a Linux FIB carries without trouble.
func prefixRoutes(pool *addr.Pool, hostOf []int, inv Inventory) [][]host.Route {
	out := make([][]host.Route, len(inv.Hosts))
	if pool == nil {
		return out
	}
	owner := map[uint32]int{}
	for pre, idxs := range pool.Groups() {
		owner[pre] = hostOf[idxs[0]] // Draw keeps a /24 on one machine
	}
	for h := range inv.Hosts {
		for pre, o := range owner {
			if o != h {
				out[h] = append(out[h], host.Route{Prefix: addr.CIDR(pre), Via: inv.Hosts[o].IP})
			}
		}
	}
	return out
}

// placeByPrefix puts every node of a /24 on one machine, filling machines in
// turn and taking the largest /24s first so a prefix with hundreds of nodes
// still fits somewhere. Keeping a /24 whole is what lets the machines route
// each other per /24 instead of per node.
func placeByPrefix(cfg scenario.Config, inv Inventory, pool *addr.Pool) (hostOf, local []int, err error) {
	n := cfg.Scenario.Population.Nodes
	hostOf, local = make([]int, n), make([]int, n)
	cap_ := make([]int, len(inv.Hosts))
	for h, hs := range inv.Hosts {
		cap_[h] = hs.Nodes
	}
	groups := pool.Groups()
	order := make([]uint32, 0, len(groups))
	for pre := range groups {
		order = append(order, pre)
	}
	sort.Slice(order, func(i, j int) bool {
		if len(groups[order[i]]) != len(groups[order[j]]) {
			return len(groups[order[i]]) > len(groups[order[j]])
		}
		return order[i] < order[j]
	})
	used := make([]int, len(inv.Hosts))
	for _, pre := range order {
		g := groups[pre]
		h := -1
		for c := range cap_ {
			if cap_[c]-used[c] >= len(g) && (h < 0 || cap_[c]-used[c] > cap_[h]-used[h]) {
				h = c
			}
		}
		if h < 0 {
			return nil, nil, fmt.Errorf("placement: /24 %s holds %d nodes, more than any machine has room for", addr.CIDR(pre), len(g))
		}
		for _, i := range g {
			hostOf[i], local[i] = h, used[h]
			used[h]++
		}
	}
	return hostOf, local, nil
}

func place(cfg scenario.Config, inv Inventory, pool *addr.Pool) (hostOf, local []int, err error) {
	if pool != nil {
		return placeByPrefix(cfg, inv, pool)
	}
	n := cfg.Scenario.Population.Nodes
	hostOf, local = make([]int, n), make([]int, n)
	type slot struct{ host, k int }
	free := map[string][]slot{}
	order := []string{}
	for h, host := range inv.Hosts {
		if _, ok := free[host.Region]; !ok {
			order = append(order, host.Region)
		}
		for k := 0; k < host.Nodes; k++ {
			free[host.Region] = append(free[host.Region], slot{h, k})
		}
	}
	take := func(region string) (slot, bool) {
		if len(free[region]) == 0 {
			return slot{}, false
		}
		s := free[region][0]
		free[region] = free[region][1:]
		return s, true
	}
	pinned := map[int]string{}
	for idx, r := range cfg.Scenario.Network.NodeRegions {
		if idx >= 0 && idx < n {
			pinned[idx] = r
		}
	}
	if _, ok := pinned[0]; !ok && inv.Hosts[0].Region != "" {
		pinned[0] = cfg.Testbed.Cloud.HomeRegion
	}
	for idx, r := range pinned {
		s, ok := take(r)
		if !ok {
			return nil, nil, fmt.Errorf("node %d pinned to %s but no free instance there", idx, r)
		}
		hostOf[idx], local[idx] = s.host, s.k
	}
	// Home region first so the bootnode's neighbours in index order are not
	// all remote; then the others.
	home := cfg.Testbed.Cloud.HomeRegion
	sort.SliceStable(order, func(i, j int) bool { return order[i] == home && order[j] != home })
	ri := 0
	for idx := 0; idx < n; idx++ {
		if _, ok := pinned[idx]; ok {
			continue
		}
		for ri < len(order) && len(free[order[ri]]) == 0 {
			ri++
		}
		if ri == len(order) {
			return nil, nil, fmt.Errorf("inventory has fewer free instances than nodes")
		}
		s, _ := take(order[ri])
		hostOf[idx], local[idx] = s.host, s.k
	}
	return hostOf, local, nil
}
