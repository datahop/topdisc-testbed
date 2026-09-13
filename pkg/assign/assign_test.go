package assign

import (
	"math/rand"
	"testing"
)

func TestLookupTimes(t *testing.T) {
	at := LookupTimes(rand.New(rand.NewSource(1)), 1000, 11000, 5)
	if len(at) != 5 {
		t.Fatalf("%d times", len(at))
	}
	for k, ms := range at {
		lo, hi := int64(1000+k*2000), int64(1000+(k+1)*2000)
		if ms < lo || ms >= hi {
			t.Fatalf("lookup %d at %d outside its interval [%d,%d)", k, ms, lo, hi)
		}
	}
}
