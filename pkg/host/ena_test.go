package host

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

// ethtool -S on an ENA interface, laid out after the counter list of the
// driver's ena_ethtool.c (global, ENI, per-queue, admin queue); one queue kept.
const enaEthtoolS = `NIC statistics:
     tx_timeout: 0
     suspend: 0
     resume: 0
     wd_expired: 0
     interface_up: 1
     interface_down: 0
     admin_q_pause: 0
     bw_in_allowance_exceeded: 12
     bw_out_allowance_exceeded: 34
     pps_allowance_exceeded: 5678
     conntrack_allowance_exceeded: 9
     linklocal_allowance_exceeded: 2
     conntrack_allowance_available: 1966080
     queue_0_tx_cnt: 731402946
     queue_0_tx_bytes: 194281034813
     queue_0_tx_queue_stop: 0
     queue_0_tx_queue_wakeup: 0
     queue_0_tx_dma_mapping_err: 0
     queue_0_rx_cnt: 812094112
     queue_0_rx_bytes: 233917448276
     queue_0_rx_rx_copybreak_pkt: 799214775
     queue_0_rx_page_alloc_fail: 0
     ena_admin_q_aborted_cmd: 0
     ena_admin_q_submitted_cmd: 61
     ena_admin_q_completed_cmd: 61
     ena_admin_q_out_of_space: 0
     ena_admin_q_no_completion: 0
`

// parseEthtoolS reads `ethtool -S` output into parallel name and value lists.
func parseEthtoolS(t *testing.T, out string) ([]string, []uint64) {
	t.Helper()
	var names []string
	var vals []uint64
	for _, ln := range strings.Split(out, "\n")[1:] {
		k, v, ok := strings.Cut(strings.TrimSpace(ln), ": ")
		if !ok {
			continue
		}
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			t.Fatalf("%q: %v", ln, err)
		}
		names, vals = append(names, k), append(vals, n)
	}
	return names, vals
}

func TestENAFromStats(t *testing.T) {
	names, vals := parseEthtoolS(t, enaEthtoolS)
	// through the GSTRINGS table the ioctl returns
	tab := make([]byte, 32*len(names))
	for i, n := range names {
		copy(tab[32*i:], n)
	}
	got := enaFromStats("ens5", parseGStrings(tab, len(names)), vals)
	want := ENASample{ENAIface: "ens5", PPSAllowanceExceeded: 5678, BWInAllowanceExceeded: 12, BWOutAllowanceExceeded: 34,
		ConntrackAllowanceExceeded: 9, LinklocalAllowanceExceeded: 2}
	if got != want {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	// a device without ENI stats reports no allowance counters: not measured, not zero
	if got := enaFromStats("ens5", names[:7], vals[:7]); got != (ENASample{}) {
		t.Fatalf("without ENI stats: got %+v", got)
	}
}

// A table without allowance counters is an error to retry, not a reading.
func TestENASampleNeedsAllowance(t *testing.T) {
	names, vals := parseEthtoolS(t, enaEthtoolS)
	if s, err := enaSample("ens5", names, vals); err != nil || s.PPSAllowanceExceeded != 5678 {
		t.Fatalf("full table: %+v, %v", s, err)
	}
	s, err := enaSample("ens5", names[:7], vals[:7])
	if !errors.Is(err, errNoAllowance) || s.ENAIface != "" {
		t.Fatalf("no allowance counters: %+v, %v", s, err)
	}
	// so the sample says ENA was not measured
	m := &monitor{ena: enaFunc(func() (ENASample, error) { return enaSample("ens5", names[:7], vals[:7]) })}
	var smp Sample
	m.sampleENA(&smp)
	if smp.ENAError != "ens5: allowance counters unavailable" || smp.ENAIface != "" || m.ena != nil || m.enaOff {
		t.Fatalf("sample %+v, reader %v, off %v", smp, m.ena, m.enaOff)
	}
}

type enaFunc func() (ENASample, error)

func (f enaFunc) ena() (ENASample, error) { return f() }
func (f enaFunc) Close()                  {}

func TestDefaultRouteIface(t *testing.T) {
	b := []byte("Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
		"tbbr0\t00006A0A\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n" +
		"ens6\t00000000\t0101A8C0\t0003\t0\t0\t200\t00000000\t0\t0\t0\n" +
		"ens5\t00000000\t0100A8C0\t0003\t0\t0\t100\t00000000\t0\t0\t0\n" +
		"ens7\t00000000\t0102A8C0\t0002\t0\t0\t0\t00000000\t0\t0\t0\n") // not up
	if got := defaultRouteIface(b); got != "ens5" {
		t.Fatalf("got %q, want ens5", got)
	}
	if got := defaultRouteIface(b[:strings.Index(string(b), "ens6")]); got != "" {
		t.Fatalf("no default route: got %q", got)
	}
}

type fakeENA struct {
	v      ENASample
	err    error
	closed *int
}

func (f fakeENA) ena() (ENASample, error) { return f.v, f.err }
func (f fakeENA) Close()                  { *f.closed++ }

// Only a host without an ENA device stops trying; other failures are noted in
// the sample and retried.
func TestSampleENARetry(t *testing.T) {
	ok := ENASample{ENAIface: "ens5", PPSAllowanceExceeded: 4}
	steps := []struct {
		open      error
		read      error
		wantErr   bool
		wantIface string
	}{
		{open: errors.New("no default route"), wantErr: true},
		{read: errors.New("ens5: 0 stats, expected 61"), wantErr: true}, // opened, read fails: reopened next time
		{wantIface: "ens5"},
		{wantIface: "ens5"},
	}
	var opens, closes int
	i := 0
	defer func(f func() (enaReader, error)) { openENA = f }(openENA)
	openENA = func() (enaReader, error) {
		opens++
		if err := steps[i].open; err != nil {
			return nil, err
		}
		return fakeENA{ok, steps[i].read, &closes}, nil
	}
	m := &monitor{}
	for i = range steps {
		var s Sample
		m.sampleENA(&s)
		if (s.ENAError != "") != steps[i].wantErr || s.ENAIface != steps[i].wantIface || m.enaOff {
			t.Fatalf("step %d: %+v, off %v", i, s, m.enaOff)
		}
	}
	if opens != 3 || closes != 1 {
		t.Fatalf("opened %d times, closed %d, want 3 (after each failure, then kept) and 1 (the reader that failed)", opens, closes)
	}
	m.closeENA()
	if closes != 2 || m.ena != nil {
		t.Fatalf("closeENA: %d closes, reader %v", closes, m.ena)
	}
	// a host with no ENA device stops for good, and its samples say nothing
	openENA = func() (enaReader, error) { opens++; return nil, errNotENA }
	m, opens = &monitor{}, 0
	for range 3 {
		var s Sample
		m.sampleENA(&s)
		if s.ENAError != "" || s.ENAIface != "" {
			t.Fatalf("non-ENA host: %+v", s)
		}
	}
	if !m.enaOff || opens != 1 {
		t.Fatalf("non-ENA host: off %v after %d opens", m.enaOff, opens)
	}
}
