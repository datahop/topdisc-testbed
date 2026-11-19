package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	slotTime  = 12 * time.Second
	epochTime = 32 * slotTime
)

type clNode struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

type blockRef struct {
	Number uint64 `json:"number"`
	Hash   string `json:"hash"`
}

type elSample struct {
	Name      string    `json:"name"`
	Latest    *blockRef `json:"latest,omitempty"`
	Finalized *blockRef `json:"finalized,omitempty"`
	Syncing   *bool     `json:"syncing,omitempty"`
	// NotFinalized: the EL answered "finalized block not found".
	NotFinalized bool   `json:"not_finalized,omitempty"`
	Error        string `json:"error,omitempty"`
}

type clSample struct {
	Name           string  `json:"name"`
	HeadSlot       *uint64 `json:"head_slot,omitempty"`
	FinalizedEpoch *uint64 `json:"finalized_epoch,omitempty"`
	Error          string  `json:"error,omitempty"`
}

type headSample struct {
	Time    time.Time  `json:"time"`
	Elapsed float64    `json:"elapsed_s"`
	ELs     []elSample `json:"els"`
	CLs     []clSample `json:"cls"`
}

type headsResult struct {
	Genesis  time.Time     `json:"genesis"`
	Samples  int           `json:"samples"`
	First    time.Time     `json:"first"`
	Last     time.Time     `json:"last"`
	Coverage headsCoverage `json:"coverage"`
	Check    check         `json:"check"`
}

func init() {
	register("heads", "sample EL/CL heads every 12 s for a duration, E3 heads", runHeads)
}

var clNameRe = regexp.MustCompile(`^cl-0*(\d+)-`)

// clNodes finds the beacon nodes of the enclave (cl-N-…:4000), or parses -cl name=url,….
func clNodes(enclave, override string) ([]clNode, error) {
	var cls []clNode
	if override != "" {
		for _, kv := range strings.Split(override, ",") {
			name, url, ok := strings.Cut(kv, "=")
			if !ok {
				return nil, fmt.Errorf("-cl: bad entry %q", kv)
			}
			cls = append(cls, clNode{name, url})
		}
		return cls, nil
	}
	ips, _, err := enclaveServices(enclave)
	if err != nil {
		return nil, err
	}
	for name, ip := range ips {
		if clNameRe.MatchString(name) {
			cls = append(cls, clNode{name, "http://" + ip + ":4000"})
		}
	}
	sort.Slice(cls, func(i, j int) bool { return clIndex(cls[i].Name) < clIndex(cls[j].Name) })
	if len(cls) == 0 {
		return nil, fmt.Errorf("no cl-* services in enclave %s", enclave)
	}
	return cls, nil
}

func clIndex(name string) int {
	m := clNameRe.FindStringSubmatch(name)
	if m == nil {
		return math.MaxInt
	}
	i, _ := strconv.Atoi(m[1])
	return i
}

func runHeads(args []string) error {
	fs := flag.NewFlagSet("heads", flag.ExitOnError)
	rf := addRunFlags(fs)
	duration := fs.Duration("duration", 75*time.Minute, "sampling duration, a full M4 run by default")
	interval := fs.Duration("interval", slotTime, "sampling interval")
	enclave := fs.String("enclave", "", "enclave name (default: from -run, <date>-d79-<run> → d79-<run>)")
	clList := fs.String("cl", "", "beacon APIs as name=url,… instead of docker discovery")
	fs.Parse(args)
	if err := rf.check(); err != nil {
		return err
	}
	inv, err := rf.inventory()
	if err != nil {
		return err
	}
	if *enclave == "" {
		*enclave = rf.enclave()
	}
	cls, err := clNodes(*enclave, *clList)
	if err != nil {
		return err
	}
	if len(cls) != len(inv.Nodes) {
		logf("heads: %d beacon nodes for %d ELs in the inventory; E3c will be incomplete", len(cls), len(inv.Nodes))
	}
	genesis, err := rf.genesis()
	if err != nil {
		if genesis, err = clGenesis(cls); err != nil {
			return fmt.Errorf("genesis: %v", err)
		}
	}
	f, err := os.Create(filepath.Join(rf.out, "heads.jsonl"))
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	var samples []headSample
	end := time.Now().Add(*duration)
	tick := time.NewTicker(*interval)
	defer tick.Stop()
	for {
		s := sampleHeads(context.Background(), inv, cls, genesis)
		samples = append(samples, s)
		b, _ := json.Marshal(s)
		w.Write(append(b, '\n'))
		w.Flush()
		logf("heads sample %d: %s", len(samples), sampleLine(s))
		if !time.Now().Before(end) {
			break
		}
		<-tick.C
	}
	res := headsResult{Genesis: genesis, Samples: len(samples), First: samples[0].Time, Last: samples[len(samples)-1].Time}
	res.Check, res.Coverage = evalHeads(samples, len(inv.Nodes))
	printCheck(res.Check)
	return writeJSON(filepath.Join(rf.out, "heads.json"), res)
}

func clGenesis(cls []clNode) (time.Time, error) {
	var g struct {
		Data struct {
			GenesisTime string `json:"genesis_time"`
		} `json:"data"`
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpGetJSON(ctx, cls[0].URL+"/eth/v1/beacon/genesis", &g); err != nil {
		return time.Time{}, err
	}
	s, err := strconv.ParseInt(g.Data.GenesisTime, 10, 64)
	return time.Unix(s, 0).UTC(), err
}

func sampleLine(s headSample) string {
	var b strings.Builder
	for _, e := range s.ELs {
		if e.Latest != nil {
			fmt.Fprintf(&b, "%s=%d ", e.Name, e.Latest.Number)
		} else {
			fmt.Fprintf(&b, "%s=err ", e.Name)
		}
	}
	for _, c := range s.CLs {
		if c.HeadSlot != nil && c.FinalizedEpoch != nil {
			fmt.Fprintf(&b, "%s=%d/%d ", c.Name, *c.HeadSlot, *c.FinalizedEpoch)
		} else {
			fmt.Fprintf(&b, "%s=err ", c.Name)
		}
	}
	return strings.TrimSpace(b.String())
}

func sampleHeads(ctx context.Context, inv *inventory, cls []clNode, genesis time.Time) headSample {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	now := time.Now().UTC()
	s := headSample{Time: now, Elapsed: now.Sub(genesis).Seconds(),
		ELs: make([]elSample, len(inv.Nodes)), CLs: make([]clSample, len(cls))}
	forEach(len(inv.Nodes), func(i int) {
		n := inv.Nodes[i]
		e := elSample{Name: n.Name}
		var errs []string
		if b, err := blockByNumber(ctx, elURL(n), "latest"); err != nil {
			errs = append(errs, err.Error())
		} else {
			e.Latest = b
		}
		if b, err := blockByNumber(ctx, elURL(n), "finalized"); err != nil && strings.Contains(err.Error(), "finalized block not found") {
			e.NotFinalized = true
		} else if err != nil {
			errs = append(errs, err.Error())
		} else {
			e.Finalized = b
		}
		var syncing json.RawMessage
		if err := rpcCall(ctx, elURL(n), "eth_syncing", &syncing); err != nil {
			errs = append(errs, err.Error())
		} else {
			v := string(syncing) != "false"
			e.Syncing = &v
		}
		e.Error = strings.Join(errs, "; ")
		s.ELs[i] = e
	})
	forEach(len(cls), func(i int) {
		c := clSample{Name: cls[i].Name}
		var errs []string
		var head struct {
			Data struct {
				Header struct {
					Message struct {
						Slot string `json:"slot"`
					} `json:"message"`
				} `json:"header"`
			} `json:"data"`
		}
		if err := httpGetJSON(ctx, cls[i].URL+"/eth/v1/beacon/headers/head", &head); err != nil {
			errs = append(errs, err.Error())
		} else if v, err := strconv.ParseUint(head.Data.Header.Message.Slot, 10, 64); err != nil {
			errs = append(errs, "head slot: "+err.Error())
		} else {
			c.HeadSlot = &v
		}
		var fin struct {
			Data struct {
				Finalized struct {
					Epoch string `json:"epoch"`
				} `json:"finalized"`
			} `json:"data"`
		}
		if err := httpGetJSON(ctx, cls[i].URL+"/eth/v1/beacon/states/head/finality_checkpoints", &fin); err != nil {
			errs = append(errs, err.Error())
		} else if v, err := strconv.ParseUint(fin.Data.Finalized.Epoch, 10, 64); err != nil {
			errs = append(errs, "finalized epoch: "+err.Error())
		} else {
			c.FinalizedEpoch = &v
		}
		c.Error = strings.Join(errs, "; ")
		s.CLs[i] = c
	})
	return s
}

func blockByNumber(ctx context.Context, url, tag string) (*blockRef, error) {
	var b *struct {
		Number string `json:"number"`
		Hash   string `json:"hash"`
	}
	if err := rpcCall(ctx, url, "eth_getBlockByNumber", &b, tag, false); err != nil {
		return nil, err
	}
	if b == nil {
		return nil, fmt.Errorf("no %s block", tag)
	}
	n, err := parseHexUint(b.Number)
	if err != nil {
		return nil, err
	}
	return &blockRef{n, b.Hash}, nil
}

// headsCoverage records the windows the E3c criteria were judged in.
type headsCoverage struct {
	FirstS          float64  `json:"first_s"`
	LastS           float64  `json:"last_s"`
	FirstFinalizedS *float64 `json:"first_finalized_s,omitempty"`
	SyncingFromS    *float64 `json:"syncing_from_s,omitempty"`
	SyncingToS      *float64 `json:"syncing_to_s,omitempty"`
}

// evalHeads applies the E3 head criteria to a series of samples of n ELs and n CLs.
// Finality is judged from the first sample in which a CL reports finalized epoch ≥ 1.
func evalHeads(samples []headSample, n int) (check, headsCoverage) {
	var cov headsCoverage
	if len(samples) == 0 {
		return check{ID: "E3c", Desc: "heads", Status: statusMissing, Detail: "no samples"}, cov
	}
	cov.FirstS, cov.LastS = samples[0].Elapsed, samples[len(samples)-1].Elapsed
	first := -1
	for i, s := range samples {
		for _, c := range s.CLs {
			if c.FinalizedEpoch != nil && *c.FinalizedEpoch >= 1 && first < 0 {
				first = i
				cov.FirstFinalizedS = &samples[i].Elapsed
			}
		}
	}
	var (
		errs, population, hashConflicts, finFails, finalityFails, syncingLate []string
		nodeErrs                                                              = make(map[string]int)
		seen                                                                  = make(map[string]map[string]bool)
		maxDistinct, maxSlotSpread                                            int
		hashOf                                                                = make(map[uint64]string)
		lastFin, lastEpoch                                                    = make(map[string]uint64), make(map[string]uint64)
		graced                                                                = make(map[string]bool)
	)
	obs := func(name, field string) {
		if seen[name] == nil {
			seen[name] = make(map[string]bool)
		}
		seen[name][field] = true
	}
	seenHash := func(where string, b *blockRef) bool {
		if h, ok := hashOf[b.Number]; ok && h != b.Hash {
			hashConflicts = append(hashConflicts, fmt.Sprintf("%s block %d %s vs %s", where, b.Number, short(h), short(b.Hash)))
			return false
		} else if !ok {
			hashOf[b.Number] = b.Hash
		}
		return true
	}
	addErr := func(s headSample, name, e string) {
		errs = append(errs, fmt.Sprintf("%s %s: %s", s.Time.Format("15:04:05"), name, e))
		nodeErrs[name]++
	}
	for i, s := range samples {
		after := first >= 0 && i >= first
		at := fmt.Sprintf("+%.0fs", s.Elapsed)
		if len(s.ELs) != n || len(s.CLs) != n {
			population = append(population, fmt.Sprintf("%s: %d ELs, %d CLs, want %d each", at, len(s.ELs), len(s.CLs), n))
		}
		distinct := make(map[blockRef]bool)
		for _, e := range s.ELs {
			obs("EL "+e.Name, "")
			if e.Error != "" {
				addErr(s, e.Name, e.Error)
			}
			if e.Latest != nil {
				obs("EL "+e.Name, "latest")
				distinct[*e.Latest] = true
				seenHash(e.Name, e.Latest)
			}
			_, hadFin := lastFin[e.Name]
			switch {
			case e.Finalized != nil:
				obs("EL "+e.Name, "finalized")
				if after {
					obs("EL "+e.Name, "finalized after first finality")
				}
				if !seenHash(e.Name+" finalized", e.Finalized) {
					finFails = append(finFails, fmt.Sprintf("%s finalized %d hash differs", e.Name, e.Finalized.Number))
				}
				if prev := lastFin[e.Name]; hadFin && e.Finalized.Number < prev {
					finFails = append(finFails, fmt.Sprintf("%s %d→%d at %s", e.Name, prev, e.Finalized.Number, at))
				}
				lastFin[e.Name] = e.Finalized.Number
			case e.NotFinalized && hadFin:
				finFails = append(finFails, fmt.Sprintf("%s finalized block disappeared at %s", e.Name, at))
			case e.NotFinalized && !after:
				obs("EL "+e.Name, "finalized")
			case e.NotFinalized && !graced[e.Name]:
				// One sample of grace: the EL may import the finalizing block just after its CL.
				graced[e.Name] = true
			case e.NotFinalized:
				addErr(s, e.Name, "finalized block not found after first finality")
			}
			if e.Syncing != nil && s.Elapsed >= 600 {
				obs("EL "+e.Name, "syncing")
				if cov.SyncingFromS == nil {
					cov.SyncingFromS = &samples[i].Elapsed
				}
				cov.SyncingToS = &samples[i].Elapsed
				if *e.Syncing {
					syncingLate = append(syncingLate, fmt.Sprintf("%s at %s", e.Name, at))
				}
			}
		}
		maxDistinct = max(maxDistinct, len(distinct))
		var lo, hi uint64 = math.MaxUint64, 0
		for _, c := range s.CLs {
			obs("CL "+c.Name, "")
			if c.Error != "" {
				addErr(s, c.Name, c.Error)
			}
			if c.HeadSlot != nil {
				obs("CL "+c.Name, "head slot")
				lo, hi = min(lo, *c.HeadSlot), max(hi, *c.HeadSlot)
			}
			if c.FinalizedEpoch != nil {
				obs("CL "+c.Name, "finalized epoch")
				ep := *c.FinalizedEpoch
				if prev, ok := lastEpoch[c.Name]; ok && ep < prev {
					finalityFails = append(finalityFails, fmt.Sprintf("%s epoch %d→%d at %s", c.Name, prev, ep, at))
				}
				lastEpoch[c.Name] = ep
				if cur := int(s.Elapsed / epochTime.Seconds()); after && int(ep) < cur-3 {
					finalityFails = append(finalityFails, fmt.Sprintf("%s finalized epoch %d, current %d at %s", c.Name, ep, cur, at))
				}
			}
		}
		if hi >= lo {
			maxSlotSpread = max(maxSlotSpread, int(hi-lo))
		}
	}
	if first < 0 {
		finalityFails = append(finalityFails, fmt.Sprintf("never finalized: epoch 0 until +%.0fs", cov.LastS))
	}
	var gaps []string
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fields := []string{"head slot", "finalized epoch"}
		if strings.HasPrefix(name, "EL ") {
			fields = []string{"latest", "finalized"}
			if cov.SyncingFromS != nil {
				fields = append(fields, "syncing")
			}
			if first >= 0 {
				fields = append(fields, "finalized after first finality")
			}
		}
		for _, f := range fields {
			if !seen[name][f] {
				gaps = append(gaps, name[3:]+": no "+f)
			}
		}
	}
	finalityOK := "lag ≤ 3 epochs, monotone"
	if cov.FirstFinalizedS != nil {
		finalityOK = fmt.Sprintf("first finalized at +%.0fs; %s", *cov.FirstFinalizedS, finalityOK)
	}
	sub := []check{
		{ID: "E3c-distinct", Desc: "≤ 2 distinct EL heads per sample", Status: passIf(maxDistinct <= 2),
			Detail: fmt.Sprintf("max %d", maxDistinct)},
		{ID: "E3c-hash", Desc: "one hash per block number", Status: passIf(len(hashConflicts) == 0),
			Detail: firstN(hashConflicts, 3)},
		{ID: "E3c-finalized", Desc: "EL finalized hash unique per number, monotone, never lost", Status: passIf(len(finFails) == 0),
			Detail: firstN(finFails, 3)},
		{ID: "E3c-syncing", Desc: "eth_syncing false from +600 s", Status: passIf(len(syncingLate) == 0),
			Detail: firstN(syncingLate, 3)},
		{ID: "E3c-slots", Desc: "CL head slots within 1", Status: passIf(maxSlotSpread <= 1),
			Detail: fmt.Sprintf("max spread %d", maxSlotSpread)},
		{ID: "E3c-finality", Desc: "CL finality reached; then finalized epoch ≥ current − 3", Status: passIf(len(finalityFails) == 0),
			Detail: firstN(finalityFails, 3)},
		errorsCheck(errs, nodeErrs, len(samples)),
		{ID: "E3c-nodes", Desc: fmt.Sprintf("every sample has the inventory's %d ELs and %d CLs", n, n), Status: statusPass},
		{ID: "E3c-coverage", Desc: "every node answered every assessed field at least once", Status: statusPass},
	}
	if cov.SyncingFromS == nil {
		sub[3].Status, sub[3].Detail = statusMissing, "no sample at +600 s or later"
	} else if len(syncingLate) == 0 {
		sub[3].Detail = fmt.Sprintf("+%.0f…+%.0fs", *cov.SyncingFromS, *cov.SyncingToS)
	}
	if len(finalityFails) == 0 {
		sub[5].Detail = finalityOK
	}
	if len(population) > 0 {
		sub[7].Status, sub[7].Detail = statusMissing, firstN(population, 3)
	}
	if len(gaps) > 0 {
		sub[8].Status, sub[8].Detail = statusMissing, firstN(gaps, 4)
	}
	detail := fmt.Sprintf("%d samples, +%.0f…+%.0fs", len(samples), cov.FirstS, cov.LastS)
	return check{ID: "E3c", Desc: "heads, finality, sync", Status: overall(sub), Detail: detail, Sub: sub}, cov
}

// errorsCheck allows each node to miss max(1, 1 % of the samples); those errors are
// informational, more fail.
func errorsCheck(errs []string, nodeErrs map[string]int, samples int) check {
	c := check{ID: "E3c-errors", Desc: "query errors within budget"}
	budget := max(1, samples/100)
	var over []string
	for name, k := range nodeErrs {
		if k > budget {
			over = append(over, fmt.Sprintf("%s %d/%d", name, k, samples))
		}
	}
	sort.Strings(over)
	switch {
	case len(over) > 0:
		c.Status, c.Detail = statusFail, fmt.Sprintf("over budget %d: %s; %s", budget, strings.Join(over, ", "), firstN(errs, 3))
	case len(errs) > 0:
		c.Status, c.Detail = statusInfo, fmt.Sprintf("%d errors, budget %d per node: %s", len(errs), budget, firstN(errs, 3))
	default:
		c.Status = statusPass
	}
	return c
}

func firstN(s []string, n int) string {
	if len(s) <= n {
		return strings.Join(s, "; ")
	}
	return strings.Join(s[:n], "; ") + fmt.Sprintf("; … %d more", len(s)-n)
}

func printCheck(c check) {
	fmt.Print(renderTable([]check{c}))
}
