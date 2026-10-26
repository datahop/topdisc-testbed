package host

import (
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
)

// NetSample holds where a node namespace lost packets: its egress qdisc
// (netem), its UDP stack and its veth. Counters are cumulative since the
// namespace was created, except the qdisc backlog, which is what sits in the
// queue at the sample: at 1 Mbit/s every 125 kB of it is a second of delay.
type NetSample struct {
	QdiscDrops      int64 `json:"qdisc_drops,omitempty"`
	QdiscOverlimits int64 `json:"qdisc_overlimits,omitempty"`
	QdiscBacklog    int64 `json:"qdisc_backlog_bytes,omitempty"`
	QdiscQlen       int64 `json:"qdisc_qlen,omitempty"`
	UDPRcvbufErrors int64 `json:"ns_udp_rcvbuf_errors,omitempty"`
	UDPSndbufErrors int64 `json:"ns_udp_sndbuf_errors,omitempty"`
	UDPInErrors     int64 `json:"ns_udp_in_errors,omitempty"`
	UDPNoPorts      int64 `json:"ns_udp_no_ports,omitempty"`
	VethRxDropped   int64 `json:"veth_rx_dropped,omitempty"`
	VethTxDropped   int64 `json:"veth_tx_dropped,omitempty"`
}

func (s *NetSample) add(o NetSample) {
	s.QdiscDrops += o.QdiscDrops
	s.QdiscOverlimits += o.QdiscOverlimits
	s.QdiscBacklog += o.QdiscBacklog
	s.QdiscQlen += o.QdiscQlen
	s.UDPRcvbufErrors += o.UDPRcvbufErrors
	s.UDPSndbufErrors += o.UDPSndbufErrors
	s.UDPInErrors += o.UDPInErrors
	s.UDPNoPorts += o.UDPNoPorts
	s.VethRxDropped += o.VethRxDropped
	s.VethTxDropped += o.VethTxDropped
}

// parseSnmp returns the named row of a /proc/net/snmp file ("Udp", "Ip", ...)
// as counter name -> value.
func parseSnmp(b []byte, proto string) map[string]int64 {
	lines := strings.Split(string(b), "\n")
	for i := 0; i+1 < len(lines); i++ {
		if strings.HasPrefix(lines[i], proto+":") && strings.HasPrefix(lines[i+1], proto+":") {
			keys, vals := strings.Fields(lines[i])[1:], strings.Fields(lines[i+1])[1:]
			m := make(map[string]int64, len(keys))
			for j, k := range keys {
				if j < len(vals) {
					m[k], _ = strconv.ParseInt(vals[j], 10, 64)
				}
			}
			return m
		}
	}
	return nil
}

// parseNetDev returns the rx and tx drop counts of dev in a /proc/net/dev file.
func parseNetDev(b []byte, dev string) (rxDrop, txDrop int64, ok bool) {
	for _, ln := range strings.Split(string(b), "\n") {
		name, rest, found := strings.Cut(ln, ":")
		if !found || strings.TrimSpace(name) != dev {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 12 {
			return 0, 0, false
		}
		rxDrop, _ = strconv.ParseInt(f[3], 10, 64)
		txDrop, _ = strconv.ParseInt(f[11], 10, 64)
		return rxDrop, txDrop, true
	}
	return 0, 0, false
}

// parseNeighStat reads a /proc/net/stat/arp_cache file: one hex row per CPU,
// summed, except entries, which is the table's size and the same in each.
func parseNeighStat(b []byte) map[string]int64 {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) < 2 {
		return nil
	}
	keys := strings.Fields(lines[0])
	m := make(map[string]int64, len(keys))
	for _, ln := range lines[1:] {
		for j, f := range strings.Fields(ln) {
			if j >= len(keys) {
				break
			}
			v, _ := strconv.ParseInt(f, 16, 64)
			if keys[j] == "entries" {
				m[keys[j]] = v
			} else {
				m[keys[j]] += v
			}
		}
	}
	return m
}

// Netlink constants for the qdisc dump; syscall has them only on Linux.
const (
	nlmsgHdrLen   = 16
	tcmsgLen      = 20
	nlmsgDone     = 3
	nlmsgError    = 2
	rtmNewQdisc   = 36
	tcaStats2     = 7
	tcaStatsQueue = 3
	tcHRoot       = 0xFFFFFFFF
)

// qdiscStats is the gnet_stats_queue of one qdisc.
type qdiscStats struct {
	Ifindex                                  int32
	Qlen, Backlog, Drops, Requeues, Overlims uint32
}

// parseQdiscDump reads one buffer of an RTM_GETQDISC dump and returns the
// root qdiscs in it; done is set once NLMSG_DONE is seen.
func parseQdiscDump(b []byte) (qs []qdiscStats, done bool, err error) {
	ne := binary.NativeEndian
	for len(b) >= nlmsgHdrLen {
		l, typ := int(ne.Uint32(b)), ne.Uint16(b[4:])
		if l < nlmsgHdrLen || l > len(b) {
			return qs, done, fmt.Errorf("netlink: bad message length %d", l)
		}
		msg := b[nlmsgHdrLen:l]
		switch typ {
		case nlmsgDone:
			return qs, true, nil
		case nlmsgError:
			if len(msg) >= 4 {
				if e := int32(ne.Uint32(msg)); e != 0 {
					return qs, true, fmt.Errorf("netlink: errno %d", -e)
				}
			}
		case rtmNewQdisc:
			if len(msg) >= tcmsgLen && ne.Uint32(msg[12:]) == tcHRoot {
				q := qdiscStats{Ifindex: int32(ne.Uint32(msg[4:]))}
				if st := rtattr(msg[tcmsgLen:], tcaStats2); st != nil {
					if v := rtattr(st, tcaStatsQueue); len(v) >= 20 {
						q.Qlen, q.Backlog, q.Drops = ne.Uint32(v), ne.Uint32(v[4:]), ne.Uint32(v[8:])
						q.Requeues, q.Overlims = ne.Uint32(v[12:]), ne.Uint32(v[16:])
					}
				}
				qs = append(qs, q)
			}
		}
		if align4(l) >= len(b) {
			break
		}
		b = b[align4(l):]
	}
	return qs, done, nil
}

// rtattr returns the payload of the first attribute of type typ in b.
func rtattr(b []byte, typ uint16) []byte {
	ne := binary.NativeEndian
	for len(b) >= 4 {
		l, t := int(ne.Uint16(b)), ne.Uint16(b[2:])&0x3FFF // drop NLA_F_NESTED and byte-order flags
		if l < 4 || l > len(b) {
			return nil
		}
		if t == typ {
			return b[4:l]
		}
		if align4(l) >= len(b) {
			return nil
		}
		b = b[align4(l):]
	}
	return nil
}

func align4(n int) int { return (n + 3) &^ 3 }
