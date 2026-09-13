package wan

import (
	"encoding/json"
	"fmt"
	"os"
)

// RTTTable is the measured round-trip time between regions, ms, symmetric.
// Intra is the RTT between two nodes of the same region.
type RTTTable struct {
	Intra float64                       `json:"intra_ms"`
	RTT   map[string]map[string]float64 `json:"rtt_ms"`
}

func LoadRTTTable(path string) (*RTTTable, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var t RTTTable
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &t, nil
}

// OneWay is half the RTT between two regions; an error names a missing pair.
func (t *RTTTable) OneWay(a, b string) (float64, error) {
	if a == b {
		return t.Intra / 2, nil
	}
	if r, ok := t.RTT[a][b]; ok {
		return r / 2, nil
	}
	if r, ok := t.RTT[b][a]; ok {
		return r / 2, nil
	}
	return 0, fmt.Errorf("no RTT between %s and %s", a, b)
}

// LatencyMatrix gives the one-way delay in ms from every node to every other,
// by the region each node sits in; the diagonal is zero. This is the format of
// Distem's peers_matrix_latencies.
func (t *RTTTable) LatencyMatrix(regions []string) ([][]float64, error) {
	m := make([][]float64, len(regions))
	for i := range regions {
		m[i] = make([]float64, len(regions))
		for j := range regions {
			if i == j {
				continue
			}
			d, err := t.OneWay(regions[i], regions[j])
			if err != nil {
				return nil, err
			}
			m[i][j] = d
		}
	}
	return m, nil
}
