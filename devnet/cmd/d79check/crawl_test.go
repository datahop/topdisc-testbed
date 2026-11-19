package main

import (
	"testing"
	"time"
)

func TestScoreReach(t *testing.T) {
	start := time.Date(2026, 11, 18, 16, 0, 0, 400e6, time.UTC)
	reach := map[string]time.Time{
		"01": start.Truncate(time.Second),
		"02": start.Add(40 * time.Second).Truncate(time.Second),
		"03": start.Add(70 * time.Second).Truncate(time.Second),
		"04": start.Add(10 * time.Second).Truncate(time.Second),
		"ff": start.Add(5 * time.Second),
	}
	r := &reachResult{Start: start, Of: 4}
	scoreReach(r, fourNodes(), reach, 5*time.Minute)
	if r.Reached != 4 || r.Others != 1 || len(r.Missing) != 0 || !r.Pass || r.TFullS == nil || *r.TFullS != 70 {
		t.Errorf("full reach: %+v", r)
	}

	delete(reach, "03")
	r = &reachResult{Start: start, Of: 4}
	scoreReach(r, fourNodes(), reach, 5*time.Minute)
	if r.Reached != 3 || r.Pass || r.TFullS != nil || len(r.Missing) != 1 || r.Missing[0] != "el-3" {
		t.Errorf("partial reach: %+v", r)
	}
}
