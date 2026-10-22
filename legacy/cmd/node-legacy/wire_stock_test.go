//go:build !fork

package main

// testWire is two sessions' counters and their sum: none on the stock build.
func testWire() (w1, w2, sum map[string]wireCounter) { return nil, nil, map[string]wireCounter{} }
