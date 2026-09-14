package coordinator

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/datahop/topdisc-testbed/pkg/assign"
	"github.com/ethereum/go-ethereum/crypto"
)

// Trace parity: real backends write per-node traces; this file turns them,
// with the coordinator's ground truth (assignments), into the metrics.json and
// series.json that simnet's harness writes from its global view, in the same
// shape, so figures/ runs unchanged. Field semantics follow cmd/simnet
// (report.go, search.go, coverage.go, workload_multi.go, overhead_series.go).

const seriesBuckets = 50

// rawTrace is the full per-node record (NodeTrace keeps the summary subset).
type rawTrace struct {
	Idx            int                         `json:"idx"`
	ID             string                      `json:"id"`
	Legacy         bool                        `json:"legacy"`
	Topics         []string                    `json:"topics"`
	Topic          string                      `json:"topic"`
	RegisterAtMs   int64                       `json:"register_at_ms"`
	SearchAtMs     int64                       `json:"search_at_ms"`
	Lookups        []rawLookup                 `json:"lookups"`
	AdsFirstSeenMs map[string]map[string]int64 `json:"ads_first_seen_ms"`
	AdsFinal       map[string][]string         `json:"ads_final"`
	Wait           map[string]waitStats        `json:"wait"`
	Samples        []rawSample                 `json:"samples"`
	RegBucketFull  []int64                     `json:"reg_bucket_full_ms"`
	RegCompleteMs  int64                       `json:"reg_complete_ms"`
	Wire           map[string]map[string]int64 `json:"wire"`
}

type rawLookup struct {
	StartMs   int64 `json:"start_ms"`
	LatencyMs int64 `json:"latency_ms"`
	FirstMs   int64 `json:"first_ms"`
	Results   int   `json:"results"`
	HitTarget bool  `json:"hit_target"`
	Queries   int   `json:"queries"`
	Contacted int   `json:"contacted"`
	Found     []struct {
		ID   string `json:"id"`
		AtMs int64  `json:"at_ms"`
	} `json:"found"`
}

type waitStats struct {
	Admitted   int64   `json:"admitted"`
	Quoted     int64   `json:"quoted"`
	QuotedMs   []int64 `json:"quotedMs"`
	AdmittedMs []int64 `json:"admittedMs"`
}

type rawSample struct {
	AtMs      int64                       `json:"at_ms"`
	Wire      map[string]map[string]int64 `json:"wire"`
	CacheHeld int                         `json:"cache_held"`
	CacheCap  int                         `json:"cache_cap"`
	ByTopic   map[string]int              `json:"cache_by_topic"`
}

// --- metrics.json shapes (cmd/simnet/report.go, search.go, coverage.go)

type searchResult struct {
	NodeIdx             int      `json:"nodeIdx"`
	NodeID              string   `json:"nodeId"`
	Topic               int      `json:"topic"`
	Target              int      `json:"target"`
	Found               int      `json:"found"`
	FoundRegistrant     int      `json:"foundRegistrant"`
	UniqueRegistrant    int      `json:"uniqueRegistrant"`
	ConnectedAtStart    int      `json:"connectedAtStart"`
	AlreadyConnectedReg int      `json:"alreadyConnectedReg"`
	NewRegistrant       int      `json:"newRegistrant"`
	NewFoundAtMs        []int64  `json:"newFoundAtMs"`
	FoundExtra          int      `json:"foundExtra"`
	TimeToFirstNs       int64    `json:"timeToFirstNs"`
	TimeToCompletionNs  int64    `json:"timeToCompletionNs"`
	HitTimeout          bool     `json:"hitTimeout"`
	FoundIDs            []string `json:"foundIds"`
	FoundRegistrantIDs  []string `json:"foundRegistrantIds"`
	UniqueFoundAtMs     []int64  `json:"uniqueFoundAtMs"`
	UniqueFoundIDs      []string `json:"uniqueFoundIds"`
	OutboundConns       int      `json:"outboundConns"`
	DialAttempts        int      `json:"dialAttempts"`
	DialRefused         int      `json:"dialRefused"`
	SlotsFilledAtMs     int64    `json:"slotsFilledAtMs"`
	Lookups             int      `json:"lookups"`
	LookupsHitTarget    int      `json:"lookupsHitTarget"`
	LookupLatencyMs     []int64  `json:"lookupLatencyMs"`
	LookupResults       []int    `json:"lookupResults"`
	SearchStartMs       int64    `json:"searchStartMs"`
	// Real backends only: TOPICQUERY requests and distinct nodes per lookup.
	LookupQueries   []int `json:"lookupQueries,omitempty"`
	LookupContacted []int `json:"lookupContacted,omitempty"`
}

type topicReport struct {
	Topic           int     `json:"topic"`
	NumSearchers    int     `json:"numSearchers"`
	Target          int     `json:"target"`
	FullRecall      int     `json:"fullRecall"`
	MeanRecall      float64 `json:"meanRecall"`
	MeanFoundDup    float64 `json:"meanFoundDup"`
	MeanUniqueCount float64 `json:"meanUniqueCount"`
	HitTimeout      int     `json:"hitTimeout"`
}

type findCountStats struct {
	Topic       int     `json:"topic"`
	Registrants int     `json:"registrants"`
	NeverFound  int     `json:"neverFound"`
	Min         int     `json:"min"`
	P5          int     `json:"p5"`
	P25         int     `json:"p25"`
	P50         int     `json:"p50"`
	P75         int     `json:"p75"`
	P95         int     `json:"p95"`
	Max         int     `json:"max"`
	Mean        float64 `json:"mean"`
	Counts      []int   `json:"counts"`
}

type coverage struct {
	ByRegistrant map[string]int `json:"byRegistrant"`
	ByHost       map[string]int `json:"byHost"`
}

type placeAgg struct {
	SumNs int64 `json:"sumNs"`
	Count int   `json:"count"`
}

// --- series.json shapes (cmd/simnet/overhead_series.go)

type bucketed struct {
	TxBytes []int64 `json:"txBytes"`
	RxBytes []int64 `json:"rxBytes"`
	TxMsgs  []int64 `json:"txMsgs"`
	RxMsgs  []int64 `json:"rxMsgs"`
}

type overheadSample struct {
	TSec         float64              `json:"tSec"`
	TxBytes      []int64              `json:"txBytes"`
	RxBytes      []int64              `json:"rxBytes"`
	TxMsgs       []int64              `json:"txMsgs"`
	RxMsgs       []int64              `json:"rxMsgs"`
	ByType       map[string]*bucketed `json:"byType"`
	Nodes        []int                `json:"nodes"`
	CacheHeld    int64                `json:"cacheHeld"`
	CacheCap     int64                `json:"cacheCap"`
	CacheByTopic map[string]int64     `json:"cacheByTopic"`
}

type waitOut struct {
	Topic      string  `json:"topic"`
	Admitted   int64   `json:"admitted"`
	Quoted     int64   `json:"quoted"`
	QuotedMs   []int64 `json:"quotedMs"`
	AdmittedMs []int64 `json:"admittedMs"`
}

func short(id string) string {
	if len(id) > 16 {
		return id[:16]
	}
	return id
}

func topicHex(name string) string { return hex.EncodeToString(crypto.Keccak256([]byte(name))) }

func topicIndex(name string) int {
	i, _ := strconv.Atoi(strings.TrimPrefix(name, "topic-"))
	return i
}

func idBucket(id string) int {
	b, err := hex.DecodeString(id)
	if err != nil || len(b) < 8 {
		return 0
	}
	k := int(float64(binary.BigEndian.Uint64(b[:8])) / float64(1<<64) * seriesBuckets)
	if k >= seriesBuckets {
		k = seriesBuckets - 1
	}
	return k
}

// writeMetrics writes metrics.json and series.json into runDir from the raw traces.
// epochMs is the registration start (assignment 0's RegisterAt): the common
// clock of registrationTimingNs, registrationStartNs and searchStartMs.
func writeMetrics(trDir, runDir string, as []assign.Assignment) error {
	traces := map[int]*rawTrace{}
	for _, a := range as {
		b, err := os.ReadFile(filepath.Join(trDir, fmt.Sprintf("node%d.json", a.Idx)))
		if err != nil {
			continue
		}
		var t rawTrace
		if json.Unmarshal(b, &t) == nil {
			traces[a.Idx] = &t
		}
	}
	if len(traces) == 0 {
		return fmt.Errorf("no traces")
	}
	epochMs := as[0].Phases.RegisterBase
	if epochMs == 0 {
		epochMs = as[0].Phases.RegisterAt
	}
	ns := func(ms int64) int64 { return (ms - epochMs) * 1e6 }

	// Ground truth: registrants per topic (TopDisc nodes register their topic).
	topicOf := map[int]string{}
	registrants := map[string]map[string]bool{} // topic hex -> 64-hex id
	topicIds := map[string]int{}
	idOf := map[int]string{}
	for _, a := range as {
		name := a.Topics[0]
		topicOf[a.Idx] = name
		th := topicHex(name)
		topicIds[th] = topicIndex(name)
		if t, ok := traces[a.Idx]; ok {
			idOf[a.Idx] = t.ID
			if !a.Legacy {
				if registrants[th] == nil {
					registrants[th] = map[string]bool{}
				}
				registrants[th][t.ID] = true
			}
		}
	}

	// results[]: one per TopDisc searcher, its lookups merged.
	var results []searchResult
	for _, a := range as {
		t, ok := traces[a.Idx]
		if !ok || t.Legacy {
			continue
		}
		th := topicHex(topicOf[a.Idx])
		target := len(registrants[th])
		if registrants[th][t.ID] {
			target--
		}
		r := searchResult{NodeIdx: a.Idx, NodeID: short(t.ID), Topic: topicIds[th], Target: target,
			SearchStartMs: t.SearchAtMs - epochMs, TimeToFirstNs: 0,
			NewFoundAtMs: []int64{}, FoundRegistrantIDs: []string{}, UniqueFoundAtMs: []int64{}, UniqueFoundIDs: []string{},
			LookupLatencyMs: []int64{}, LookupResults: []int{}}
		seen := map[string]bool{}
		var first, last int64 = -1, 0
		for _, l := range t.Lookups {
			r.Lookups++
			if l.HitTarget {
				r.LookupsHitTarget++
			} else {
				r.HitTimeout = true
			}
			r.LookupLatencyMs = append(r.LookupLatencyMs, l.LatencyMs)
			r.LookupResults = append(r.LookupResults, l.Results)
			if l.Queries >= 0 {
				r.LookupQueries = append(r.LookupQueries, l.Queries)
				r.LookupContacted = append(r.LookupContacted, l.Contacted)
			}
			r.Found += l.Results
			end := l.StartMs + l.LatencyMs - t.SearchAtMs
			if end > last {
				last = end
			}
			for _, f := range l.Found {
				at := l.StartMs + f.AtMs - t.SearchAtMs
				r.FoundRegistrant++
				if first < 0 || at < first {
					first = at
				}
				if seen[f.ID] {
					continue
				}
				seen[f.ID] = true
				r.UniqueFoundIDs = append(r.UniqueFoundIDs, short(f.ID))
				r.UniqueFoundAtMs = append(r.UniqueFoundAtMs, at)
				r.NewFoundAtMs = append(r.NewFoundAtMs, at)
			}
		}
		r.UniqueRegistrant, r.NewRegistrant = len(seen), len(seen)
		r.FoundRegistrantIDs = append(r.FoundRegistrantIDs, r.UniqueFoundIDs...)
		if first >= 0 {
			r.TimeToFirstNs = first * 1e6
		}
		r.TimeToCompletionNs = last * 1e6
		results = append(results, r)
	}
	sort.Slice(results, func(i, j int) bool { return results[i].NodeIdx < results[j].NodeIdx })

	// perTopic and findCountByTopic from results.
	byTopic := map[int][]searchResult{}
	for _, r := range results {
		byTopic[r.Topic] = append(byTopic[r.Topic], r)
	}
	hexOf := map[int]string{}
	for th, k := range topicIds {
		hexOf[k] = th
	}
	topics := make([]int, 0, len(byTopic))
	for k := range byTopic {
		topics = append(topics, k)
	}
	sort.Ints(topics)
	perTopic := []topicReport{}
	findCounts := []findCountStats{}
	for _, k := range topics {
		rs := byTopic[k]
		rep := topicReport{Topic: k, NumSearchers: len(rs), Target: rs[0].Target}
		var sumRecall, sumUnique float64
		n := 0
		for _, r := range rs {
			if r.Target > 0 {
				n++
				sumRecall += float64(r.UniqueRegistrant) / float64(r.Target)
				sumUnique += float64(r.UniqueRegistrant)
				if r.UniqueRegistrant >= r.Target {
					rep.FullRecall++
				}
			}
			if r.HitTimeout {
				rep.HitTimeout++
			}
		}
		if n > 0 {
			rep.MeanRecall, rep.MeanFoundDup, rep.MeanUniqueCount = sumRecall/float64(n), sumRecall/float64(n), sumUnique/float64(n)
		}
		perTopic = append(perTopic, rep)
		counts := map[string]int{}
		for id := range registrants[hexOf[k]] {
			counts[short(id)] = 0
		}
		for _, r := range rs {
			for _, id := range r.UniqueFoundIDs {
				if _, ok := counts[id]; ok {
					counts[id]++
				}
			}
		}
		vals := make([]int, 0, len(counts))
		never, sum := 0, 0
		for _, c := range counts {
			vals = append(vals, c)
			sum += c
			if c == 0 {
				never++
			}
		}
		sort.Ints(vals)
		fc := findCountStats{Topic: k, Registrants: len(vals), NeverFound: never, Counts: vals}
		if len(vals) > 0 {
			p := func(q int) int { return vals[(q*(len(vals)-1))/100] }
			fc.Min, fc.P5, fc.P25, fc.P50, fc.P75, fc.P95, fc.Max = p(0), p(5), p(25), p(50), p(75), p(95), p(100)
			fc.Mean = float64(sum) / float64(len(vals))
		}
		findCounts = append(findCounts, fc)
	}

	// Registration coverage, timing, placements from registrar snapshots.
	// Per-bucket registration completion (advertiser side), ns since regStart.
	bucketFull := map[string]map[string][]int64{}
	complete := map[string]map[string]int64{}
	for _, a := range as {
		t, ok := traces[a.Idx]
		if !ok || a.Legacy {
			continue
		}
		th := topicHex(topicOf[a.Idx])
		if bucketFull[th] == nil {
			bucketFull[th], complete[th] = map[string][]int64{}, map[string]int64{}
		}
		if len(t.RegBucketFull) > 0 {
			v := make([]int64, len(t.RegBucketFull))
			for i, ms := range t.RegBucketFull {
				v[i] = -1
				if ms >= 0 {
					v[i] = ns(ms)
				}
			}
			bucketFull[th][t.ID] = v
		}
		if t.RegCompleteMs > 0 {
			complete[th][t.ID] = ns(t.RegCompleteMs)
		}
	}

	cov := map[int]coverage{}
	timing := map[string]map[string]int64{}
	placements := map[string]map[string]*placeAgg{}
	startNs := map[string]int64{}
	for th, k := range topicIds {
		cov[k] = coverage{ByRegistrant: map[string]int{}, ByHost: map[string]int{}}
		timing[th] = map[string]int64{}
		placements[th] = map[string]*placeAgg{}
	}
	for _, a := range as {
		t, ok := traces[a.Idx]
		if !ok {
			continue
		}
		if !a.Legacy {
			startNs[t.ID] = ns(a.Phases.RegisterAt)
		}
		for th, k := range topicIds {
			held := 0
			for _, id := range t.AdsFinal[th] {
				if id != t.ID && registrants[th][id] {
					held++
					cov[k].ByRegistrant[id]++
				}
			}
			cov[k].ByHost[t.ID] = held
			for id, atMs := range t.AdsFirstSeenMs[th] {
				if id == t.ID || !registrants[th][id] {
					continue
				}
				v := ns(atMs)
				if cur, ok := timing[th][id]; !ok || v < cur {
					timing[th][id] = v
				}
				pa := placements[th][id]
				if pa == nil {
					pa = &placeAgg{}
					placements[th][id] = pa
				}
				pa.SumNs += v
				pa.Count++
			}
		}
	}
	m := map[string]any{
		"perTopic": perTopic, "results": results, "registrationCoverage": map[string]any{"byTopic": cov},
		"registrationTimingNs": timing, "findCountByTopic": findCounts, "topicIds": topicIds,
		"registrationStartNs": startNs, "registrationPlacements": placements,
		"registrationBucketFullNs": bucketFull, "registrationCompleteNs": complete,
	}
	f, err := os.Create(filepath.Join(runDir, "metrics.json"))
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		f.Close()
		return err
	}
	f.Close()

	// series.json: align every node's samples on the sample period.
	period := as[0].SampleMs
	if period <= 0 {
		return nil
	}
	var t0 int64 = -1
	for _, t := range traces {
		if len(t.Samples) > 0 && (t0 < 0 || t.Samples[0].AtMs < t0) {
			t0 = t.Samples[0].AtMs
		}
	}
	if t0 < 0 {
		return nil
	}
	slots := map[int]*overheadSample{}
	newSample := func(slot int) *overheadSample {
		s := &overheadSample{TSec: float64(slot*int(period)) / 1000, TxBytes: make([]int64, seriesBuckets), RxBytes: make([]int64, seriesBuckets),
			TxMsgs: make([]int64, seriesBuckets), RxMsgs: make([]int64, seriesBuckets), ByType: map[string]*bucketed{}, Nodes: make([]int, seriesBuckets), CacheByTopic: map[string]int64{}}
		return s
	}
	for _, t := range traces {
		b := idBucket(t.ID)
		for _, smp := range t.Samples {
			slot := int((smp.AtMs - t0 + period/2) / period)
			s := slots[slot]
			if s == nil {
				s = newSample(slot)
				slots[slot] = s
			}
			s.Nodes[b]++
			s.CacheHeld += int64(smp.CacheHeld)
			s.CacheCap += int64(smp.CacheCap)
			for th, n := range smp.ByTopic {
				s.CacheByTopic[th] += int64(n)
			}
			for typ, c := range smp.Wire {
				s.TxBytes[b] += c["txBytes"]
				s.RxBytes[b] += c["rxBytes"]
				s.TxMsgs[b] += c["txMsgs"]
				s.RxMsgs[b] += c["rxMsgs"]
				bt := s.ByType[typ]
				if bt == nil {
					bt = &bucketed{make([]int64, seriesBuckets), make([]int64, seriesBuckets), make([]int64, seriesBuckets), make([]int64, seriesBuckets)}
					s.ByType[typ] = bt
				}
				bt.TxBytes[b] += c["txBytes"]
				bt.RxBytes[b] += c["rxBytes"]
				bt.TxMsgs[b] += c["txMsgs"]
				bt.RxMsgs[b] += c["rxMsgs"]
			}
		}
	}
	keys := make([]int, 0, len(slots))
	for k := range slots {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	samples := make([]overheadSample, 0, len(keys))
	for _, k := range keys {
		samples = append(samples, *slots[k])
	}
	waits := map[string]*waitOut{}
	for _, t := range traces {
		for th, w := range t.Wait {
			o := waits[th]
			if o == nil {
				o = &waitOut{Topic: th, QuotedMs: []int64{}, AdmittedMs: []int64{}}
				waits[th] = o
			}
			o.Admitted += w.Admitted
			o.Quoted += w.Quoted
			o.QuotedMs = append(o.QuotedMs, w.QuotedMs...)
			o.AdmittedMs = append(o.AdmittedMs, w.AdmittedMs...)
		}
	}
	waitList := make([]waitOut, 0, len(waits))
	for _, th := range sortedKeys(waits) {
		waitList = append(waitList, *waits[th])
	}
	out := struct {
		Buckets  int              `json:"buckets"`
		Samples  []overheadSample `json:"samples"`
		WaitTime []waitOut        `json:"waitTime"`
	}{seriesBuckets, samples, waitList}
	b, _ := json.Marshal(out)
	return os.WriteFile(filepath.Join(runDir, "series.json"), append(b, '\n'), 0o644)
}

func sortedKeys(m map[string]*waitOut) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
