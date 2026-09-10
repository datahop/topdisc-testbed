// Config is a run: a testbed-agnostic scenario plus how this testbed executes
// it. A run directory's run.yaml is this struct fully resolved.
package scenario

import (
	"fmt"
	"github.com/datahop/topdisc-testbed/pkg/wan"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	SourcePath string         `yaml:"-"`
	Name       string         `yaml:"name"`
	Scenario   ScenarioConfig `yaml:"scenario"`
	Testbed    TestbedConfig  `yaml:"testbed"`
}

// ScenarioConfig describes the experiment; it means the same on any testbed.
type ScenarioConfig struct {
	Population   PopulationConfig   `yaml:"population"`
	Network      NetworkConfig      `yaml:"network"`
	Phases       PhasesConfig       `yaml:"phases"`
	Topic        TopicConfig        `yaml:"topic"`
	Search       SearchConfig       `yaml:"search"`
	ConnModel    ConnModelConfig    `yaml:"conn_model"`
	SessionChurn SessionChurnConfig `yaml:"session_churn"`
	Disconnect   DisconnectConfig   `yaml:"disconnect"`
	Churn        ChurnConfig        `yaml:"churn"`
}

// TestbedConfig is how this harness executes a scenario.
type TestbedConfig struct {
	Backend   string          `yaml:"backend"`
	Local     LocalConfig     `yaml:"local"`
	Wan       WanConfig       `yaml:"wan"`
	Cloud     CloudConfig     `yaml:"cloud"`
	Simulator SimulatorConfig `yaml:"simulator"`
	Harness   HarnessConfig   `yaml:"harness"`
	Traces    TracesConfig    `yaml:"traces"`
	Safety    SafetyConfig    `yaml:"safety"`
}

// PopulationConfig: who is in the network.
type PopulationConfig struct {
	Nodes        int     `yaml:"nodes"`
	Topics       int     `yaml:"topics"`
	AllRegister  bool    `yaml:"all_register"`
	RegisterFrac float64 `yaml:"register_frac"`
	ZipfS        float64 `yaml:"zipf_s"`
	CommonTopic  bool    `yaml:"common_topic"`
	Seed         int64   `yaml:"seed"`
	LegacyFrac   float64 `yaml:"legacy_frac"`
	VanillaFrac  float64 `yaml:"vanilla_frac"`
}

// NetworkConfig: the emulated WAN conditions.
type NetworkConfig struct {
	LatencyMs      int                `yaml:"latency_ms"`
	BandwidthMibps int                `yaml:"bandwidth_mibps"`
	Regions        map[string]float64 `yaml:"regions"`
	NodeRegions    map[int]string     `yaml:"node_regions"`
}

// PhasesConfig: how the run is paced.
type PhasesConfig struct {
	BootstrapWait   time.Duration `yaml:"bootstrap_wait"`
	RegisterStagger time.Duration `yaml:"register_stagger"`
	RegisterWait    time.Duration `yaml:"register_wait"`
	SearchStagger   time.Duration `yaml:"search_stagger"`
	SearchTimeout   time.Duration `yaml:"search_timeout"`
	RefreshInterval time.Duration `yaml:"refresh_interval"`
}

// TopicConfig: protocol parameters.
type TopicConfig struct {
	AdLifetime           time.Duration `yaml:"ad_lifetime"`
	AdCacheSize          int           `yaml:"ad_cache_size"`
	RegAttemptTimeout    time.Duration `yaml:"reg_attempt_timeout"`
	SearchBucketSize     int           `yaml:"search_bucket_size"`
	TopicNodesLimit      int           `yaml:"topic_nodes_limit"`
	AuxNodesLimit        int           `yaml:"aux_nodes_limit"`
	NodesPerSourceBucket int           `yaml:"nodes_per_source_bucket"`
	RemoveOnExpiry       bool          `yaml:"remove_on_expiry"`
}

// SearchConfig: how nodes search.
type SearchConfig struct {
	Model          string        `yaml:"model"`
	RequestDelay   time.Duration `yaml:"request_delay"`
	RequestTimeout time.Duration `yaml:"request_timeout"`
	TargetCount    int           `yaml:"target_count"`
}

// ConnModelConfig: geth peer slots: a node stops searching once its outbound slots are full.
type ConnModelConfig struct {
	Enabled    bool          `yaml:"enabled"`
	MaxPeers   int           `yaml:"max_peers"`
	DialRatio  int           `yaml:"dial_ratio"`
	RedialWait time.Duration `yaml:"redial_wait"`
}

// SessionChurnConfig: nodes leave and return with session lengths from a measured discv5 crawl.
type SessionChurnConfig struct {
	Enabled      bool          `yaml:"enabled"`
	Gap          time.Duration `yaml:"gap"`
	AlwaysOnFrac float64       `yaml:"always_on_frac"`
	Scale        float64       `yaml:"scale"`
	Model        string        `yaml:"model"`
	WindowHours  float64       `yaml:"window_real_hours"`
}

// DisconnectConfig: link failure without any node leaving.
type DisconnectConfig struct {
	Interval time.Duration `yaml:"interval"`
	Frac     float64       `yaml:"frac"`
}

// ChurnConfig: node kill/join churn (the older model).
type ChurnConfig struct {
	Interval time.Duration `yaml:"interval"`
	Frac     float64       `yaml:"frac"`
	Mode     string        `yaml:"mode"`
}

// WanConfig: per-node WAN emulation (netns + netem); star model, Linux only.
type WanConfig struct {
	Enabled    bool    `yaml:"enabled"`
	DelayMinMs float64 `yaml:"delay_min_ms"`
	DelayMaxMs float64 `yaml:"delay_max_ms"`
	JitterMs   float64 `yaml:"jitter_ms"`
	RateKbps   int     `yaml:"rate_kbps"`
}

// CloudConfig: the cloud backend drives hostagents listed in a Terraform
// inventory (deploy/terraform/*/outputs.tf).
type CloudConfig struct {
	Inventory  string        `yaml:"inventory"`
	HomeRegion string        `yaml:"home_region"`
	AgentPort  int           `yaml:"agent_port"`
	Verbosity  int           `yaml:"verbosity"`
	Grace      time.Duration `yaml:"grace"`
}

// LocalConfig: the local backend — N node processes on this host.
type LocalConfig struct {
	NodeBinary string        `yaml:"node_binary"`
	BasePort   int           `yaml:"base_port"`
	Grace      time.Duration `yaml:"grace"`
	Verbosity  int           `yaml:"verbosity"`
}

// SimulatorConfig: simnet internals; no equivalent on real hosts.
type SimulatorConfig struct {
	LinkBuf      int  `yaml:"link_buf"`
	LinkNoAqm    bool `yaml:"link_no_aqm"`
	RouterBuf    int  `yaml:"router_buf"`
	RouterShards int  `yaml:"router_shards"`
}

// HarnessConfig: how this harness drives the nodes.
type HarnessConfig struct {
	SpawnDelay           time.Duration `yaml:"spawn_delay"`
	MaxBootnodes         int           `yaml:"max_bootnodes"`
	RegProbePeriod       time.Duration `yaml:"reg_probe_period"`
	SearchPauseMax       time.Duration `yaml:"search_pause_max"`
	SearchPauseNovelOnly bool          `yaml:"search_pause_novel_only"`
}

// TracesConfig: what to record; files are written into the run directory.
type TracesConfig struct {
	Metrics              string        `yaml:"metrics"`
	Overhead             string        `yaml:"overhead"`
	OverheadSeries       string        `yaml:"overhead_series"`
	OverheadSeriesPeriod time.Duration `yaml:"overhead_series_period"`
	Reach                string        `yaml:"reach"`
	SnapshotDir          string        `yaml:"snapshot_dir"`
	CheckpointInterval   time.Duration `yaml:"checkpoint_interval"`
}

type SafetyConfig struct {
	AbortOnDrop bool `yaml:"abort_on_drop"`
}

type paramDoc struct{ path, doc string }

var paramDocs = []paramDoc{
	{"testbed.backend", "simnet: in-process on this host; local: one node process per node on this host; cloud: Terraform fleet"},
	{"testbed.local.node_binary", "local backend: path to the node binary"},
	{"testbed.local.base_port", "local backend: node i listens on base_port+i, status on +10000"},
	{"testbed.local.grace", "local backend: wait after the last StopAt before collecting"},
	{"testbed.local.verbosity", "node log level: 2 warn, 3 info, 4 debug (disconnect reasons)"},
	{"testbed.cloud.inventory", "cloud backend: inventory.json written on the coordinator by deploy/inventory-*.sh"},
	{"testbed.cloud.home_region", "cloud backend: coordinator, bootnode and binaries bucket live here"},
	{"testbed.cloud.agent_port", "cloud backend: hostagent port on every host"},
	{"testbed.cloud.verbosity", "cloud backend: node log level"},
	{"testbed.cloud.grace", "cloud backend: wait after the last StopAt before fetching traces"},
	{"testbed.wan.enabled", "give each node its own netns shaped by netem (Linux, root)"},
	{"testbed.wan.delay_min_ms", "star model: min one-way delay per node; plan RTT 8ms -> 4"},
	{"testbed.wan.delay_max_ms", "star model: max one-way delay per node; plan RTT 91ms -> 45"},
	{"testbed.wan.jitter_ms", "netem jitter"},
	{"testbed.wan.rate_kbps", "per-node rate cap; plan 20 KB/s -> 160; 0 = unshaped"},
	{"scenario.population.nodes", "discv5 nodes to spawn"},
	{"scenario.population.topics", "distinct topics; >1 assigns one per node by Zipf"},
	{"scenario.population.all_register", "one shared topic that every node registers and searches"},
	{"scenario.population.register_frac", "single-topic mode: fraction that register, the rest search"},
	{"scenario.population.zipf_s", "Zipf skew for topic assignment when topics > 1"},
	{"scenario.population.common_topic", "topics > 1: everyone also registers and searches topic 0"},
	{"scenario.population.seed", "RNG seed for every random draw; 0 = time"},
	{"scenario.population.legacy_frac", "fraction of nodes without the topic-discovery ENR flag"},
	{"scenario.population.vanilla_frac", "fraction running stock upstream geth (needs -tags vanilla)"},
	{"scenario.network.latency_ms", "simnet: per-pair one-way latency, ms (cloud: given by regions)"},
	{"scenario.network.bandwidth_mibps", "simnet: per-direction link bandwidth (cloud: given by the instance type)"},
	{"scenario.network.regions", "cloud: node placement, region -> weight, e.g. {us-east-1: 0.4, eu-central-1: 0.3, ap-southeast-1: 0.3}; empty = all in the home region"},
	{"scenario.network.node_regions", "cloud: pin nodes, index -> region, e.g. {0: us-east-1, 7: sa-east-1}; the rest follow regions"},
	{"scenario.phases.bootstrap_wait", "after spawning, before registrations start"},
	{"scenario.phases.register_stagger", "gap between consecutive nodes starting to register"},
	{"scenario.phases.register_wait", "after the last node starts registering, before searches start"},
	{"scenario.phases.search_stagger", "gap between consecutive searchers starting"},
	{"scenario.phases.search_timeout", "length of the search phase"},
	{"scenario.phases.refresh_interval", "discv5 table refresh; 0 = default 30m"},
	{"scenario.topic.ad_lifetime", "ad lifetime; 0 = default 15m. Also sets reg_attempt_timeout"},
	{"scenario.topic.ad_cache_size", "ads a registrar holds; 0 = default 5000"},
	{"scenario.topic.reg_attempt_timeout", "give up on a registrar after this; 0 = 1.5 x ad_lifetime"},
	{"scenario.topic.search_bucket_size", "search table entries per distance bucket; 0 = spec default 16"},
	{"scenario.topic.topic_nodes_limit", "topic nodes in a TOPICQUERY reply; 0 = default 16"},
	{"scenario.topic.aux_nodes_limit", "closest-to-topic nodes attached to TOPICQUERY and REGTOPIC replies; 0 = default 8"},
	{"scenario.topic.nodes_per_source_bucket", "cap per source per bucket; 0 = default 1 (inert on topdisc)"},
	{"scenario.topic.remove_on_expiry", "drop ads at expiry instead of renewing (inert on topdisc)"},
	{"scenario.search.model", "conn: search only while outbound slots are empty; continuous: lookups back to back"},
	{"scenario.search.request_delay", "continuous: pause between lookups"},
	{"scenario.search.request_timeout", "continuous: give up on a lookup after this; 0 = only target_count ends it"},
	{"scenario.search.target_count", "conn: stop a searcher after this many distinct registrants; continuous: end each lookup at this many. 0 = never"},
	{"scenario.conn_model.enabled", ""},
	{"scenario.conn_model.max_peers", "total slots per node"},
	{"scenario.conn_model.dial_ratio", "1/N of slots are outbound"},
	{"scenario.conn_model.redial_wait", "cooldown before re-dialing the same node"},
	{"scenario.session_churn.enabled", "42.3% stay all run; the rest fall off geometrically from a short mode"},
	{"scenario.session_churn.gap", "how long a departed node is unreachable"},
	{"scenario.session_churn.always_on_frac", "share of nodes that never leave (crawl: 0.423)"},
	{"scenario.session_churn.scale", "multiplier on every session length; 0.5 doubles the churn rate, 2 halves it"},
	{"scenario.session_churn.model", "fitted hazard model JSON (see scenarios/models); when set, replaces the builtin distribution and always_on_frac/scale"},
	{"scenario.session_churn.window_real_hours", "with a model: the search window stands for this many real hours"},
	{"scenario.disconnect.interval", "drop a fraction of live connections this often; 0 = off"},
	{"scenario.disconnect.frac", "fraction dropped per interval"},
	{"scenario.churn.interval", "churn round period; 0 = off"},
	{"scenario.churn.frac", "fraction of nodes acted on per round"},
	{"scenario.churn.mode", "steadystate (50/50 leave/join) or killonly"},
	{"testbed.simulator.link_buf", "link input queue depth; 0 = simnet default 1024"},
	{"testbed.simulator.link_no_aqm", "skip fq_codel and rate limiting on links"},
	{"testbed.simulator.router_buf", "router per-shard queue; 0 = simnet default 8192"},
	{"testbed.simulator.router_shards", "router shards; 0 = simnet default 16"},
	{"testbed.harness.spawn_delay", "gap between spawning consecutive nodes"},
	{"testbed.harness.max_bootnodes", "bootnodes each new node contacts"},
	{"testbed.harness.reg_probe_period", "how often the harness polls for registration admission"},
	{"testbed.harness.search_pause_max", "random sleep up to this between results; 0 with conn_model"},
	{"testbed.harness.search_pause_novel_only", "only pause on registrants not seen before"},
	{"testbed.traces.metrics", "search and registration record (JSON)"},
	{"testbed.traces.overhead", "per-node traffic totals by message type (JSON)"},
	{"testbed.traces.overhead_series", "traffic and ad-cache samples over time (JSON)"},
	{"testbed.traces.overhead_series_period", "sampling period for overhead_series"},
	{"testbed.traces.reach", "per-searcher registrar reach sets (JSON)"},
	{"testbed.traces.snapshot_dir", "periodic find-count snapshots"},
	{"testbed.traces.checkpoint_interval", "print coverage this often during search; 0 = off"},
	{"testbed.safety.abort_on_drop", "exit on the first dropped packet: drops bias every timing"},
}

func Default() Config {
	var c Config
	c.Testbed.Backend = "simnet"
	c.Testbed.Local.NodeBinary = "./topdisc-node"
	c.Testbed.Local.BasePort = 30300
	c.Testbed.Local.Grace = mustDur("10s")
	c.Testbed.Local.Verbosity = 2
	c.Testbed.Cloud.Inventory, c.Testbed.Cloud.HomeRegion, c.Testbed.Cloud.AgentPort, c.Testbed.Cloud.Verbosity, c.Testbed.Cloud.Grace = "inventory.json", "us-east-1", 9000, 2, mustDur("30s")
	c.Testbed.Wan.DelayMinMs, c.Testbed.Wan.DelayMaxMs, c.Testbed.Wan.JitterMs, c.Testbed.Wan.RateKbps = 4, 45, 3, 160
	c.Scenario.Population.Nodes = 5
	c.Scenario.Population.Topics = 1
	c.Scenario.Population.RegisterFrac = 0.5
	c.Scenario.Population.ZipfS = 1.07
	c.Scenario.Network.LatencyMs = 30
	c.Scenario.Network.BandwidthMibps = 100
	c.Testbed.Harness.MaxBootnodes = 20
	c.Scenario.Phases.BootstrapWait = mustDur("3s")
	c.Scenario.Phases.RegisterWait = mustDur("5s")
	c.Scenario.Phases.SearchTimeout = mustDur("30s")
	c.Testbed.Harness.RegProbePeriod = mustDur("500ms")
	c.Scenario.Search.Model = "conn"
	c.Scenario.ConnModel.MaxPeers = 50
	c.Scenario.ConnModel.DialRatio = 3
	c.Scenario.ConnModel.RedialWait = mustDur("35s")
	c.Scenario.SessionChurn.Gap = mustDur("30s")
	c.Scenario.SessionChurn.AlwaysOnFrac = 0.423
	c.Scenario.SessionChurn.Scale = 1.0
	c.Scenario.SessionChurn.WindowHours = 24
	c.Scenario.Disconnect.Frac = 0.01
	c.Scenario.Churn.Frac = 0.1
	c.Scenario.Churn.Mode = "steadystate"
	c.Testbed.Traces.OverheadSeriesPeriod = mustDur("30s")
	c.Testbed.Safety.AbortOnDrop = true
	return c
}

func mustDur(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		panic(err)
	}
	return d
}

func Load(path string) (Config, error) {
	c := Default()
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	c.SourcePath = path
	if c.Name == "" {
		c.Name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	return c, nil
}

// flatten walks the struct as yaml-path -> value, in declaration order.
func (c Config) flatten() (keys []string, vals map[string]any) {
	vals = map[string]any{}
	var walk func(prefix string, rv reflect.Value)
	walk = func(prefix string, rv reflect.Value) {
		rt := rv.Type()
		for i := 0; i < rt.NumField(); i++ {
			sf := rt.Field(i)
			k := sf.Tag.Get("yaml")
			if prefix != "" {
				k = prefix + "." + k
			}
			if sf.Type.Kind() == reflect.Struct {
				walk(k, rv.Field(i))
				continue
			}
			keys = append(keys, k)
			vals[k] = rv.Field(i).Interface()
		}
	}
	walk("", reflect.ValueOf(c))
	return
}

func fmtVal(v any) string {
	switch x := v.(type) {
	case time.Duration:
		return x.String()
	case map[string]float64:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = fmt.Sprintf("%s:%g", k, x[k])
		}
		return "{" + strings.Join(parts, ",") + "}"
	case map[int]string:
		keys := make([]int, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Ints(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = fmt.Sprintf("%d:%s", k, x[k])
		}
		return "{" + strings.Join(parts, ",") + "}"
	}
	return fmt.Sprint(v)
}

// ParamsLine is the one-liner the figure scripts parse.
func (c Config) ParamsLine() string {
	keys, vals := c.flatten()
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+fmtVal(vals[k]))
	}
	sort.Strings(parts)
	return "PARAMS: " + strings.Join(parts, " ")
}

func PrintReference() {
	_, vals := Default().flatten()
	fmt.Println("# Every parameter, with its default and meaning. Copy and edit.")
	fmt.Println("name: my-run")
	lastTop, lastSec := "", ""
	for _, d := range paramDocs {
		top, rest, _ := strings.Cut(d.path, ".")
		sec, key, _ := strings.Cut(rest, ".")
		if top != lastTop {
			fmt.Printf("\n%s:\n", top)
			lastTop, lastSec = top, ""
		}
		if sec != lastSec {
			fmt.Printf("  %s:\n", sec)
			lastSec = sec
		}
		v := vals[d.path]
		s := fmtVal(v)
		if str, ok := v.(string); ok && str == "" {
			s = `""`
		}
		kv := fmt.Sprintf("    %s: %s", key, s)
		if d.doc != "" {
			fmt.Printf("%-38s # %s\n", kv, d.doc)
		} else {
			fmt.Println(kv)
		}
	}
}

// PrepareRun creates <name>-<timestamp>/, points relative trace paths into
// it, writes the resolved run.yaml, and tees stdout into run.log.
// FlushLog drains the stdout tee into run.log; call it before any exit.
var FlushLog = func() {}

func PrepareRun(c *Config) (string, error) {
	dir := fmt.Sprintf("%s-%s", c.Name, time.Now().Format("20060102-150405"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	for _, p := range []*string{&c.Testbed.Traces.Metrics, &c.Testbed.Traces.Overhead, &c.Testbed.Traces.OverheadSeries, &c.Testbed.Traces.Reach, &c.Testbed.Traces.SnapshotDir} {
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(dir, *p)
		}
	}
	b, err := yaml.Marshal(c)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "run.yaml"), b, 0o644); err != nil {
		return "", err
	}
	logf, err := os.Create(filepath.Join(dir, "run.log"))
	if err != nil {
		return "", err
	}
	r, w, err := os.Pipe()
	if err != nil {
		return "", err
	}
	real := os.Stdout
	os.Stdout = w
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 64<<10)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				real.Write(buf[:n])
				logf.Write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	FlushLog = func() {
		w.Close()
		<-done
		logf.Close()
		os.Stdout = real
		FlushLog = func() {}
	}
	return dir, nil
}

// Star returns the WAN model, or nil when emulation is off.
func (w WanConfig) Star() *wan.Star {
	if !w.Enabled {
		return nil
	}
	return &wan.Star{MinMs: w.DelayMinMs, MaxMs: w.DelayMaxMs, JitterMs: w.JitterMs, RateKbps: w.RateKbps}
}

// Fleet turns the placement into instance counts per region for the cloud
// backend: pinned nodes first, the rest by weight (largest remainder), or
// all in the home region when no weights are given.
func (c Config) Fleet() map[string]int {
	n := c.Scenario.Population.Nodes
	w := c.Scenario.Network.Regions
	out := map[string]int{}
	for idx, r := range c.Scenario.Network.NodeRegions {
		if idx >= 0 && idx < n {
			out[r]++
			n--
		}
	}
	if len(w) == 0 {
		out[c.Testbed.Cloud.HomeRegion] += n
		return out
	}
	keys := make([]string, 0, len(w))
	total := 0.0
	for k, v := range w {
		keys = append(keys, k)
		total += v
	}
	sort.Strings(keys)
	given := 0
	rem := make([]float64, len(keys))
	for i, k := range keys {
		exact := float64(n) * w[k] / total
		out[k] += int(exact)
		rem[i] = exact - float64(int(exact))
		given += int(exact)
	}
	for given < n {
		best := 0
		for i := range rem {
			if rem[i] > rem[best] {
				best = i
			}
		}
		out[keys[best]]++
		rem[best] = -1
		given++
	}
	return out
}
