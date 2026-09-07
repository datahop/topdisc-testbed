//go:build !vanilla

package main

import (
	"time"

	"github.com/marcopolo/simnet"
)

// vanillaRec is a placeholder so the single-stack build does not need a second
// copy of go-ethereum. Build with -tags vanilla for the mixed-binary workload.
type vanillaRec struct{ idx int }

func spawnMixed(*simnet.Simnet, simnet.NodeBiDiLinkSettings, int, float64, int, time.Duration, time.Duration, int64) ([]nodeRec, []vanillaRec) {
	fatalf("-vanilla-frac needs a build with -tags vanilla (see README)")
	return nil, nil
}

func runVanillaInterop(*simnet.Simnet, simnet.NodeBiDiLinkSettings, int, float64, int, float64, int64, time.Duration, time.Duration, time.Duration, time.Duration, time.Duration, time.Duration, int, time.Duration, string, searchPacing) {
	fatalf("-vanilla-frac needs a build with -tags vanilla (see README)")
}
