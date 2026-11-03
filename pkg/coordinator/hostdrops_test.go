package coordinator

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/datahop/topdisc-testbed/pkg/host"
)

// hostDrops follows H1's rules: the cases figures/test_checks.py runs too.
func TestHostDropsSharedCases(t *testing.T) {
	b, err := os.ReadFile("../../figures/testdata/h1-cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Cases []struct {
			Name  string
			Hosts [][]host.Sample
			ByCat orderedCounts  `json:"by_category"`
			Gaps  map[string]int `json:"coverage_gaps"`
		}
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Cases) == 0 {
		t.Fatal("no cases")
	}
	for _, c := range f.Cases {
		cats, gaps := hostDrops(c.Hosts)
		got := make(orderedCounts, len(cats))
		for i, d := range cats {
			got[i] = dropCount{What: d.What, N: d.N}
		}
		if !reflect.DeepEqual(got, c.ByCat) || !reflect.DeepEqual(gaps, c.Gaps) {
			t.Errorf("%s:\n got %v, gaps %v\nwant %v, gaps %v", c.Name, got, gaps, c.ByCat, c.Gaps)
		}
	}
}

// orderedCounts decodes a JSON object into categories in document order.
type orderedCounts []dropCount

func (o *orderedCounts) UnmarshalJSON(b []byte) error {
	d := json.NewDecoder(bytes.NewReader(b))
	if _, err := d.Token(); err != nil {
		return err
	}
	for d.More() {
		k, err := d.Token()
		if err != nil {
			return err
		}
		var n int64
		if err := d.Decode(&n); err != nil {
			return err
		}
		*o = append(*o, dropCount{What: k.(string), N: n})
	}
	return nil
}

func TestDropLine(t *testing.T) {
	ns := func(k int, q int64, sampled int) host.Sample {
		s := host.Sample{AtMs: int64(k), NetnsSampled: sampled, NetnsExpected: 500}
		if sampled > 0 {
			s.Netns = &host.NetSample{QdiscDrops: q}
		}
		return s
	}
	for _, c := range []struct {
		hosts [][]host.Sample
		want  string
	}{
		// 0 -> 100 -> unread: the 100 stands, and the gap is said
		{[][]host.Sample{{ns(0, 0, 500), ns(1, 100, 500), ns(2, 0, 0)}},
			"100 packet drops (treat timings with care): netem 100, namespace UDP receive buffer 0, host UDP receive buffer 0; not measured throughout: netns on 1 hosts"},
		{[][]host.Sample{{ns(0, 0, 0), ns(1, 0, 0)}},
			"insufficient coverage, no packet drops: netem 0, namespace UDP receive buffer 0, host UDP receive buffer 0; not measured throughout: netns on 1 hosts"},
		// a non-root WAN monitor: two namespaces expected, none entered
		{[][]host.Sample{{{NetnsExpected: 2}, {NetnsExpected: 2}}},
			"insufficient coverage, no packet drops: netem 0, namespace UDP receive buffer 0, host UDP receive buffer 0; not measured throughout: netns on 1 hosts"},
		{[][]host.Sample{{ns(0, 7, 500), ns(1, 7, 500)}},
			"no packet drops: netem 0, namespace UDP receive buffer 0, host UDP receive buffer 0"},
		// a run without the newer counters keeps the root namespace's line
		{[][]host.Sample{{{UDPRcvbufErrors: 1}, {UDPRcvbufErrors: 4}}, {{}, {}}}, "UDP receive-buffer drops: 3 on 1 hosts (host saturation; treat timings with care)"},
		{[][]host.Sample{{{}, {}}}, "no UDP receive-buffer drops"},
	} {
		if got := dropLine(hostDrops(c.hosts)); got != c.want {
			t.Errorf("got  %s\nwant %s", got, c.want)
		}
	}
}

// The trace upload's drops fall after the stop: a sample at the stop keeps
// them out of the window, which otherwise ends at the next periodic sample.
func TestHostDropsUpToStop(t *testing.T) {
	const stop = 1_800_000_000_000
	smp := func(at, bwOut int64) host.Sample {
		return host.Sample{AtMs: at, ENASample: host.ENASample{ENAIface: "ens5", BWOutAllowanceExceeded: bwOut},
			Netns: &host.NetSample{QdiscDrops: 7}, NetnsSampled: 500, NetnsExpected: 500}
	}
	periodic := []host.Sample{smp(stop-30_000, 0), smp(stop-10_000, 0), smp(stop+20_000, 500)}
	atStop := []host.Sample{smp(stop-30_000, 0), smp(stop-10_000, 0), smp(stop+5_000, 0), smp(stop+20_000, 500)}
	for _, c := range []struct {
		hosts [][]host.Sample
		stop  int64
		want  string
	}{
		{[][]host.Sample{periodic}, 0,
			"500 packet drops (treat timings with care): netem 0, namespace UDP receive buffer 0, ENA allowance 500 (bw_out 500), host UDP receive buffer 0"},
		{[][]host.Sample{periodic}, stop,
			"500 packet drops up to the stop (treat timings with care): netem 0, namespace UDP receive buffer 0, ENA allowance 500 (bw_out 500), host UDP receive buffer 0"},
		{[][]host.Sample{atStop}, stop,
			"no packet drops up to the stop: netem 0, namespace UDP receive buffer 0, ENA allowance 0, host UDP receive buffer 0; 500 after the stop (trace collection), not counted: ENA allowance 500 (bw_out 500)"},
		// a periodic sample at the stop ends the window too
		{[][]host.Sample{{smp(stop-30_000, 0), smp(stop, 0), smp(stop+20_000, 500)}}, stop,
			"no packet drops up to the stop: netem 0, namespace UDP receive buffer 0, ENA allowance 0, host UDP receive buffer 0; 500 after the stop (trace collection), not counted: ENA allowance 500 (bw_out 500)"},
		// a host whose samples all fall after the stop is a gap
		{[][]host.Sample{atStop, {smp(stop+1, 0), smp(stop+2, 9)}}, stop,
			"insufficient coverage, no packet drops up to the stop: netem 0, namespace UDP receive buffer 0, ENA allowance 0, host UDP receive buffer 0; not measured throughout: ENA on 1 hosts, netns on 1 hosts; 509 after the stop (trace collection), not counted: ENA allowance 509 (bw_out 509)"},
	} {
		if got := windowDropLine(c.hosts, c.stop); got != c.want {
			t.Errorf("got  %s\nwant %s", got, c.want)
		}
	}
}
