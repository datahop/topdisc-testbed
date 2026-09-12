// Package assign turns a scenario into per-node assignments: everything one
// node process needs, derived deterministically from the scenario and its seed.
package assign

import (
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"

	"github.com/datahop/topdisc-testbed/pkg/churn"
	"github.com/datahop/topdisc-testbed/pkg/scenario"
	"github.com/ethereum/go-ethereum/crypto"
)

type Phases struct {
	RegisterAt int64 `json:"register_at_ms"` // absolute unix ms
	SearchAt   int64 `json:"search_at_ms"`
	StopAt     int64 `json:"stop_at_ms"`
}

type Search struct {
	Model          string `json:"model"`
	TargetCount    int    `json:"target_count"`
	RequestDelayMs int64  `json:"request_delay_ms"`
	RequestTimeout int64  `json:"request_timeout_ms"`
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
		z := rand.NewZipf(rng, sc.Population.ZipfS, 1, uint64(sc.Population.Topics-1))
		for i := range topics {
			topics[i] = []string{fmt.Sprintf("topic-%d", z.Uint64())}
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
	registerAt := t0 + ph.BootstrapWait.Milliseconds()
	searchAt := registerAt + int64(n)*ph.RegisterStagger.Milliseconds() + ph.RegisterWait.Milliseconds()
	stopAt := searchAt + ph.SearchTimeout.Milliseconds()
	// Node 0 is the bootnode for everyone.
	h0 := hosts(0)
	boot := fmt.Sprintf("enode://%s@%s:%d", hex.EncodeToString(crypto.FromECDSAPub(&keys[0].PublicKey)[1:]), h0.IP, h0.BasePort)
	out := make([]Assignment, n)
	for i := range out {
		h := hosts(i)
		a := Assignment{
			Idx: i, Key: hex.EncodeToString(crypto.FromECDSA(keys[i])), IP: h.IP, Port: h.BasePort, StatusPort: h.BasePort + h.StatusOff, TraceFile: h.TraceFile,
			Topics: topics[i], MaxPeers: sc.ConnModel.MaxPeers, DialRatio: sc.ConnModel.DialRatio,
			Phases: Phases{RegisterAt: registerAt + int64(i)*ph.RegisterStagger.Milliseconds(), SearchAt: searchAt + int64(i)*ph.SearchStagger.Milliseconds(), StopAt: stopAt},
			Search: Search{Model: sc.Search.Model, TargetCount: sc.Search.TargetCount, RequestDelayMs: sc.Search.RequestDelay.Milliseconds(), RequestTimeout: sc.Search.RequestTimeout.Milliseconds()},
		}
		if i != 0 {
			a.Bootnodes = []string{boot}
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
