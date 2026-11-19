package main

import (
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
)

const testChainID = 3151908

func strp(s string) *string { return &s }

// testInventory has the d79-smoke node IDs: el-1 and el-4 OLD, el-2 and el-3 NEW.
func testInventory() *inventory {
	inv := &inventory{Nodes: []node{
		{Name: "el-1-geth-lighthouse", Kind: kindOld, IP: "172.16.0.12", ID: "5dd048d94d3a599e58c5ffadb1dd864889a099848bbd28d243788e43e1b80cc9", ENR: "enr:-old1", Image: "ethereum/client-go:v1.17.10"},
		{Name: "el-2-geth-lighthouse", Kind: kindNew, IP: "172.16.0.15", ID: "4822759436a4e6794efa0092310a5f49ddb8d8f09ae746871d6d291215f18c15", ENR: "enr:-new2", TD: strp("01"), Image: "topdisc-geth:3b8bef9e31d8"},
		{Name: "el-3-geth-lighthouse", Kind: kindNew, IP: "172.16.0.13", ID: "78de8ec159533c80ef806b933c4215f17b2ed07884d1575d252a90077cf6bf4c", ENR: "enr:-new3", TD: strp("01"), Image: "topdisc-geth:3b8bef9e31d8"},
		{Name: "el-4-geth-lighthouse", Kind: kindOld, IP: "172.16.0.14", ID: "121c802941f418ce8fdeb661d21e713b9cfab3caddf7166d1ebb4df80d768d64", ENR: "enr:-old4", Image: "ethereum/client-go:v1.17.10"},
	}}
	if errs := inv.validate(); len(errs) > 0 {
		panic(strings.Join(errs, "; "))
	}
	return inv
}

// fakeNet serves JSON-RPC at /el/<name>, metrics at /metrics/<name> and the beacon API
// at /cl/<name>/… for every node.
type fakeNet struct {
	t   *testing.T
	inv *inventory
	srv *httptest.Server

	mu        sync.Mutex
	head      map[string]uint64
	finalized map[string]uint64
	peers     map[string][]map[string]any
	metrics   map[string]map[string]any
	slots     map[string]uint64
	finEpoch  map[string]uint64
	deaf      map[string]bool          // never sees gossiped txs
	minedOnly map[string]bool          // reports txs as already mined
	delay     map[string]time.Duration // RPC response delay
	pool      map[string]map[string]bool
	mined     map[string]uint64
	senders   []string
	nonces    []uint64
}

func newFakeNet(t *testing.T, inv *inventory) *fakeNet {
	f := &fakeNet{t: t, inv: inv, head: map[string]uint64{}, finalized: map[string]uint64{},
		peers: map[string][]map[string]any{}, metrics: map[string]map[string]any{}, slots: map[string]uint64{},
		finEpoch: map[string]uint64{}, deaf: map[string]bool{}, minedOnly: map[string]bool{}, delay: map[string]time.Duration{}, pool: map[string]map[string]bool{}, mined: map[string]uint64{}}
	for _, n := range inv.Nodes {
		f.head[n.Name], f.finalized[n.Name], f.pool[n.Name] = 100, 64, map[string]bool{}
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	oldEL, oldMetrics := elURL, metricsURL
	elURL = func(n node) string { return f.srv.URL + "/el/" + n.Name }
	metricsURL = func(n node) string { return f.srv.URL + "/metrics/" + n.Name }
	t.Cleanup(func() { elURL, metricsURL = oldEL, oldMetrics })
	return f
}

func (f *fakeNet) clList() string {
	var parts []string
	for i := range f.inv.Nodes {
		name := fmt.Sprintf("cl-%d-lighthouse-geth", i+1)
		parts = append(parts, name+"="+f.srv.URL+"/cl/"+name)
	}
	return strings.Join(parts, ",")
}

func blockHash(n uint64) string { return fmt.Sprintf("0x%064x", n) }

func (f *fakeNet) serve(w http.ResponseWriter, r *http.Request) {
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 3)
	f.mu.Lock()
	d := f.delay[parts[1]]
	f.mu.Unlock()
	time.Sleep(d)
	f.mu.Lock()
	defer f.mu.Unlock()
	switch parts[0] {
	case "metrics":
		json.NewEncoder(w).Encode(f.metrics[parts[1]])
	case "cl":
		name := parts[1]
		switch "/" + parts[2] {
		case "/eth/v1/beacon/headers/head":
			fmt.Fprintf(w, `{"data":{"header":{"message":{"slot":"%d"}}}}`, f.slots[name])
		case "/eth/v1/beacon/states/head/finality_checkpoints":
			fmt.Fprintf(w, `{"data":{"finalized":{"epoch":"%d","root":"0x00"}}}`, f.finEpoch[name])
		case "/eth/v1/beacon/genesis":
			fmt.Fprint(w, `{"data":{"genesis_time":"1795015835"}}`)
		default:
			http.NotFound(w, r)
		}
	case "el":
		f.serveRPC(w, r, parts[1])
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeNet) serveRPC(w http.ResponseWriter, r *http.Request, name string) {
	var req struct {
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	str := func(i int) string {
		var s string
		json.Unmarshal(req.Params[i], &s)
		return s
	}
	var result any
	switch req.Method {
	case "admin_peers":
		result = f.peers[name]
	case "eth_getBlockByNumber":
		n := f.head[name]
		if str(0) == "finalized" {
			n = f.finalized[name]
			if n == 0 {
				json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "error": map[string]any{"code": -39001, "message": "finalized block not found"}})
				return
			}
		}
		result = map[string]any{"number": hexutil.Uint64(n), "hash": blockHash(n), "baseFeePerGas": "0x7"}
	case "eth_syncing":
		result = false
	case "eth_chainId":
		result = hexutil.Uint64(testChainID)
	case "eth_getTransactionCount":
		result = hexutil.Uint64(5 + len(f.senders))
	case "eth_maxPriorityFeePerGas":
		result = "0x1"
	case "eth_blockNumber":
		result = hexutil.Uint64(f.head[name])
	case "eth_sendRawTransaction":
		var raw hexutil.Bytes
		json.Unmarshal(req.Params[0], &raw)
		tx := new(types.Transaction)
		if err := tx.UnmarshalBinary(raw); err != nil {
			f.t.Errorf("bad raw tx: %v", err)
			return
		}
		from, err := types.Sender(types.LatestSignerForChainID(big.NewInt(testChainID)), tx)
		if err != nil {
			f.t.Errorf("sender: %v", err)
		}
		f.senders, f.nonces = append(f.senders, from.Hex()), append(f.nonces, tx.Nonce())
		h := tx.Hash().Hex()
		for _, n := range f.inv.Nodes {
			if !f.deaf[n.Name] || n.Name == name {
				f.pool[n.Name][h] = true
			}
		}
		f.mined[h] = f.head[name] + 1
		result = h
	case "eth_getTransactionByHash":
		if f.pool[name][str(0)] {
			result = map[string]any{"hash": str(0), "blockNumber": nil}
			if f.minedOnly[name] {
				result = map[string]any{"hash": str(0), "blockNumber": "0x65"}
			}
		}
	case "eth_getTransactionReceipt":
		if b, ok := f.mined[str(0)]; ok {
			result = map[string]any{"blockNumber": hexutil.Uint64(b)}
		}
	default:
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "error": map[string]any{"code": -32601, "message": "no method " + req.Method}})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
}

func (f *fakeNet) set(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

func (f *fakeNet) setPeers(name string, peers ...map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.peers[name] = peers
}

func peerEntry(n node, inbound bool) map[string]any {
	pname := "Geth/v1.17.10-stable-1df30b18/linux-amd64/go1.27.3"
	if n.Kind == kindNew {
		pname = "Geth/topdisc/v1.17.10-stable-3b8bef9e/linux-amd64/go1.25.0"
	}
	return map[string]any{"id": n.ID, "name": pname, "network": map[string]any{"inbound": inbound}}
}

// writeRunDir writes inventory.json and genesis.txt and links testdata/logs.
func writeRunDir(t *testing.T, inv *inventory) string {
	dir := filepath.Join(t.TempDir(), "2026-11-18-d79-test")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, "inventory.json"), inv.Nodes); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "genesis.txt"), []byte("1795015835\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	abs, _ := filepath.Abs("testdata/logs")
	if err := os.Symlink(abs, filepath.Join(dir, "logs")); err != nil {
		t.Fatal(err)
	}
	return dir
}
