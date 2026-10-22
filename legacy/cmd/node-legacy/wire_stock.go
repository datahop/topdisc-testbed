//go:build !fork

package main

import "github.com/ethereum/go-ethereum/p2p"

// forkBuild is false for the stock build: upstream geth counts no wire
// traffic per node, so the trace's wire record stays empty.
const forkBuild = false

type wireCounter struct{}

func addCounter(wireCounter, wireCounter) wireCounter { return wireCounter{} }

func enableWireStats() {}

func wireStats(*p2p.Server) map[string]wireCounter { return nil }

// findnodesSent is nil when the build cannot count FINDNODE requests.
func findnodesSent(*p2p.Server) func() int64 { return nil }
