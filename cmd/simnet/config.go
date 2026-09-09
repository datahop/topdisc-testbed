// Generated from the schema in scripts/gen_config.py's source table; a run's
// run.yaml is this struct fully resolved.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Name         string             `yaml:"name"`
	Population   PopulationConfig   `yaml:"population"`
	Network      NetworkConfig      `yaml:"network"`
	Phases       PhasesConfig       `yaml:"phases"`
	Topic        TopicConfig        `yaml:"topic"`
	Search       SearchConfig       `yaml:"search"`
	ConnModel    ConnModelConfig    `yaml:"conn_model"`
	SessionChurn SessionChurnConfig `yaml:"session_churn"`
	Disconnect   DisconnectConfig   `yaml:"disconnect"`
	Churn        ChurnConfig        `yaml:"churn"`
	Traces       TracesConfig       `yaml:"traces"`
	Safety       SafetyConfig       `yaml:"safety"`
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

// NetworkConfig: the simulated links and router.
type NetworkConfig struct {
	LatencyMs      int  `yaml:"latency_ms"`
	BandwidthMibps int  `yaml:"bandwidth_mibps"`
	LinkBuf        int  `yaml:"link_buf"`
	LinkNoAqm      bool `yaml:"link_no_aqm"`
	RouterBuf      int  `yaml:"router_buf"`
	RouterShards   int  `yaml:"router_shards"`
}

// PhasesConfig: how the run is paced.
type PhasesConfig struct {
	SpawnDelay      time.Duration `yaml:"spawn_delay"`
	MaxBootnodes    int           `yaml:"max_bootnodes"`
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
	RegProbePeriod       time.Duration `yaml:"reg_probe_period"`
}

// SearchConfig: how searchers consume results.
type SearchConfig struct {
	Model          string        `yaml:"model"`
	RequestDelay   time.Duration `yaml:"request_delay"`
	RequestTimeout time.Duration `yaml:"request_timeout"`
	PauseMax       time.Duration `yaml:"pause_max"`
	PauseNovelOnly bool          `yaml:"pause_novel_only"`
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
	Enabled bool          `yaml:"enabled"`
	Gap     time.Duration `yaml:"gap"`
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
	{"population.nodes", "discv5 nodes to spawn"},
	{"population.topics", "distinct topics; >1 assigns one per node by Zipf"},
	{"population.all_register", "one shared topic that every node registers and searches"},
	{"population.register_frac", "single-topic mode: fraction that register, the rest search"},
	{"population.zipf_s", "Zipf skew for topic assignment when topics > 1"},
	{"population.common_topic", "topics > 1: everyone also registers and searches topic 0"},
	{"population.seed", "RNG seed for every random draw; 0 = time"},
	{"population.legacy_frac", "fraction of nodes without the topic-discovery ENR flag"},
	{"population.vanilla_frac", "fraction running stock upstream geth (needs -tags vanilla)"},
	{"network.latency_ms", "per-pair one-way latency, ms"},
	{"network.bandwidth_mibps", "per-direction link bandwidth"},
	{"network.link_buf", "link input queue depth; 0 = simnet default 1024"},
	{"network.link_no_aqm", "skip fq_codel and rate limiting on links"},
	{"network.router_buf", "router per-shard queue; 0 = simnet default 8192"},
	{"network.router_shards", "router shards; 0 = simnet default 16"},
	{"phases.spawn_delay", "gap between spawning consecutive nodes"},
	{"phases.max_bootnodes", "bootnodes each new node contacts"},
	{"phases.bootstrap_wait", "after spawning, before registrations start"},
	{"phases.register_stagger", "gap between consecutive nodes starting to register"},
	{"phases.register_wait", "after the last node starts registering, before searches start"},
	{"phases.search_stagger", "gap between consecutive searchers starting"},
	{"phases.search_timeout", "length of the search phase"},
	{"phases.refresh_interval", "discv5 table refresh; 0 = default 30m"},
	{"topic.ad_lifetime", "ad lifetime; 0 = default 15m. Also sets reg_attempt_timeout"},
	{"topic.ad_cache_size", "ads a registrar holds; 0 = default 5000"},
	{"topic.reg_attempt_timeout", "give up on a registrar after this; 0 = 1.5 x ad_lifetime"},
	{"topic.search_bucket_size", "search table entries per distance bucket; 0 = spec default 16"},
	{"topic.topic_nodes_limit", "topic nodes in a TOPICQUERY reply; 0 = default 16"},
	{"topic.aux_nodes_limit", "closest-to-topic nodes attached to TOPICQUERY and REGTOPIC replies; 0 = default 8"},
	{"topic.nodes_per_source_bucket", "cap per source per bucket; 0 = default 1 (inert on topdisc)"},
	{"topic.remove_on_expiry", "drop ads at expiry instead of renewing (inert on topdisc)"},
	{"topic.reg_probe_period", "how often the harness polls for registration admission"},
	{"search.model", "conn: search only while outbound slots are empty; continuous: lookups back to back"},
	{"search.request_delay", "continuous: pause between lookups"},
	{"search.request_timeout", "continuous: give up on a lookup after this; 0 = only target_count ends it"},
	{"search.pause_max", "random sleep up to this between results; 0 with conn_model"},
	{"search.pause_novel_only", "only pause on registrants not seen before"},
	{"search.target_count", "conn: stop a searcher after this many distinct registrants; continuous: end each lookup at this many. 0 = never"},
	{"conn_model.enabled", ""},
	{"conn_model.max_peers", "total slots per node"},
	{"conn_model.dial_ratio", "1/N of slots are outbound"},
	{"conn_model.redial_wait", "cooldown before re-dialing the same node"},
	{"session_churn.enabled", "42.3% stay all run; the rest fall off geometrically from a short mode"},
	{"session_churn.gap", "how long a departed node is unreachable"},
	{"disconnect.interval", "drop a fraction of live connections this often; 0 = off"},
	{"disconnect.frac", "fraction dropped per interval"},
	{"churn.interval", "churn round period; 0 = off"},
	{"churn.frac", "fraction of nodes acted on per round"},
	{"churn.mode", "steadystate (50/50 leave/join) or killonly"},
	{"traces.metrics", "search and registration record (JSON)"},
	{"traces.overhead", "per-node traffic totals by message type (JSON)"},
	{"traces.overhead_series", "traffic and ad-cache samples over time (JSON)"},
	{"traces.overhead_series_period", "sampling period for overhead_series"},
	{"traces.reach", "per-searcher registrar reach sets (JSON)"},
	{"traces.snapshot_dir", "periodic find-count snapshots"},
	{"traces.checkpoint_interval", "print coverage this often during search; 0 = off"},
	{"safety.abort_on_drop", "exit on the first dropped packet: drops bias every timing"},
}

func defaultConfig() Config {
	var c Config
	c.Population.Nodes = 5
	c.Population.Topics = 1
	c.Population.RegisterFrac = 0.5
	c.Population.ZipfS = 1.07
	c.Network.LatencyMs = 30
	c.Network.BandwidthMibps = 100
	c.Phases.MaxBootnodes = 20
	c.Phases.BootstrapWait = mustDur("3s")
	c.Phases.RegisterWait = mustDur("5s")
	c.Phases.SearchTimeout = mustDur("30s")
	c.Topic.RegProbePeriod = mustDur("500ms")
	c.Search.Model = "conn"
	c.ConnModel.MaxPeers = 50
	c.ConnModel.DialRatio = 3
	c.ConnModel.RedialWait = mustDur("35s")
	c.SessionChurn.Gap = mustDur("30s")
	c.Disconnect.Frac = 0.01
	c.Churn.Frac = 0.1
	c.Churn.Mode = "steadystate"
	c.Traces.OverheadSeriesPeriod = mustDur("30s")
	c.Safety.AbortOnDrop = true
	return c
}

func mustDur(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		panic(err)
	}
	return d
}

func loadConfig(path string) (Config, error) {
	c := defaultConfig()
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	if c.Name == "" {
		c.Name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	return c, nil
}

// flatten walks the struct as yaml-path -> value, in declaration order.
func (c Config) flatten() (keys []string, vals map[string]any) {
	vals = map[string]any{}
	rv := reflect.ValueOf(c)
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		sf := rt.Field(i)
		tag := sf.Tag.Get("yaml")
		if sf.Type.Kind() != reflect.Struct {
			keys = append(keys, tag)
			vals[tag] = rv.Field(i).Interface()
			continue
		}
		sv := rv.Field(i)
		for j := 0; j < sv.NumField(); j++ {
			k := tag + "." + sf.Type.Field(j).Tag.Get("yaml")
			keys = append(keys, k)
			vals[k] = sv.Field(j).Interface()
		}
	}
	return
}

func fmtVal(v any) string {
	if d, ok := v.(time.Duration); ok {
		return d.String()
	}
	return fmt.Sprint(v)
}

// paramsLine is the one-liner the figure scripts parse.
func (c Config) paramsLine() string {
	keys, vals := c.flatten()
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+fmtVal(vals[k]))
	}
	sort.Strings(parts)
	return "PARAMS: " + strings.Join(parts, " ")
}

func printReference() {
	_, vals := defaultConfig().flatten()
	fmt.Println("# Every parameter, with its default and meaning. Copy and edit.")
	fmt.Println("name: my-run")
	last := ""
	for _, d := range paramDocs {
		sec, key, _ := strings.Cut(d.path, ".")
		if sec != last {
			fmt.Printf("\n%s:\n", sec)
			last = sec
		}
		v := vals[d.path]
		s := fmtVal(v)
		if str, ok := v.(string); ok && str == "" {
			s = `""`
		}
		kv := fmt.Sprintf("  %s: %s", key, s)
		if d.doc != "" {
			fmt.Printf("%-38s # %s\n", kv, d.doc)
		} else {
			fmt.Println(kv)
		}
	}
}

// prepareRun creates <name>-<timestamp>/, points relative trace paths into
// it, writes the resolved run.yaml, and tees stdout into run.log.
// flushLog drains the stdout tee into run.log; call it before any exit.
var flushLog = func() {}

func prepareRun(c *Config) (string, error) {
	dir := fmt.Sprintf("%s-%s", c.Name, time.Now().Format("20060102-150405"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	for _, p := range []*string{&c.Traces.Metrics, &c.Traces.Overhead, &c.Traces.OverheadSeries, &c.Traces.Reach, &c.Traces.SnapshotDir} {
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
	flushLog = func() {
		w.Close()
		<-done
		logf.Close()
		os.Stdout = real
		flushLog = func() {}
	}
	return dir, nil
}
