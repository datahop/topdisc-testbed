package host

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// sessionPath is where Start parks the trace of a node's earlier session
// before it restarts the node: node<idx>.s<k>.json next to node<idx>.json.
func sessionPath(trace string, k int) string {
	return strings.TrimSuffix(trace, ".json") + fmt.Sprintf(".s%d.json", k)
}

// rotateTrace parks the trace an earlier session of the node left, so a
// churn restart does not overwrite it.
func rotateTrace(trace string) error {
	if _, err := os.Stat(trace); err != nil {
		return nil
	}
	k := 1
	for {
		if _, err := os.Stat(sessionPath(trace, k)); err != nil {
			break
		}
		k++
	}
	return os.Rename(trace, sessionPath(trace, k))
}

// LoadTrace reads a node's trace. A node that churn restarted left one file
// per session; they are merged into one trace, oldest first: counters are
// summed, event lists joined, sample counters carried on across restarts and
// sessions[] lists when each process ran.
func LoadTrace(trace string) ([]byte, error) {
	var parts [][]byte
	for k := 1; ; k++ {
		b, err := os.ReadFile(sessionPath(trace, k))
		if err != nil {
			break
		}
		parts = append(parts, b)
	}
	last, err := os.ReadFile(trace)
	if err == nil {
		parts = append(parts, last)
	} else if len(parts) == 0 {
		return nil, err
	}
	if len(parts) == 1 {
		return parts[0], nil
	}
	sessions := make([]map[string]any, 0, len(parts))
	for _, b := range parts {
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, err
		}
		sessions = append(sessions, m)
	}
	return json.Marshal(mergeTraces(sessions))
}

type counters map[string]map[string]float64

func (c counters) add(v any) {
	w, _ := v.(map[string]any)
	for typ, m := range w {
		mm, _ := m.(map[string]any)
		if c[typ] == nil {
			c[typ] = map[string]float64{}
		}
		for name, n := range mm {
			if f, ok := n.(float64); ok {
				c[typ][name] += f
			}
		}
	}
}

func (c counters) plus(v any) map[string]any {
	out := map[string]any{}
	w, _ := v.(map[string]any)
	for typ, m := range w {
		mm, _ := m.(map[string]any)
		o := map[string]any{}
		for name, n := range mm {
			f, _ := n.(float64)
			o[name] = f + c[typ][name]
		}
		out[typ] = o
	}
	return out
}

func mergeTraces(ss []map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range ss[len(ss)-1] {
		out[k] = v
	}
	var sessions []any
	lists := map[string][]any{}
	drops, capable := 0.0, -1.0
	wire := counters{}
	var conn map[string]any
	var connFound []any
	for _, s := range ss {
		if v, ok := s["session"]; ok {
			sessions = append(sessions, v)
		}
		for _, key := range []string{"lookups", "ops", "refill_ms", "admissions"} {
			if l, ok := s[key].([]any); ok {
				lists[key] = append(lists[key], l...)
			}
		}
		// A restarted process counts from zero again: carry the earlier
		// sessions' totals so the node's sample series stays cumulative.
		if l, ok := s["samples"].([]any); ok {
			for _, smp := range l {
				if m, ok := smp.(map[string]any); ok && len(wire) > 0 {
					m["wire"] = wire.plus(m["wire"])
				}
				lists["samples"] = append(lists["samples"], smp)
			}
		}
		wire.add(s["wire"])
		if d, ok := s["peer_drops"].(float64); ok {
			drops += d
		}
		if f, ok := s["first_capable_ms"].(float64); ok && f >= 0 && (capable < 0 || f < capable) {
			capable = f
		}
		if c, ok := s["conn"].(map[string]any); ok {
			start, _ := c["start_ms"].(float64)
			if found, ok := c["found"].([]any); ok {
				for _, f := range found {
					fm, _ := f.(map[string]any)
					at, _ := fm["at_ms"].(float64)
					connFound = append(connFound, map[string]any{"id": fm["id"], "at_ms": start + at})
				}
			}
			conn = c
		}
	}
	for key, l := range lists {
		out[key] = l
	}
	if _, ok := out["peer_drops"]; ok {
		out["peer_drops"] = drops
	}
	if len(wire) > 0 {
		w := map[string]any{}
		for typ, m := range wire {
			o := map[string]any{}
			for name, f := range m {
				o[name] = f
			}
			w[typ] = o
		}
		out["wire"] = w
	}
	if capable >= 0 {
		out["first_capable_ms"] = capable
	}
	if conn != nil {
		// The dialer's finds of every session, timed from the last session's start.
		c := map[string]any{}
		for k, v := range conn {
			c[k] = v
		}
		start, _ := c["start_ms"].(float64)
		for _, f := range connFound {
			fm := f.(map[string]any)
			fm["at_ms"] = fm["at_ms"].(float64) - start
		}
		c["found"] = connFound
		out["conn"] = c
	}
	delete(out, "session")
	out["sessions"] = sessions
	return out
}
