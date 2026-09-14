package assign

import (
	"math/rand"
	"testing"
	"time"
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

func TestZipfAlphaOne(t *testing.T) {
	z := NewZipf(1.0, 300)
	rng := rand.New(rand.NewSource(1))
	counts := make([]int, 300)
	for i := 0; i < 100000; i++ {
		counts[z.Draw(rng)]++
	}
	// P(0)/P(1) = 2, P(0)/P(9) = 10 for s = 1.
	if r := float64(counts[0]) / float64(counts[1]); r < 1.8 || r > 2.2 {
		t.Fatalf("P0/P1 = %.2f, want 2", r)
	}
	if r := float64(counts[0]) / float64(counts[9]); r < 8.5 || r > 11.5 {
		t.Fatalf("P0/P9 = %.2f, want 10", r)
	}
}

func TestStartOffsets(t *testing.T) {
	o := StartOffsets(1, 10000, 15*time.Minute)
	if o[0] != 0 {
		t.Fatalf("bootnode offset %d, want 0", o[0])
	}
	counts := make([]int, 15)
	for i, v := range o {
		if v < 0 || v > 900000 || (i > 0 && v < o[i-1]) {
			t.Fatalf("offset %d = %d out of window or unsorted", i, v)
		}
		counts[min(14, int(v/60000))]++
	}
	for m, c := range counts {
		if c < 530 || c > 800 {
			t.Fatalf("minute %d holds %d starts, want about 667: not spread evenly", m, c)
		}
	}
	if again := StartOffsets(1, 10000, 15*time.Minute); again[5000] != o[5000] {
		t.Fatal("offsets not reproducible from the seed")
	}
	if z := StartOffsets(1, 3, 0); z[2] != 0 {
		t.Fatalf("zero window: %v", z)
	}
}
