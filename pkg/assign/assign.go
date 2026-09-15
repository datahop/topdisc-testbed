// Package assign turns a scenario into per-node assignments: everything one
// node process needs, derived deterministically from the scenario and its seed.
package assign

import (
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/datahop/topdisc-testbed/pkg/churn"
	"github.com/datahop/topdisc-testbed/pkg/scenario"
	"github.com/ethereum/go-ethereum/crypto"
)

type Phases struct {
	StartAt      int64 `json:"start_at_ms"`      // absolute unix ms; the node waits until then before starting
	RegisterBase int64 `json:"register_base_ms"` // registration phase start, the common clock of the traces
	RegisterAt   int64 `json:"register_at_ms"`   // absolute unix ms
	SearchAt     int64 `json:"search_at_ms"`
	StopAt       int64 `json:"stop_at_ms"`
}

type Search struct {
	Model            string  `json:"model"`
	TargetCount      int     `json:"target_count"`
	RequestTimeout   int64   `json:"request_timeout_ms"`
	LookupAtMs       []int64 `json:"lookup_at_ms,omitempty"` // scheduled: absolute lookup times, from the seed
	InitialResults   int     `json:"initial_results,omitempty"`
	ResultIntervalMs int64   `json:"result_interval_ms,omitempty"`
}

// Topic carries the scenario's topic-discovery parameters to the node; zero
// values mean the fork's defaults.
type Topic struct {
	AdLifetimeMs        int64 `json:"ad_lifetime_ms"`
	AdCacheSize         int   `json:"ad_cache_size"`
	RegAttemptTimeoutMs int64 `json:"reg_attempt_timeout_ms"`
	SearchBucketSize    int   `json:"search_bucket_size"`
	TopicNodesLimit     int   `json:"topic_nodes_limit"`
	AuxNodesLimit       int   `json:"aux_nodes_limit"`
	SearchTableDepth    int   `json:"search_table_depth"`
	RegTableDepth       int   `json:"reg_table_depth"`
	RegBucketSize       int   `json:"reg_bucket_size"`
	RegBucketStandby    int   `json:"reg_bucket_standby"`
}

type Assignment struct {
	Idx        int           `json:"idx"`
	Key        string        `json:"key"` // hex secp256k1 private key
	IP         string        `json:"ip"`
	Port       int           `json:"port"`
	StatusPort int           `json:"status_port"`
	Bootnodes  []string      `json:"bootnodes"`
	Topics     []string      `json:"topics"`
	MaxPeers   int           `json:"max_peers"`
	DialRatio  int           `json:"dial_ratio"`
	Phases     Phases        `json:"phases"`
	Search     Search        `json:"search"`
	Topic      Topic         `json:"topic"`
	Legacy     bool          `json:"legacy"`     // stock upstream geth, no topic discovery
	SampleMs   int64         `json:"sample_ms"`  // period of the node's counter/cache samples (series.json); 0 = off
	Churn      []churn.Event `json:"churn"`      // seconds from search start
	TraceFile  string        `json:"trace_file"` // where the node writes its trace at StopAt
}

// Host describes where a node runs; the backend supplies one per node.
type Host struct {
	IP        string
	BasePort  int
	StatusOff int    // status port = port + StatusOff
	TraceFile string // per-node trace path
}

// Generate derives every node's assignment. Keys come from the seed, topics
// from the scenario's Zipf draw, churn from the fitted model when configured.
// t0 is the run's start (unix ms); phases are absolute so hosts need no clock
// agreement beyond NTP.
func Generate(cfg scenario.Config, hosts func(idx int) Host, t0 int64, modelDir string) ([]Assignment, error) {
	sc := cfg.Scenario
	n := sc.Population.Nodes
	rng := rand.New(rand.NewSource(sc.Population.Seed))
	keys := make([]*ecdsa.PrivateKey, n)
	for i := range keys {
		// ecdsa.GenerateKey deliberately defeats seeded readers, so derive the
		// scalar directly from seed and index.
		for ctr := 0; ; ctr++ {
			d := crypto.Keccak256([]byte(fmt.Sprintf("topdisc-testbed/%d/%d/%d", sc.Population.Seed, i, ctr)))
			if k, err := crypto.ToECDSA(d); err == nil {
				keys[i] = k
				break
			}
		}
	}
	topics := make([][]string, n)
	if sc.Population.Topics <= 1 || sc.Population.AllRegister {
		for i := range topics {
			topics[i] = []string{"topic-0"}
		}
	} else {
		z := NewZipf(sc.Population.ZipfS, sc.Population.Topics)
		for i := range topics {
			topics[i] = []string{fmt.Sprintf("topic-%d", z.Draw(rng))}
			if sc.Population.CommonTopic {
				topics[i] = append(topics[i], "topic-0")
			}
		}
	}
	var model *churn.Model
	if sc.SessionChurn.Enabled && sc.SessionChurn.Model != "" {
		var err error
		model, err = churn.Load(filepath.Join(modelDir, sc.SessionChurn.Model))
		if err != nil {
			return nil, err
		}
	}
	ph := sc.Phases
	// Registration phase begins bootstrap_wait after the run starts. With a
	// start window each node registers bootstrap_wait after its own start;
	// without one, node i registers i x register_stagger into the phase.
	registerAt := t0 + ph.BootstrapWait.Milliseconds()
	startOff := StartOffsets(sc.Population.Seed, n, ph.StartWindow)
	regOff := make([]int64, n)
	regSpan := int64(n) * ph.RegisterStagger.Milliseconds()
	for i := range regOff {
		regOff[i] = int64(i) * ph.RegisterStagger.Milliseconds()
	}
	if ph.StartWindow > 0 {
		copy(regOff, startOff)
		regSpan = ph.StartWindow.Milliseconds()
	}
	searchAt := registerAt + regSpan + ph.RegisterWait.Milliseconds()
	stopAt := searchAt + ph.SearchTimeout.Milliseconds()
	// Node 0 is the bootnode for everyone.
	h0 := hosts(0)
	boot := fmt.Sprintf("enode://%s@%s:%d", hex.EncodeToString(crypto.FromECDSAPub(&keys[0].PublicKey)[1:]), h0.IP, h0.BasePort)
	legacy := legacySet(rng, topics, sc.Population.LegacyFrac)
	if !sc.Population.LegacyBootnode {
		legacy[0] = false
	}
	out := make([]Assignment, n)
	for i := range out {
		h := hosts(i)
		a := Assignment{
			Idx: i, Key: hex.EncodeToString(crypto.FromECDSA(keys[i])), IP: h.IP, Port: h.BasePort, StatusPort: h.BasePort + h.StatusOff, TraceFile: h.TraceFile,
			Topics: topics[i], MaxPeers: sc.ConnModel.MaxPeers, DialRatio: sc.ConnModel.DialRatio,
			Phases: Phases{StartAt: t0 + startOff[i], RegisterBase: registerAt, RegisterAt: registerAt + regOff[i], SearchAt: searchAt + int64(i)*ph.SearchStagger.Milliseconds(), StopAt: stopAt},
			Search: Search{Model: sc.Search.Model, TargetCount: sc.Search.TargetCount, RequestTimeout: sc.Search.RequestTimeout.Milliseconds(),
				InitialResults: sc.Search.InitialResults, ResultIntervalMs: sc.Search.ResultInterval.Milliseconds()},
		}
		if i != 0 {
			a.Bootnodes = []string{boot}
		}
		if legacy[i] {
			a.Legacy = true
		}
		a.SampleMs = cfg.Testbed.Traces.OverheadSeriesPeriod.Milliseconds()
		a.Topic = Topic{AdLifetimeMs: sc.Topic.AdLifetime.Milliseconds(), AdCacheSize: sc.Topic.AdCacheSize,
			RegAttemptTimeoutMs: sc.Topic.RegAttemptTimeout.Milliseconds(), SearchBucketSize: sc.Topic.SearchBucketSize,
			TopicNodesLimit: sc.Topic.TopicNodesLimit, AuxNodesLimit: sc.Topic.AuxNodesLimit,
			SearchTableDepth: sc.Topic.SearchTableDepth, RegTableDepth: sc.Topic.RegTableDepth, RegBucketSize: sc.Topic.RegBucketSize, RegBucketStandby: sc.Topic.RegBucketStandby}
		if sc.Search.Model == "scheduled" && sc.Search.Intervals > 0 {
			a.Search.LookupAtMs = LookupTimes(rand.New(rand.NewSource(sc.Population.Seed+int64(i)*40503)), a.Phases.SearchAt, stopAt, sc.Search.Intervals)
		}
		if model != nil {
			a.Churn = model.Schedule(rand.New(rand.NewSource(sc.Population.Seed+int64(i)*2654435761)), ph.SearchTimeout.Seconds(), sc.SessionChurn.WindowHours)
		}
		out[i] = a
	}
	return out, nil
}

// Write stores one JSON file per node under dir.
func Write(dir string, as []Assignment) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, a := range as {
		b, _ := json.MarshalIndent(a, "", " ")
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("node%d.json", a.Idx)), b, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func Read(path string) (Assignment, error) {
	var a Assignment
	b, err := os.ReadFile(path)
	if err != nil {
		return a, err
	}
	return a, json.Unmarshal(b, &a)
}

// LookupTimes draws one uniformly random time in each of n equal intervals of
// [from, to) (unix ms): the plan's "L lookups per node" schedule.
func LookupTimes(rng *rand.Rand, from, to int64, n int) []int64 {
	if n <= 0 || to <= from {
		return nil
	}
	span := float64(to-from) / float64(n)
	out := make([]int64, n)
	for k := 0; k < n; k++ {
		out[k] = from + int64(float64(k)*span+rng.Float64()*span)
	}
	return out
}

// legacySet picks the legacy (stock discv5) nodes: the same fraction within
// every service, as the partial-deployment sweep requires, rounded per
// service and drawn from the seed.
func legacySet(rng *rand.Rand, topics [][]string, frac float64) []bool {
	set := make([]bool, len(topics))
	if frac <= 0 {
		return set
	}
	byTopic := map[string][]int{}
	for i, t := range topics {
		byTopic[t[0]] = append(byTopic[t[0]], i)
	}
	keys := make([]string, 0, len(byTopic))
	for k := range byTopic {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		members := byTopic[k]
		count := int(math.Round(frac * float64(len(members))))
		for _, j := range rng.Perm(len(members))[:count] {
			set[members[j]] = true
		}
	}
	return set
}

// Zipf draws topic indices 0..n-1 with P(k) ∝ 1/(k+1)^s. Unlike math/rand's
// Zipf it accepts s <= 1, which the plan's α = 1 needs.
type Zipf struct{ cdf []float64 }

func NewZipf(s float64, n int) *Zipf {
	cdf := make([]float64, n)
	sum := 0.0
	for k := 0; k < n; k++ {
		sum += 1 / math.Pow(float64(k+1), s)
		cdf[k] = sum
	}
	for k := range cdf {
		cdf[k] /= sum
	}
	return &Zipf{cdf}
}

func (z *Zipf) Draw(rng *rand.Rand) int {
	u := rng.Float64()
	lo, hi := 0, len(z.cdf)-1
	for lo < hi {
		mid := (lo + hi) / 2
		if z.cdf[mid] < u {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// StartOffsets spreads node starts uniformly at random over window (ms since
// the run's start), ascending by node index so the bootnode (node 0) starts
// first. A node registers bootstrap_wait after its start, so the same offsets
// spread registrations and ad expiries. A zero window starts everyone at once.
func StartOffsets(seed int64, n int, window time.Duration) []int64 {
	o := sortedUniform(seed+7919, n, window)
	if n > 0 {
		o[0] = 0
	}
	return o
}

func sortedUniform(seed int64, n int, window time.Duration) []int64 {
	out := make([]int64, n)
	if window <= 0 || n == 0 {
		return out
	}
	rng := rand.New(rand.NewSource(seed))
	w := window.Milliseconds()
	for i := range out {
		out[i] = rng.Int63n(w + 1)
	}
	sort.Slice(out, func(a, b int) bool { return out[a] < out[b] })
	return out
}
