package main

import (
	"math"
	"testing"
)

// shortInventory builds an inventory with short IDs, skipping validate.
func shortInventory(ns ...node) *inventory {
	inv := &inventory{Nodes: ns, byID: map[string]int{}}
	for i, n := range ns {
		inv.byID[n.ID] = i
	}
	return inv
}

func fourNodes() *inventory {
	return shortInventory(
		node{Name: "el-1", Kind: "old", ID: "01"},
		node{Name: "el-2", Kind: "new", ID: "02"},
		node{Name: "el-3", Kind: "new", ID: "03"},
		node{Name: "el-4", Kind: "old", ID: "04"},
	)
}

func TestClassifyTables(t *testing.T) {
	ct := &crawlTables{Nodes: map[string]crawlRead{
		"01": {Buckets: map[string][]crawlEntry{"256": {{"02", true}, {"04", true}}, "255": {{"ff", true}}}},
		"02": {Buckets: map[string][]crawlEntry{"256": {{"01", true}, {"03", true}, {"04", false}}}},
		"03": {Buckets: map[string][]crawlEntry{"256": {{"02", true}}}},
	}}
	res := classifyTables(fourNodes(), ct)

	want := map[string][5]int{ // new, old, other, dead, entries
		"el-1": {1, 1, 1, 0, 3},
		"el-2": {1, 1, 0, 1, 3},
		"el-3": {1, 0, 0, 0, 1},
		"el-4": {0, 0, 0, 0, 0},
	}
	for _, n := range res.Nodes {
		got := [5]int{n.New, n.Old, n.Other, n.Dead, n.Entries}
		if got != want[n.Name] {
			t.Errorf("%s: new/old/other/dead/entries = %v, want %v", n.Name, got, want[n.Name])
		}
	}
	if k := res.Nodes[0].Table[2]; k.ID != "ff" || k.Kind != "other" || k.Bucket != 255 {
		t.Errorf("el-1 last entry = %+v, want ff other in 255", k)
	}
	if res.Nodes[0].Table[0].Kind != "new" || res.Nodes[0].Table[0].Name != "el-2" {
		t.Errorf("el-1 first entry = %+v, want el-2 new", res.Nodes[0].Table[0])
	}
	if !res.E1a.Applicable || res.E1a.Pass {
		t.Fatalf("E1(a) = %+v, want applicable and failing", res.E1a)
	}
	// el-3 holds no OLD node, el-4 was not read.
	if f := res.E1a.Failing; len(f) != 1 || f[0] != "el-3" {
		t.Errorf("E1(a) failing = %v, want [el-3]", f)
	}
	if u := res.Unread; len(u) != 1 || u[0] != "el-4" {
		t.Errorf("unread = %v, want [el-4]", u)
	}
	if p := res.Nodes[0].P; p != 2.0/3 {
		t.Errorf("OLD node p = %v, want 2/3", p)
	}
	if p := res.Nodes[1].P; p != 1.0/3 {
		t.Errorf("NEW node p = %v, want 1/3", p)
	}
}

func TestClassifyTablesControl(t *testing.T) {
	inv := shortInventory(node{Name: "el-1", Kind: "old", ID: "01"}, node{Name: "el-2", Kind: "old", ID: "02"})
	ct := &crawlTables{Nodes: map[string]crawlRead{
		"01": {Buckets: map[string][]crawlEntry{"256": {{"02", true}}}},
		"02": {Buckets: map[string][]crawlEntry{"256": {{"01", true}}}},
	}}
	res := classifyTables(inv, ct)
	if res.E1a.Applicable || res.E1a.Pass || len(res.E1b.Outside) != 0 {
		t.Errorf("control run: E1(a) %+v, E1(b) %+v, want n/a and nothing outside", res.E1a, res.E1b)
	}
}

func TestBinomialBand(t *testing.T) {
	cases := []struct {
		n      int
		p      float64
		lo, hi int
	}{
		{10, 0.5, 1, 9},
		{1, 0.5, 0, 1},
		{20, 0, 0, 0},
		{20, 1, 20, 20},
	}
	for _, c := range cases {
		if lo, hi := binomialBand(c.n, c.p, 0.99); lo != c.lo || hi != c.hi {
			t.Errorf("band(%d, %v) = [%d,%d], want [%d,%d]", c.n, c.p, lo, hi, c.lo, c.hi)
		}
	}
	// The band's tails each hold at most 0.5 %, and one more value would push them over.
	for _, n := range []int{16, 39, 100} {
		p := 20.0 / 39
		lo, hi := binomialBand(n, p, 0.99)
		below, above := 0.0, 0.0
		for k := 0; k < lo; k++ {
			below += binomialPMF(n, k, p)
		}
		for k := hi + 1; k <= n; k++ {
			above += binomialPMF(n, k, p)
		}
		if below > 0.005 || below+binomialPMF(n, lo, p) < 0.005 {
			t.Errorf("n %d: lower tail below %d = %v", n, lo, below)
		}
		if above > 0.005 || above+binomialPMF(n, hi, p) < 0.005 {
			t.Errorf("n %d: upper tail above %d = %v", n, hi, above)
		}
	}
	sum := 0.0
	for k := 0; k <= 39; k++ {
		sum += binomialPMF(39, k, 20.0/39)
	}
	if math.Abs(sum-1) > 1e-9 {
		t.Errorf("pmf sums to %v", sum)
	}
}
