package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ethereum/go-ethereum/p2p/discover"
	"github.com/ethereum/go-ethereum/p2p/discover/topicindex"
)

// admissionDumpMaxNodes bounds admissions.json: every REGTOPIC decision of
// every registrar is too much at evaluation scale, and the checks that read
// it are functional ones run on small networks.
const admissionDumpMaxNodes = 500

// dumpNodeState writes what the correctness checks read from a per-node trace
// on the real backends: every registrar's ads with their expiry (ads.json),
// every advertiser's registration buckets (buckets.json) and, on small runs,
// every registrar's admission decisions (admissions.json).
func dumpNodeState(dir string, all []nodeRec, nodeTopics [][]int, topics []topicindex.TopicID) {
	now := time.Now().UnixMilli()
	ads := map[int]map[string]map[string]int64{}
	buckets := map[int][]topicindex.BucketStats{}
	admissions := map[int][]discover.AdmissionEvent{}
	var staleMax int64
	for i := range all {
		d := currentDisc(i)
		if d == nil {
			continue
		}
		for _, t := range topics {
			held := d.LocalTopicAds(t)
			if len(held) == 0 {
				continue
			}
			if ads[i] == nil {
				ads[i] = map[string]map[string]int64{}
			}
			m := map[string]int64{}
			for _, ad := range held {
				if ad.ExpiresIn < 0 {
					staleMax = max(staleMax, (-ad.ExpiresIn).Milliseconds())
					continue
				}
				m[ad.Node.ID().String()] = now + ad.ExpiresIn.Milliseconds()
			}
			ads[i][t.String()] = m
		}
		for _, ti := range nodeTopics[i] {
			if ti >= 0 {
				if bs := d.TopicRegistrationBuckets(topics[ti]); bs != nil {
					buckets[i] = bs
				}
			}
		}
		if len(all) <= admissionDumpMaxNodes {
			if ev := discover.AdmissionEvents(all[i].ln.ID()); len(ev) > 0 {
				admissions[i] = ev
			}
		}
	}
	write := func(name string, v any) {
		b, _ := json.Marshal(v)
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			fmt.Printf("%s not written: %v\n", name, err)
		}
	}
	write("ads.json", map[string]any{"at_ms": now, "stale_max_ms": staleMax, "nodes": ads})
	write("buckets.json", buckets)
	if len(all) <= admissionDumpMaxNodes {
		write("admissions.json", admissions)
	} else {
		fmt.Printf("admissions.json skipped: %d nodes, written up to %d\n", len(all), admissionDumpMaxNodes)
	}
}
