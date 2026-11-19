package main

import (
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/discover"
	"github.com/ethereum/go-ethereum/p2p/discover/topicindex"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

func testTopic() topicindex.TopicID {
	return eth.DiscoveryTopic(big.NewInt(3151908), common.HexToHash("0x01"))
}

// startLoopback starts a TopDisc node, or a probe without td when probe is set.
func startLoopback(t *testing.T, probe bool, bootnodes ...*enode.Node) *discover.UDPv5 {
	t.Helper()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	disc, err := listenV5(net.IP{127, 0, 0, 1}, 0, key, bootnodes, log.NewLogger(log.DiscardHandler()), probe)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(disc.Close)
	return disc
}

// TestTopicSearchLoopback registers the eth topic from two fork nodes, which are each
// other's registrar, and searches it from a probe seeded with one of them. The probe
// has no td key, so neither node registers with it.
func TestTopicSearchLoopback(t *testing.T) {
	topic := testTopic()
	a := startLoopback(t, false)
	b := startLoopback(t, false, a.Self())
	a.RegisterTopic(topic, 0)
	b.RegisterTopic(topic, 0)
	// A search asks each registrar once, so the ads must be in place before it starts,
	// as they are in a running devnet.
	waitForAd(t, a, b, topic)
	waitForAd(t, b, a, topic)
	probe := startLoopback(t, true, a.Self())
	if topicindex.SupportsTopicDiscovery(probe.Self()) {
		t.Fatal("probe record has td")
	}

	inv := shortInventory(
		node{Name: "a", Kind: "new", ID: a.Self().ID().String()},
		node{Name: "b", Kind: "new", ID: b.Self().ID().String()},
		node{Name: "old", Kind: "old", ID: "0101"},
	)
	start := time.Now()
	hits := searchTopic(probe, topic, inv, 10*time.Second)
	if d := time.Since(start); d < 10*time.Second {
		t.Errorf("search ended after %v, before its deadline", d)
	}
	r := &topicResult{}
	scoreTopic(r, inv, hits, 10*time.Second, 10*time.Second)
	if !r.Pass || r.FoundNew != 2 || len(r.Old) != 0 {
		t.Fatalf("%+v", r)
	}
	for _, n := range []*discover.UDPv5{a, b} {
		if !topicindex.SupportsTopicDiscovery(n.Self()) {
			t.Fatal("registrant lost td")
		}
	}
	if ads := probe.LocalTopicNodes(topic); len(ads) != 0 {
		t.Errorf("probe holds %d ads", len(ads))
	}
	// a learned the probe from its first packets on: the record it holds has no td.
	var seen *enode.Node
	for _, n := range a.AllNodes() {
		if n.ID() == probe.Self().ID() {
			seen = n
		}
	}
	if seen == nil {
		t.Fatal("a never learned the probe")
	}
	if topicindex.SupportsTopicDiscovery(seen) {
		t.Errorf("a holds the probe's record with td (seq %d)", seen.Seq())
	}
}

// TestGatedConn holds a write until release.
func TestGatedConn(t *testing.T) {
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IP{127, 0, 0, 1}})
	if err != nil {
		t.Fatal(err)
	}
	c := &gatedConn{UDPConn: udp, open: make(chan struct{})}
	defer c.Close()
	done := make(chan struct{})
	go func() {
		c.WriteToUDPAddrPort([]byte{1}, udp.LocalAddr().(*net.UDPAddr).AddrPort())
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("write passed the gate before release")
	case <-time.After(100 * time.Millisecond):
	}
	c.release()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("write still held after release")
	}
}

func waitForAd(t *testing.T, registrar, node *discover.UDPv5, topic topicindex.TopicID) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		for _, n := range registrar.LocalTopicNodes(topic) {
			if n.ID() == node.Self().ID() {
				return
			}
		}
	}
	t.Fatalf("no ad of %s at %s", node.Self().ID().TerminalString(), registrar.Self().ID().TerminalString())
}

func TestScoreTopic(t *testing.T) {
	inv := fourNodes()
	hits := []topicHit{
		{ID: "02", Kind: "new", TS: 10},
		{ID: "ff", Kind: "other", TS: 20},
		{ID: "03", Kind: "new", TS: 400},
	}
	r := &topicResult{}
	scoreTopic(r, inv, hits, 3*time.Minute, 10*time.Minute)
	if !r.Pass || r.EarlyNew != 1 || r.FoundNew != 2 || r.Others != 1 {
		t.Errorf("one late NEW node: %+v", r)
	}
	r = &topicResult{}
	scoreTopic(r, inv, append(hits[:2:2], topicHit{ID: "01", Name: "el-1", Kind: "old", TS: 30}), 3*time.Minute, 10*time.Minute)
	if r.Pass || r.PassAll || r.ZeroOld || len(r.Missing) != 1 || r.Missing[0] != "el-3" {
		t.Errorf("missing NEW and one OLD: %+v", r)
	}
}
