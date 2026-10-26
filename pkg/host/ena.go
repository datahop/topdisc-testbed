package host

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

var (
	// errNotENA: the host has no ENA device, so there is nothing to retry.
	errNotENA = errors.New("no ENA device")
	// errNoAllowance: an ENA device whose driver or instance reports no
	// allowance counters. Retried, and noted in the sample, so the run is not
	// taken for one where no allowance was exceeded.
	errNoAllowance = errors.New("allowance counters unavailable")
)

// enaSample is enaFromStats, failing when the counters are not there.
func enaSample(iface string, names []string, vals []uint64) (ENASample, error) {
	s := enaFromStats(iface, names, vals)
	if s.ENAIface == "" {
		return s, fmt.Errorf("%s: %w", iface, errNoAllowance)
	}
	return s, nil
}

// ENASample holds the AWS ENA driver's allowance counters for the host's
// primary interface: packets the hypervisor dropped or queued because the
// instance exceeded its PPS, bandwidth, conntrack or link-local allowance.
// None of them shows in the kernel's own counters. Cumulative since the driver
// loaded; ENAIface is set only when the driver reports them, so a zero means
// none exceeded rather than not measured.
type ENASample struct {
	ENAIface                   string `json:"ena_iface,omitempty"`
	PPSAllowanceExceeded       int64  `json:"ena_pps_allowance_exceeded,omitempty"`
	BWInAllowanceExceeded      int64  `json:"ena_bw_in_allowance_exceeded,omitempty"`
	BWOutAllowanceExceeded     int64  `json:"ena_bw_out_allowance_exceeded,omitempty"`
	ConntrackAllowanceExceeded int64  `json:"ena_conntrack_allowance_exceeded,omitempty"`
	LinklocalAllowanceExceeded int64  `json:"ena_linklocal_allowance_exceeded,omitempty"`
}

// enaFromStats picks the allowance counters out of an interface's ethtool
// statistics, given as parallel name and value lists.
func enaFromStats(iface string, names []string, vals []uint64) ENASample {
	var s ENASample
	found := false
	for i, n := range names {
		if i >= len(vals) {
			break
		}
		v := int64(vals[i])
		found = found || strings.HasSuffix(n, "_allowance_exceeded")
		switch n {
		case "pps_allowance_exceeded":
			s.PPSAllowanceExceeded = v
		case "bw_in_allowance_exceeded":
			s.BWInAllowanceExceeded = v
		case "bw_out_allowance_exceeded":
			s.BWOutAllowanceExceeded = v
		case "conntrack_allowance_exceeded":
			s.ConntrackAllowanceExceeded = v
		case "linklocal_allowance_exceeded":
			s.LinklocalAllowanceExceeded = v
		}
	}
	if found {
		s.ENAIface = iface
	}
	return s
}

// defaultRouteIface returns the interface of the IPv4 default route with the
// lowest metric in a /proc/net/route file.
func defaultRouteIface(b []byte) string {
	best, metric := "", int64(-1)
	for _, ln := range strings.Split(string(b), "\n")[1:] {
		f := strings.Fields(ln)
		if len(f) < 8 || f[1] != "00000000" || f[7] != "00000000" {
			continue
		}
		flags, _ := strconv.ParseInt(f[3], 16, 64)
		m, _ := strconv.ParseInt(f[6], 10, 64)
		if flags&1 != 0 && (metric < 0 || m < metric) { // RTF_UP
			best, metric = f[0], m
		}
	}
	return best
}

// parseGStrings splits an ETHTOOL_GSTRINGS table: n names, 32 bytes each,
// NUL-padded.
func parseGStrings(b []byte, n int) []string {
	const strLen = 32
	names := make([]string, 0, n)
	for i := 0; i < n && (i+1)*strLen <= len(b); i++ {
		s := b[i*strLen : (i+1)*strLen]
		if j := bytes.IndexByte(s, 0); j >= 0 {
			s = s[:j]
		}
		names = append(names, string(s))
	}
	return names
}
