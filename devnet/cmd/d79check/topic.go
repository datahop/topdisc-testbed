package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/p2p/discover"
	"github.com/ethereum/go-ethereum/p2p/discover/topicindex"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/rpc"
)

func init() {
	register("topic", "E4(a,b): search the eth topic from a host probe seeded with a NEW or OLD node", runTopic)
}

type topicHit struct {
	ID   string  `json:"id"`
	Name string  `json:"name,omitempty"`
	Kind string  `json:"kind"`
	TS   float64 `json:"t_s"` // since the probe started
}

type topicResult struct {
	Seed      string     `json:"seed"`
	SeedNode  node       `json:"seed_node"`
	Topic     string     `json:"topic"`
	ChainID   string     `json:"chain_id"`
	Genesis   string     `json:"genesis"`
	GenesisAt time.Time  `json:"genesis_time"`
	Start     time.Time  `json:"start"`
	Early     string     `json:"early"`
	Timeout   string     `json:"timeout"`
	Found     []topicHit `json:"found"`
	Expected  int        `json:"expected"`
	FoundNew  int        `json:"found_new"`
	EarlyNew  int        `json:"found_new_early"`
	Missing   []string   `json:"missing"`
	Old       []string   `json:"old"`
	Others    int        `json:"others"`
	PassEarly bool       `json:"pass_early"`
	PassAll   bool       `json:"pass_all"`
	ZeroOld   bool       `json:"zero_old"`
	Pass      bool       `json:"pass"`
	ProbeLog  string     `json:"probe_log"`
	ProbeID   string     `json:"probe_id"`
	ProbeENR  string     `json:"probe_enr"`
	ProbeIP   string     `json:"probe_ip"`
	ProbePort int        `json:"probe_port"`
}

func runTopic(args []string) error {
	var rf probeFlags
	fs := flag.NewFlagSet("topic", flag.ExitOnError)
	rf.register(fs)
	port := fs.Int("port", 31420, "UDP port of the probe on the bridge IP")
	seedFlag := fs.String("seed", "both", "seed: new, old, both (new then old) or an enr: record")
	early := fs.Duration("early", 3*time.Minute, "deadline for all NEW nodes but one")
	timeout := fs.Duration("timeout", 10*time.Minute, "search length, the deadline for all NEW nodes; results are checked for OLD nodes until then")
	minAge := fs.Duration("min-age", 20*time.Minute, "refuse to run earlier than this after genesis")
	rpcURL := fs.String("rpc", "", "EL JSON-RPC for chain id and genesis hash (default: the first node, port 8545)")
	fs.Parse(args)

	inv, ip, err := rf.setup()
	if err != nil {
		return err
	}
	if err := checkPort(*port); err != nil {
		return err
	}
	if len(inv.Nodes) == 0 {
		return fmt.Errorf("empty inventory")
	}
	if *rpcURL == "" {
		*rpcURL = "http://" + net.JoinHostPort(inv.Nodes[0].IP, "8545")
	}
	chainID, genesis, genesisTime, err := chainInfo(*rpcURL)
	if err != nil {
		return err
	}
	if age := time.Since(genesisTime); age < *minAge {
		return fmt.Errorf("genesis was %v ago, E4 is judged from %v after genesis", age.Round(time.Second), *minAge)
	}
	topic := eth.DiscoveryTopic(chainID, genesis)

	type seed struct {
		label string
		seed  node
	}
	var seeds []seed
	switch s := *seedFlag; s {
	case "new", "old", "both":
		kinds := []string{s}
		if s == "both" {
			kinds = []string{"new", "old"}
		}
		for _, k := range kinds {
			n, ok := inv.first(k)
			if !ok {
				fmt.Printf("no %s node, skipping that seed\n", k)
				continue
			}
			seeds = append(seeds, seed{k, n})
		}
	default:
		if !strings.HasPrefix(s, "enr:") {
			return fmt.Errorf("bad -seed %q", s)
		}
		en, err := enode.Parse(enode.ValidSchemes, s)
		if err != nil {
			return fmt.Errorf("-seed: %v", err)
		}
		n, ok := inv.nodesByID()[en.ID().String()]
		if !ok {
			n = node{Name: "enr", Kind: "other", ID: en.ID().String(), ENR: s}
		}
		seeds = append(seeds, seed{"enr", n})
	}

	outPath := filepath.Join(rf.out, "topic.json")
	var all topicFile
	if data, err := os.ReadFile(outPath); err == nil {
		if err := json.Unmarshal(data, &all); err != nil {
			return fmt.Errorf("%s: %v", outPath, err)
		}
	}
	if all.Seeds == nil {
		all.Seeds = map[string]*topicResult{}
	}
	for _, s := range seeds {
		sn, err := enrNode(s.seed)
		if err != nil {
			return fmt.Errorf("seed %s: %v", s.seed.Name, err)
		}
		logPath := filepath.Join(rf.out, "topic-"+s.label+".log")
		logger, lf, err := fileLogger(logPath)
		if err != nil {
			return err
		}
		// One key per seed: a later search must not start from what an earlier one learned.
		key := fixedKey("topic-" + s.label)
		stopped, err := recordProbe(rf.run, "topic-"+s.label, key)
		if err != nil {
			lf.Close()
			return err
		}
		disc, err := startProbe(ip, *port, key, []*enode.Node{sn}, logger)
		if err != nil {
			stopped()
			lf.Close()
			return err
		}
		fmt.Printf("searching topic %x from %s seeded with %s (%s)\n", topic[:4], disc.Self().IP(), s.seed.Name, s.seed.Kind)
		res := &topicResult{
			Seed: s.label, SeedNode: s.seed, Topic: hex.EncodeToString(topic[:]),
			ChainID: chainID.String(), Genesis: genesis.Hex(), GenesisAt: genesisTime,
			Early: early.String(), Timeout: timeout.String(), ProbeLog: logPath,
			ProbeID: disc.Self().ID().String(), ProbeENR: disc.Self().String(), ProbeIP: ip.String(), ProbePort: *port,
		}
		res.Start = time.Now().UTC()
		hits := searchTopic(disc, topic, inv, *timeout)
		disc.Close()
		stopped()
		lf.Close()
		scoreTopic(res, inv, hits, *early, *timeout)
		all.Seeds[s.label] = res
		printTopic(res)
		all.Check = topicCheck(inv, all.Seeds)
		if err := writeJSON(outPath, all); err != nil {
			return err
		}
	}
	fmt.Print(renderTable([]check{all.Check}))
	return nil
}

type topicFile struct {
	Check check                   `json:"check"`
	Seeds map[string]*topicResult `json:"seeds"`
}

// topicCheck judges E4(a) from the NEW seed and E4(b) from the OLD seed; a seed
// given as an ENR is reported next to them.
func topicCheck(inv *inventory, seeds map[string]*topicResult) check {
	one := func(id, label, desc string) check {
		c := check{ID: id, Desc: desc}
		r := seeds[label]
		switch {
		case inv.count("new") == 0:
			c.Status, c.Detail = statusNA, "no NEW node"
		case label != "enr" && inv.count(label) == 0:
			c.Status, c.Detail = statusNA, "no "+strings.ToUpper(label)+" node"
		case r == nil:
			c.Status, c.Detail = statusMissing, "seed "+label+" not run"
		default:
			c.Status = passIf(r.Pass)
			c.Detail = fmt.Sprintf("seed %s: %d/%d NEW (%d by %s), %d OLD", r.SeedNode.Name, r.FoundNew, r.Expected, r.EarlyNew, r.Early, len(r.Old))
			if len(r.Missing) > 0 {
				c.Detail += ", missing " + strings.Join(r.Missing, ", ")
			}
			if len(r.Old) > 0 {
				c.Detail += ", OLD found " + strings.Join(r.Old, ", ")
			}
		}
		return c
	}
	sub := []check{
		one("E4a", "new", "search seeded with a NEW node: ≥ N_new−1 NEW in 3 min, all in 10 min, no OLD"),
		one("E4b", "old", "search seeded with an OLD node: same criteria"),
	}
	if seeds["enr"] != nil {
		sub = append(sub, one("E4 enr", "enr", "search seeded with a given ENR"))
	}
	return check{ID: "E4ab", Desc: "topic search from the host probe", Status: overall(sub), Sub: sub}
}

// chainInfo returns the chain id and the genesis block's hash and time.
func chainInfo(url string) (*big.Int, common.Hash, time.Time, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := rpc.DialContext(ctx, url)
	if err != nil {
		return nil, common.Hash{}, time.Time{}, err
	}
	defer c.Close()
	var id hexutil.Big
	if err := c.CallContext(ctx, &id, "eth_chainId"); err != nil {
		return nil, common.Hash{}, time.Time{}, fmt.Errorf("eth_chainId: %v", err)
	}
	var block struct {
		Hash common.Hash    `json:"hash"`
		Time hexutil.Uint64 `json:"timestamp"`
	}
	if err := c.CallContext(ctx, &block, "eth_getBlockByNumber", "0x0", false); err != nil {
		return nil, common.Hash{}, time.Time{}, fmt.Errorf("eth_getBlockByNumber: %v", err)
	}
	if block.Hash == (common.Hash{}) || block.Time == 0 {
		return nil, common.Hash{}, time.Time{}, fmt.Errorf("no genesis block from %s", url)
	}
	return id.ToInt(), block.Hash, time.Unix(int64(block.Time), 0).UTC(), nil
}

// searchTopic runs a topic search for timeout and returns the distinct hits in order.
// It runs to the end even when every NEW node is in, so that an OLD node returned
// late still counts.
func searchTopic(disc *discover.UDPv5, topic topicindex.TopicID, inv *inventory, timeout time.Duration) []topicHit {
	start := time.Now()
	known := inv.nodesByID()
	it := disc.TopicSearch(topic, 0)
	stop := time.AfterFunc(timeout, it.Close)
	defer stop.Stop()
	defer it.Close()

	var hits []topicHit
	seen := map[string]bool{}
	for it.Next() {
		id := it.Node().ID().String()
		if seen[id] {
			continue
		}
		seen[id] = true
		h := topicHit{ID: id, Kind: "other", TS: time.Since(start).Seconds()}
		if n, ok := known[id]; ok {
			h.Name, h.Kind = n.Name, n.Kind
		}
		hits = append(hits, h)
	}
	return hits
}

// scoreTopic evaluates E4: all NEW nodes but one by early, all by timeout, no OLD node.
func scoreTopic(r *topicResult, inv *inventory, hits []topicHit, early, timeout time.Duration) {
	r.Found = append([]topicHit{}, hits...)
	r.Expected = inv.count("new")
	r.Missing, r.Old = []string{}, []string{}
	found := map[string]bool{}
	for _, h := range hits {
		switch h.Kind {
		case "new":
			if h.TS <= timeout.Seconds() {
				r.FoundNew++
				found[h.ID] = true
			}
			if h.TS <= early.Seconds() {
				r.EarlyNew++
			}
		case "old":
			r.Old = append(r.Old, h.Name)
		default:
			r.Others++
		}
	}
	for _, n := range inv.Nodes {
		if n.Kind == "new" && !found[n.ID] {
			r.Missing = append(r.Missing, n.Name)
		}
	}
	r.PassEarly = r.EarlyNew >= r.Expected-1
	r.PassAll = r.FoundNew == r.Expected
	r.ZeroOld = len(r.Old) == 0
	r.Pass = r.Expected > 0 && r.PassEarly && r.PassAll && r.ZeroOld
}

func printTopic(r *topicResult) {
	fmt.Printf("seed %s (%s %s): found %d/%d NEW (%d by %s), old %d, others %d, pass %v\n",
		r.Seed, r.SeedNode.Kind, r.SeedNode.Name, r.FoundNew, r.Expected, r.EarlyNew, r.Early, len(r.Old), r.Others, r.Pass)
	for _, h := range r.Found {
		fmt.Printf("  %6.1f s  %-5s %s %s\n", h.TS, h.Kind, h.ID[:16], h.Name)
	}
	if len(r.Missing) > 0 {
		fmt.Printf("  missing: %s\n", strings.Join(r.Missing, ", "))
	}
}
