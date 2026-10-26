//go:build !linux || 386

package host

import "errors"

func netnsCounters(names []string, veth func(string) string) (map[string]NetSample, error) {
	return nil, errors.New("namespace counters need linux")
}
