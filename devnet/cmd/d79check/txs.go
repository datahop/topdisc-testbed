package main

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// ethereum-package prefunded accounts m/44'/60'/0'/0/20 (sender) and /19 (recipient),
// src/prelaunch_data_generator/genesis_constants/genesis_constants.star.
const (
	defaultTxKey = "04b9f63ecf84210c5366c66d68fa1f5da1fa4f634fad6dfc86178e4d79ff9e59"
	defaultTxTo  = "0xD8F3183DEF51A987222D845be228e0Bbb932C222"
)

type txRecord struct {
	Hash        string             `json:"hash"`
	Via         string             `json:"via"`
	ViaKind     string             `json:"via_kind"`
	Nonce       uint64             `json:"nonce"`
	Sent        time.Time          `json:"sent"`
	BlockAtSend uint64             `json:"block_at_send"`
	MinedBlock  *uint64            `json:"mined_block,omitempty"`
	SeenMs      map[string]float64 `json:"seen_ms"`
	MinedFirst  []string           `json:"mined_first,omitempty"`
	Unseen      []string           `json:"unseen,omitempty"`
	Error       string             `json:"error,omitempty"`
}

type txsResult struct {
	ChainID   uint64     `json:"chain_id"`
	From      string     `json:"from"`
	To        string     `json:"to"`
	PerVia    int        `json:"per_via"`
	SeenLimit float64    `json:"seen_limit_s"`
	MineLimit uint64     `json:"mine_limit_blocks"`
	PollMs    float64    `json:"poll_ms"`
	Txs       []txRecord `json:"txs"`
	Check     check      `json:"check"`
}

func init() {
	register("txs", "E3 tx gossip: signed transfers via one OLD and one NEW EL", runTxs)
}

func runTxs(args []string) error {
	fs := flag.NewFlagSet("txs", flag.ExitOnError)
	rf := addRunFlags(fs)
	n := fs.Int("n", 20, "transfers per injecting node")
	keyHex := fs.String("key", defaultTxKey, "sender private key (hex)")
	to := fs.String("to", defaultTxTo, "recipient address")
	oldVia := fs.String("old", "", "OLD injecting EL (default: first OLD that is not participant 0)")
	newVia := fs.String("new", "", "NEW injecting EL (default: first NEW that is not participant 0)")
	seenLimit := fs.Duration("seen", 10*time.Second, "pending visibility limit")
	mineLimit := fs.Uint64("blocks", 2, "inclusion limit in blocks after the head at send")
	poll := fs.Duration("poll", 100*time.Millisecond, "pending poll interval per EL (latency resolution)")
	fs.Parse(args)
	if err := rf.check(); err != nil {
		return err
	}
	inv, err := rf.inventory()
	if err != nil {
		return err
	}
	key, err := crypto.HexToECDSA(*keyHex)
	if err != nil {
		return fmt.Errorf("-key: %v", err)
	}
	if !common.IsHexAddress(*to) {
		return fmt.Errorf("-to: bad address %q", *to)
	}
	var vias []node
	for kind, name := range map[string]string{kindOld: *oldVia, kindNew: *newVia} {
		if v, ok := pickVia(inv, kind, name); ok {
			vias = append(vias, v)
		} else if name != "" {
			return fmt.Errorf("no %s EL named %s", kind, name)
		}
	}
	sort.Slice(vias, func(i, j int) bool { return vias[i].Kind > vias[j].Kind }) // old first
	if len(vias) == 0 {
		return errors.New("no injecting node")
	}
	tc := &txCampaign{inv: inv, key: key, to: common.HexToAddress(*to), seenLimit: *seenLimit, mineLimit: *mineLimit, poll: *poll}
	res, err := tc.run(context.Background(), vias, *n)
	if err != nil {
		return err
	}
	printCheck(res.Check)
	return writeJSON(filepath.Join(rf.out, "txs.json"), res)
}

func pickVia(inv *inventory, kind, name string) (node, bool) {
	var first *node
	for i, n := range inv.Nodes {
		if n.Kind != kind {
			continue
		}
		if name != "" {
			if n.Name == name {
				return n, true
			}
			continue
		}
		if i > 0 {
			return n, true
		}
		first = &inv.Nodes[i]
	}
	if first != nil && name == "" {
		return *first, true
	}
	return node{}, false
}

type txCampaign struct {
	inv       *inventory
	key       *ecdsa.PrivateKey
	to        common.Address
	seenLimit time.Duration
	mineLimit uint64
	poll      time.Duration
}

func (tc *txCampaign) run(ctx context.Context, vias []node, perVia int) (*txsResult, error) {
	if tc.poll == 0 {
		tc.poll = 100 * time.Millisecond
	}
	from := crypto.PubkeyToAddress(tc.key.PublicKey)
	var chainID hexutil.Uint64
	if err := rpcCall(ctx, elURL(vias[0]), "eth_chainId", &chainID); err != nil {
		return nil, err
	}
	var nonce hexutil.Uint64
	if err := rpcCall(ctx, elURL(vias[0]), "eth_getTransactionCount", &nonce, from, "pending"); err != nil {
		return nil, err
	}
	res := &txsResult{ChainID: uint64(chainID), From: from.Hex(), To: tc.to.Hex(), PerVia: perVia,
		SeenLimit: tc.seenLimit.Seconds(), MineLimit: tc.mineLimit, PollMs: float64(tc.poll.Milliseconds())}
	signer := types.LatestSignerForChainID(new(big.Int).SetUint64(uint64(chainID)))
	next := uint64(nonce)
	for _, via := range vias {
		w := new(txWatch)
		var wg sync.WaitGroup
		for _, n := range tc.inv.Nodes {
			if n.Name != via.Name {
				wg.Add(1)
				go func() {
					defer wg.Done()
					tc.watchNode(ctx, n, w)
				}()
			}
		}
		err := tc.inject(ctx, via, signer, &next, perVia, w)
		w.finish()
		wg.Wait()
		if err != nil {
			return nil, fmt.Errorf("via %s: %v", via.Name, err)
		}
		logf("txs: %d sent via %s (%s)", len(w.recs), via.Name, via.Kind)
		for _, r := range w.recs {
			for _, n := range tc.inv.Nodes {
				_, ok := r.SeenMs[n.Name]
				if !ok && n.Name != via.Name && !slices.Contains(r.MinedFirst, n.Name) {
					r.Unseen = append(r.Unseen, n.Name)
				}
			}
		}
		tc.watchMined(ctx, via, w.recs)
		for _, r := range w.recs {
			res.Txs = append(res.Txs, *r)
		}
	}
	res.Check = evalTxs(tc.inv, res)
	return res, nil
}

// txWatch is the list of sent txs shared between the injector and the per-node watchers.
type txWatch struct {
	mu   sync.Mutex
	recs []*txRecord
	done bool
}

func (w *txWatch) add(r *txRecord) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.recs = append(w.recs, r)
}

func (w *txWatch) finish() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.done = true
}

func (w *txWatch) snapshot() ([]*txRecord, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.recs), w.done
}

func (tc *txCampaign) inject(ctx context.Context, via node, signer types.Signer, nonce *uint64, n int, w *txWatch) error {
	var tip hexutil.Big
	if err := rpcCall(ctx, elURL(via), "eth_maxPriorityFeePerGas", &tip); err != nil {
		return err
	}
	var head struct {
		BaseFee hexutil.Big `json:"baseFeePerGas"`
	}
	if err := rpcCall(ctx, elURL(via), "eth_getBlockByNumber", &head, "latest", false); err != nil {
		return err
	}
	gtip := max(tip.ToInt().Uint64(), 1_000_000_000)
	feeCap := new(big.Int).Add(new(big.Int).Mul(head.BaseFee.ToInt(), big.NewInt(2)), new(big.Int).SetUint64(gtip))
	for range n {
		var bn hexutil.Uint64
		if err := rpcCall(ctx, elURL(via), "eth_blockNumber", &bn); err != nil {
			return err
		}
		tx, err := types.SignNewTx(tc.key, signer, &types.DynamicFeeTx{
			ChainID: signer.ChainID(), Nonce: *nonce, GasTipCap: new(big.Int).SetUint64(gtip), GasFeeCap: feeCap,
			Gas: 21000, To: &tc.to, Value: big.NewInt(1),
		})
		if err != nil {
			return err
		}
		raw, _ := tx.MarshalBinary()
		sent := time.Now().UTC()
		var hash common.Hash
		if err := rpcCall(ctx, elURL(via), "eth_sendRawTransaction", &hash, hexutil.Bytes(raw)); err != nil {
			return fmt.Errorf("nonce %d: %v", *nonce, err)
		}
		w.add(&txRecord{Hash: hash.Hex(), Via: via.Name, ViaKind: via.Kind, Nonce: *nonce, Sent: sent,
			BlockAtSend: uint64(bn), SeenMs: make(map[string]float64)})
		*nonce++
	}
	return nil
}

// watchNode polls one EL, from before the first send, until every tx was seen or its
// seenLimit passed. A tx counts when a response that completes within the limit shows
// it pending (no blockNumber); the latency is the time to that first poll hit.
func (tc *txCampaign) watchNode(ctx context.Context, n node, w *txWatch) {
	finished := make(map[*txRecord]bool)
	for {
		recs, done := w.snapshot()
		for _, r := range recs {
			if finished[r] {
				continue
			}
			deadline := r.Sent.Add(tc.seenLimit)
			if !time.Now().Before(deadline) {
				finished[r] = true
				continue
			}
			cctx, cancel := context.WithDeadline(ctx, deadline)
			var tx *struct {
				BlockNumber *string `json:"blockNumber"`
			}
			err := rpcCall(cctx, elURL(n), "eth_getTransactionByHash", &tx, r.Hash)
			at := time.Since(r.Sent)
			cancel()
			if err != nil || tx == nil {
				continue
			}
			finished[r] = true
			if at > tc.seenLimit {
				continue
			}
			w.mu.Lock()
			if tx.BlockNumber == nil {
				r.SeenMs[n.Name] = float64(at.Microseconds()) / 1000
			} else {
				r.MinedFirst = append(r.MinedFirst, n.Name)
			}
			w.mu.Unlock()
		}
		if done && len(finished) == len(recs) {
			return
		}
		time.Sleep(tc.poll)
	}
}

// watchMined waits for receipts on the injecting node until the head passes the inclusion limit.
func (tc *txCampaign) watchMined(ctx context.Context, via node, recs []*txRecord) {
	last := recs[len(recs)-1].BlockAtSend + tc.mineLimit + 1
	deadline := time.Now().Add(time.Duration(tc.mineLimit+3) * slotTime)
	for time.Now().Before(deadline) {
		pending := 0
		for j := range recs {
			if recs[j].MinedBlock != nil {
				continue
			}
			var r *struct {
				BlockNumber hexutil.Uint64 `json:"blockNumber"`
			}
			if err := rpcCall(ctx, elURL(via), "eth_getTransactionReceipt", &r, recs[j].Hash); err != nil || r == nil {
				pending++
				continue
			}
			b := uint64(r.BlockNumber)
			recs[j].MinedBlock = &b
		}
		var bn hexutil.Uint64
		if pending == 0 || (rpcCall(ctx, elURL(via), "eth_blockNumber", &bn) == nil && uint64(bn) > last) {
			return
		}
		time.Sleep(time.Second)
	}
}

func evalTxs(inv *inventory, res *txsResult) check {
	byVia := make(map[string][]txRecord)
	var order []string
	for _, r := range res.Txs {
		if _, ok := byVia[r.Via]; !ok {
			order = append(order, r.Via)
		}
		byVia[r.Via] = append(byVia[r.Via], r)
	}
	var sub []check
	for _, via := range order {
		recs := byVia[via]
		var lat []float64
		unseen, minedFirst, late, unmined := 0, 0, 0, 0
		for _, r := range recs {
			for _, ms := range r.SeenMs {
				lat = append(lat, ms)
			}
			unseen += len(r.Unseen)
			minedFirst += len(r.MinedFirst)
			switch {
			case r.MinedBlock == nil:
				unmined++
			case *r.MinedBlock > r.BlockAtSend+res.MineLimit:
				late++
			}
		}
		sort.Float64s(lat)
		// Sends are a burst, so the first poll hit only bounds the propagation time.
		d := fmt.Sprintf("%d txs to %d ELs: unseen %d, mined before seen pending %d, mined late %d, unmined %d; max first poll hit %.0f ms (poll %.0f ms, an upper bound)",
			len(recs), len(inv.Nodes)-1, unseen, minedFirst, late, unmined, quantile(lat, 1), res.PollMs)
		sub = append(sub, check{ID: "E3b-" + recs[0].ViaKind, Desc: "via " + via, Detail: d,
			Status: passIf(unseen == 0 && minedFirst == 0 && late == 0 && unmined == 0 && len(recs) == res.PerVia)})
	}
	return check{ID: "E3b", Desc: fmt.Sprintf("tx gossip: pending on all ELs ≤ %.0f s, mined ≤ %d blocks", res.SeenLimit, res.MineLimit),
		Status: overall(sub), Sub: sub}
}

// quantile of sorted values (nearest rank), 0 when empty.
func quantile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(q*float64(len(sorted)-1) + 0.5)
	return sorted[i]
}
