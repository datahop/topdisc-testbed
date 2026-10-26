package host

import (
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/datahop/topdisc-testbed/pkg/wan"
)

const snmpFixture = `Ip: Forwarding DefaultTTL InReceives
Ip: 1 64 100
Udp: InDatagrams NoPorts InErrors OutDatagrams RcvbufErrors SndbufErrors InCsumErrors IgnoredMulti MemErrors
Udp: 228584333 1488392 5481 203791740 5470 7 0 32725 0
UdpLite: InDatagrams NoPorts InErrors OutDatagrams RcvbufErrors SndbufErrors InCsumErrors IgnoredMulti MemErrors
UdpLite: 0 0 0 0 99 0 0 0 0
`

func TestParseSnmp(t *testing.T) {
	u := parseSnmp([]byte(snmpFixture), "Udp")
	for k, want := range map[string]int64{"InErrors": 5481, "RcvbufErrors": 5470, "SndbufErrors": 7, "NoPorts": 1488392} {
		if u[k] != want {
			t.Errorf("%s = %d, want %d", k, u[k], want)
		}
	}
	if parseSnmp([]byte("Ip: a\nIp: 1\n"), "Udp") != nil {
		t.Error("no Udp row should give nil")
	}
}

func TestParseNetDev(t *testing.T) {
	b := []byte(`Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 627 98    0    0    0     0          0         0 627 98    0    0    0     0       0          0
 vtb12: 4877 501    2 55    0     1          0    296 1065 261    0    9    0     0       0          0
`)
	rx, tx, ok := parseNetDev(b, "vtb12")
	if !ok || rx != 55 || tx != 9 {
		t.Fatalf("vtb12: rx=%d tx=%d ok=%v, want 55 9 true", rx, tx, ok)
	}
	if _, _, ok := parseNetDev(b, "vtb1"); ok {
		t.Fatal("vtb1 is not in the file")
	}
}

func TestParseNeighStat(t *testing.T) {
	b := []byte(`entries  allocs   destroys hash_grows lookups  hits     res_failed rcv_probes_mcast rcv_probes_ucast periodic_gc_runs forced_gc_runs unresolved_discards table_fulls
0000000d 00000002 00000000 00000000   00000016 00000001 00000001   00000000         00000000         00000143         00000002       00000b5c            00000001
0000000d 00000000 00000000 00000000   00000003 00000000 00000003   00000000         00000000         00000404         00000001       00000004            00000000
`)
	a := parseNeighStat(b)
	for k, want := range map[string]int64{"entries": 13, "unresolved_discards": 0xb60, "table_fulls": 1, "forced_gc_runs": 3} {
		if a[k] != want {
			t.Errorf("%s = %d, want %d", k, a[k], want)
		}
	}
}

// nlmsg builds one netlink message of type typ around payload.
func nlmsg(typ uint16, payload []byte) []byte {
	ne := binary.NativeEndian
	b := make([]byte, nlmsgHdrLen, nlmsgHdrLen+len(payload)+3)
	ne.PutUint32(b, uint32(nlmsgHdrLen+len(payload)))
	ne.PutUint16(b[4:], typ)
	b = append(b, payload...)
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	return b
}

func attr(typ uint16, payload []byte) []byte {
	ne := binary.NativeEndian
	b := make([]byte, 4, 4+len(payload)+3)
	ne.PutUint16(b, uint16(4+len(payload)))
	ne.PutUint16(b[2:], typ)
	b = append(b, payload...)
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	return b
}

func qdiscMsg(ifindex int32, parent uint32, kind string, q [5]uint32) []byte {
	ne := binary.NativeEndian
	tc := make([]byte, tcmsgLen)
	ne.PutUint32(tc[4:], uint32(ifindex))
	ne.PutUint32(tc[12:], parent)
	queue := make([]byte, 20)
	for i, v := range q {
		ne.PutUint32(queue[4*i:], v)
	}
	basic := make([]byte, 16)
	stats2 := append(attr(1, basic), attr(tcaStatsQueue, queue)...)
	body := append(tc, attr(1, []byte(kind+"\x00"))...)
	body = append(body, attr(tcaStats2|0x8000, stats2)...) // NLA_F_NESTED, as newer kernels set it
	return nlmsg(rtmNewQdisc, body)
}

func TestParseQdiscDump(t *testing.T) {
	var b []byte
	b = append(b, qdiscMsg(1, tcHRoot, "noqueue", [5]uint32{})...)
	b = append(b, qdiscMsg(7, tcHRoot, "netem", [5]uint32{3, 4200, 17, 1, 5})...)
	b = append(b, qdiscMsg(7, 0xFFFFFFF1, "ingress", [5]uint32{0, 0, 99, 0, 0})...) // not a root: skipped
	qs, done, err := parseQdiscDump(b)
	if err != nil || done {
		t.Fatalf("first part: done=%v err=%v", done, err)
	}
	if len(qs) != 2 || qs[1] != (qdiscStats{Ifindex: 7, Qlen: 3, Backlog: 4200, Drops: 17, Requeues: 1, Overlims: 5}) {
		t.Fatalf("got %+v", qs)
	}
	if _, done, _ := parseQdiscDump(nlmsg(nlmsgDone, make([]byte, 4))); !done {
		t.Fatal("NLMSG_DONE not seen")
	}
	errMsg := make([]byte, 4)
	binary.NativeEndian.PutUint32(errMsg, uint32(0xFFFFFFFF)) // -EPERM
	if _, _, err := parseQdiscDump(nlmsg(nlmsgError, errMsg)); err == nil {
		t.Fatal("NLMSG_ERROR not reported")
	}
	if _, _, err := parseQdiscDump(b[:len(b)-8]); err == nil {
		t.Fatal("truncated message not reported")
	}
}

// Old readers decode the new samples, and a node without drops costs no bytes.
func TestSampleSchemaAdditive(t *testing.T) {
	s := Sample{Nodes: []NodeSample{{Idx: 1}, {Idx: 2, NetSample: NetSample{QdiscDrops: 3}}}, Netns: &NetSample{QdiscDrops: 3}, NetnsSampled: 2}
	b, _ := json.Marshal(s)
	if strings.Contains(string(b), `"idx":1,"cpu_pct":0,"rss_mb":0,"fds":0,"`) {
		t.Fatalf("zero counters serialised: %s", b)
	}
	if !strings.Contains(string(b), `"idx":2,"cpu_pct":0,"rss_mb":0,"fds":0,"qdisc_drops":3}`) {
		t.Fatalf("node counters not inline: %s", b)
	}
	var old struct {
		UDPRcvbufErrors int64 `json:"udp_rcvbuf_errors"`
		Nodes           []struct {
			Idx int `json:"idx"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(b, &old); err != nil || len(old.Nodes) != 2 {
		t.Fatalf("old reader: %v %+v", err, old)
	}
}

// A WAN runner whose namespaces cannot be read (not root, or setns fails: here
// they do not exist) still says how many it expected.
func TestSampleNetnsUnread(t *testing.T) {
	r := &Runner{Wan: &wan.Star{MinMs: 4, MaxMs: 45}}
	r.init()
	r.delay[0], r.delay[1] = time.Millisecond, time.Millisecond
	r.mon = &monitor{lastCPU: map[int]float64{}, nsWarned: true}
	r.sample()
	s := r.mon.samples[0]
	if s.NetnsExpected != 2 || s.NetnsSampled != 0 || s.Netns != nil {
		t.Fatalf("expected %d, sampled %d, netns %v", s.NetnsExpected, s.NetnsSampled, s.Netns)
	}
	b, _ := json.Marshal(s)
	if !strings.Contains(string(b), `"netns_expected":2`) {
		t.Fatalf("netns_expected not serialised: %s", b)
	}
	// without WAN there is nothing to expect
	r = &Runner{}
	r.init()
	r.mon = &monitor{lastCPU: map[int]float64{}}
	r.sample()
	if s := r.mon.samples[0]; s.NetnsExpected != 0 {
		t.Fatalf("no WAN: expected %d", s.NetnsExpected)
	}
}
