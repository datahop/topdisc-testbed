package distem

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/datahop/topdisc-testbed/pkg/scenario"
	"github.com/datahop/topdisc-testbed/pkg/wan"
)

const (
	Vnetwork  = "topdisc"
	Iface     = "if0"
	AgentPort = 9000
)

// Vnode is one planned virtual node: a TopDisc node's home, in an emulated
// region, on a physical node.
type Vnode struct {
	Name   string
	Region string
	Pnode  string
	IP     string // known after Apply
}

type Plan struct {
	Vnodes  []Vnode
	Matrix  [][]float64 // one-way ms, Vnodes order
	Regions []string
}

// Layout spreads the scenario's fleet (cfg.Fleet(): region -> count, pins
// included) over the physical nodes round robin, home region first, and
// derives the per-pair latency matrix from the RTT table.
func Layout(cfg scenario.Config, pnodes []string, table *wan.RTTTable) (*Plan, error) {
	if len(pnodes) == 0 {
		return nil, fmt.Errorf("no physical nodes")
	}
	fleet := cfg.Fleet()
	regions := make([]string, 0, len(fleet))
	for r := range fleet {
		regions = append(regions, r)
	}
	home := cfg.Testbed.Cloud.HomeRegion
	sort.Slice(regions, func(i, j int) bool {
		if (regions[i] == home) != (regions[j] == home) {
			return regions[i] == home
		}
		return regions[i] < regions[j]
	})
	p := &Plan{Regions: regions}
	i := 0
	for _, r := range regions {
		for k := 0; k < fleet[r]; k++ {
			p.Vnodes = append(p.Vnodes, Vnode{Name: fmt.Sprintf("vn%d", i), Region: r, Pnode: pnodes[i%len(pnodes)]})
			i++
		}
	}
	per := make([]string, len(p.Vnodes))
	for i, v := range p.Vnodes {
		per[i] = v.Region
	}
	m, err := table.LatencyMatrix(per)
	if err != nil {
		return nil, err
	}
	p.Matrix = m
	return p, nil
}

func (p *Plan) Names() []string {
	n := make([]string, len(p.Vnodes))
	for i, v := range p.Vnodes {
		n[i] = v.Name
	}
	return n
}

// Apply creates the vnetwork and the vnodes on the coordinator, starts them,
// waits for the hostagent port, installs the latency matrix and the egress
// cap, and fills in the addresses.
func (p *Plan) Apply(c *Client, subnetCIDR, image string, rateKbps int, log func(string, ...any)) error {
	if err := c.VnetworkCreate(Vnetwork, subnetCIDR); err != nil {
		return fmt.Errorf("vnetwork: %w", err)
	}
	byPnode := map[string][]string{}
	order := []string{}
	for _, v := range p.Vnodes {
		if _, ok := byPnode[v.Pnode]; !ok {
			order = append(order, v.Pnode)
		}
		byPnode[v.Pnode] = append(byPnode[v.Pnode], v.Name)
	}
	for _, pn := range order {
		var d VnodeDesc
		d.Host = pn
		d.VFilesystem.Image = image
		d.VFilesystem.Shared = true
		d.VIfaces = []map[string]string{{"name": Iface, "vnetwork": Vnetwork}}
		log("creating %d vnodes on %s", len(byPnode[pn]), pn)
		if err := c.VnodesCreate(byPnode[pn], d); err != nil {
			return fmt.Errorf("create on %s: %w", pn, err)
		}
	}
	names := p.Names()
	log("starting %d vnodes", len(names))
	if err := c.VnodesStart(names); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	ok, err := c.WaitVnodes(names, AgentPort, 10*time.Minute)
	if err != nil {
		return fmt.Errorf("wait: %w", err)
	}
	if !ok {
		return fmt.Errorf("vnodes did not open port %d in time", AgentPort)
	}
	log("installing %dx%d latency matrix", len(names), len(names))
	if err := c.SetPeersLatencies(names, p.Matrix); err != nil {
		return fmt.Errorf("latencies: %w", err)
	}
	if rateKbps > 0 {
		for _, n := range names {
			if err := c.SetOutputRate(n, Iface, fmt.Sprintf("%dkbps", rateKbps)); err != nil {
				return fmt.Errorf("rate %s: %w", n, err)
			}
		}
	}
	infos, err := c.Vnodes()
	if err != nil {
		return err
	}
	addr := map[string]string{}
	for _, vi := range infos {
		addr[vi.Name] = strings.SplitN(vi.Address, "/", 2)[0]
	}
	for i := range p.Vnodes {
		p.Vnodes[i].IP = addr[p.Vnodes[i].Name]
		if p.Vnodes[i].IP == "" {
			return fmt.Errorf("%s has no address", p.Vnodes[i].Name)
		}
	}
	return nil
}

// Destroy removes every vnode of the plan and the vnetwork.
func (p *Plan) Destroy(c *Client) error {
	if err := c.VnodesRemove(p.Names()); err != nil {
		return err
	}
	return c.VnetworksRemove()
}

// WriteInventory writes the coordinator's inventory: one host per vnode.
func (p *Plan) WriteInventory(path string) error {
	type host struct {
		Index  int    `json:"index"`
		IP     string `json:"ip"`
		Nodes  int    `json:"nodes"`
		Region string `json:"region"`
		Port   int    `json:"port"`
		Pnode  string `json:"pnode"`
	}
	hosts := make([]host, len(p.Vnodes))
	for i, v := range p.Vnodes {
		hosts[i] = host{i, v.IP, 1, v.Region, AgentPort, v.Pnode}
	}
	b, _ := json.MarshalIndent(map[string]any{"coordinator": "", "hosts": hosts}, "", " ")
	return os.WriteFile(path, b, 0o644)
}
