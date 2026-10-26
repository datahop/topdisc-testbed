package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/datahop/topdisc-testbed/pkg/assign"
	"github.com/datahop/topdisc-testbed/pkg/host"
	"github.com/datahop/topdisc-testbed/pkg/workload"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/p2p/discover"
	"github.com/ethereum/go-ethereum/p2p/discover/topicindex"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"
)

var (
	wire1 = map[string]discover.WireCounter{"PING/v5": {TxMsgs: 1, TxBytes: 10}}
	wire2 = map[string]discover.WireCounter{"PING/v5": {TxMsgs: 2, TxBytes: 5}, "PONG/v5": {RxBytes: 1}}
	look1 = workload.Lookup{StartMs: 1100, FirstMs: 5, Results: 1, Found: []workload.Found{{ID: "p1", AtMs: 5}}, Queries: -1, Contacted: -1}
	look2 = workload.Lookup{StartMs: 3100, FirstMs: 7, Results: 1, Found: []workload.Found{{ID: "p2", AtMs: 7}}, Queries: -1, Contacted: -1}
	smp1  = sample{AtMs: 1500, Wire: wire1, ByTopic: map[string]int{}}
	smp2  = sample{AtMs: 3500, Wire: wire2, ByTopic: map[string]int{}}
)

// twoSessions builds a node of the connection model that ran one session
// from 1000 to 2000 ms (1 drop, no capable peer) and is in a second one since
// 3000 ms (2 drops, a capable peer at 50 ms), without a server: the state the
// sessions' goroutines and snapshots would have left.
func twoSessions(t *testing.T) *node {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	n := newNode(assign.Assignment{Idx: 3, Topics: []string{"t"}}, key)
	s1 := newSession(time.UnixMilli(1000))
	s1.wire, s1.lookups, s1.ops = wire1, []workload.Lookup{look1}, []map[string]any{{"msg": "TOPICQUERY/v5", "opid": 1}}
	s1.connStart, s1.connFound = time.UnixMilli(1200), []workload.Found{{ID: "p1", AtMs: 5}}
	n.cur = s1
	n.drops, n.refills = 1, []int64{40}
	n.addSample(smp1)
	n.fold(s1, time.UnixMilli(2000))

	s2 := newSession(time.UnixMilli(3000))
	s2.wire, s2.lookups, s2.ops, s2.firstCapableMs = wire2, []workload.Lookup{look2}, []map[string]any{{"msg": "TOPICQUERY/v5", "opid": 2}}, 50
	s2.connStart, s2.connFound = time.UnixMilli(3200), []workload.Found{{ID: "p2", AtMs: 7}}
	n.cur = s2
	n.drops, n.refills = n.drops+2, append(n.refills, 60)
	n.addSample(smp2)
	return n
}

func TestTraceAccumulatesSessions(t *testing.T) {
	n := twoSessions(t)
	m := n.trace()

	wire := m["wire"].(map[string]discover.WireCounter)
	if wire["PING/v5"] != (discover.WireCounter{TxMsgs: 3, TxBytes: 15}) || wire["PONG/v5"] != (discover.WireCounter{RxBytes: 1}) {
		t.Errorf("wire not summed: %v", wire)
	}
	smp := m["samples"].([]sample)
	if len(smp) != 2 || smp[0].Wire["PING/v5"].TxBytes != 10 || smp[1].Wire["PING/v5"].TxBytes != 15 || smp[1].Wire["PONG/v5"].RxBytes != 1 {
		t.Errorf("second session's sample does not carry the first's totals: %+v", smp)
	}
	if l := m["lookups"].([]workload.Lookup); len(l) != 2 || l[0].StartMs != 1100 || l[1].StartMs != 3100 {
		t.Errorf("lookups not joined in order: %+v", l)
	}
	if ops := m["ops"].([]map[string]any); len(ops) != 2 || ops[0]["opid"] != 1 || ops[1]["opid"] != 2 {
		t.Errorf("ops not joined in order: %v", ops)
	}
	if r := m["refill_ms"].([]int64); !reflect.DeepEqual(r, []int64{40, 60}) {
		t.Errorf("refill_ms %v", r)
	}
	if m["peer_drops"] != 3 || m["first_capable_ms"] != int64(50) {
		t.Errorf("peer_drops %v first_capable_ms %v, want 3 and 50", m["peer_drops"], m["first_capable_ms"])
	}
	conn := m["conn"].(map[string]any)
	if found := conn["found"].([]workload.Found); conn["start_ms"] != int64(3200) || len(found) != 2 || found[0].AtMs != 1205-3200 || found[1].AtMs != 7 {
		t.Errorf("conn %v, want both finds timed from 3200", conn)
	}
	ss := m["sessions"].([]span)
	if len(ss) != 2 || ss[0] != (span{1000, 2000}) || ss[1].StartMs != 3000 || ss[1].EndMs < time.Now().Add(-time.Minute).UnixMilli() {
		t.Errorf("sessions %v", ss)
	}
	if _, ok := m["session"]; ok {
		t.Error("trace has a session key")
	}
}

// The in-process trace of a node that churned once reads the same as the
// merge of the two session files a restarted process used to leave.
func TestTraceMatchesLoadTrace(t *testing.T) {
	n := twoSessions(t)
	n.fold(n.cur, time.UnixMilli(4000))

	dir := t.TempDir()
	path := filepath.Join(dir, "node3.json")
	file := func(p string, m map[string]any) {
		m["idx"], m["id"] = 3, n.id.String()
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	file(filepath.Join(dir, "node3.s1.json"), map[string]any{"peer_drops": 1, "first_capable_ms": -1, "wire": wire1, "samples": []sample{smp1},
		"lookups": []workload.Lookup{look1}, "conn": map[string]any{"start_ms": 1200, "found": []workload.Found{{ID: "p1", AtMs: 5}}, "slots_filled_ms": -1},
		"session": span{1000, 2000}})
	file(path, map[string]any{"peer_drops": 2, "first_capable_ms": 50, "wire": wire2, "samples": []sample{smp2},
		"lookups": []workload.Lookup{look2}, "conn": map[string]any{"start_ms": 3200, "found": []workload.Found{{ID: "p2", AtMs: 7}}, "slots_filled_ms": -1},
		"session": span{3000, 4000}})
	merged, err := host.LoadTrace(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(n.trace())
	if err != nil {
		t.Fatal(err)
	}
	var want, got map[string]any
	if err := json.Unmarshal(merged, &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"wire", "samples", "peer_drops", "first_capable_ms", "lookups", "conn", "sessions"} {
		if !reflect.DeepEqual(got[k], want[k]) {
			t.Errorf("%s: in-process %v, merged %v", k, got[k], want[k])
		}
	}
}

// A departure before the node's search start must not leave the dialer
// waiting on ready: the server's Stop would wait for it forever.
func TestDownWhileWaitingForSearch(t *testing.T) {
	it := newLazyIter(make(chan struct{}), func() enode.Iterator {
		t.Error("opened after close")
		return enode.IterNodes(nil)
	}, nil)
	res := make(chan bool, 1)
	go func() { res <- it.Next() }()
	time.Sleep(50 * time.Millisecond)
	it.Close()
	select {
	case ok := <-res:
		if ok {
			t.Error("Next = true after close")
		}
	case <-time.After(time.Second):
		t.Fatal("Next still blocked after close")
	}
	it.Close() // the server's discmix closes it again
}

// The ghost fields are the last session's snapshot; the timeout counters sum
// over sessions like the rest of wire.
func TestTraceGhosts(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	n := newNode(assign.Assignment{Idx: 1, Topics: []string{"t"}}, key)
	for i := 0; i < 2; i++ {
		k, _ := crypto.GenerateKey()
		n.ghosts = append(n.ghosts, enode.NewV4(&k.PublicKey, nil, 0, 20000+i))
	}
	s1 := newSession(time.UnixMilli(1000))
	s1.wire = map[string]discover.WireCounter{"FINDNODE/v5": {TxMsgs: 2, Timeouts: 2, GhostTimeouts: 1, OwnGhostTimeouts: 1}}
	s1.ghostsInTable, s1.tableSize = 2, 10
	n.cur = s1
	n.fold(s1, time.UnixMilli(2000))
	s2 := newSession(time.UnixMilli(3000))
	s2.wire = map[string]discover.WireCounter{"FINDNODE/v5": {TxMsgs: 1, Timeouts: 3, GhostTimeouts: 2}, "PING/v5": {Timeouts: 1}}
	s2.ghostsInTable, s2.tableSize = 2, 30
	n.cur = s2

	check := func(when string) {
		m := n.trace()
		if m["ghosts_assigned"] != 2 || m["ghosts_in_table"] != 2 || m["table_size"] != 30 {
			t.Errorf("%s: ghosts_assigned %v ghosts_in_table %v table_size %v, want 2 2 30", when, m["ghosts_assigned"], m["ghosts_in_table"], m["table_size"])
		}
		w := m["wire"].(map[string]discover.WireCounter)
		if f := w["FINDNODE/v5"]; f != (discover.WireCounter{TxMsgs: 3, Timeouts: 5, GhostTimeouts: 3, OwnGhostTimeouts: 1}) || w["PING/v5"].Timeouts != 1 {
			t.Errorf("%s: timeouts not summed: %+v", when, w)
		}
	}
	check("second session up")
	n.fold(s2, time.UnixMilli(4000))
	check("both sessions closed")
}

func capableNode(t *testing.T, entries ...enr.Entry) *enode.Node {
	key, _ := crypto.GenerateKey()
	var r enr.Record
	r.Set(topicindex.TopicDiscoveryVersion)
	for _, e := range entries {
		r.Set(e)
	}
	if err := enode.SignV4(&r, key); err != nil {
		t.Fatal(err)
	}
	nd, err := enode.New(enode.ValidSchemes, &r)
	if err != nil {
		t.Fatal(err)
	}
	return nd
}

// Ghosts and bootnodes carry "ng" but do not count as a first capable peer,
// also a ghost another node pinned and passed on.
func TestHasCapable(t *testing.T) {
	key, _ := crypto.GenerateKey()
	n := newNode(assign.Assignment{}, key)
	own, foreign, boot, peer := capableNode(t, discover.TestbedGhostMark), capableNode(t, discover.TestbedGhostMark), capableNode(t), capableNode(t)
	n.ghostIDs[own.ID()] = struct{}{}
	n.bootIDs[boot.ID()] = struct{}{}
	for _, nd := range []*enode.Node{own, foreign, boot} {
		if n.hasCapable([]*enode.Node{nd}) {
			t.Fatalf("%v counted as capable", nd.ID())
		}
	}
	if !n.hasCapable([]*enode.Node{own, foreign, boot, peer}) {
		t.Fatal("capable peer not counted")
	}
}
