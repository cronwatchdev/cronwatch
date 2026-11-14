package bridge

import (
	"slices"
	"testing"
	"time"

	"cronwatch.dev/go/internal/schedule"
)

// The review: the check asked for CronWatch's fires one at a time, each a
// search of its own, so a cron that fires every second took seconds to
// check (4.7 for "* * * * * 1-5" in New York). Asked for in batches they
// are the same fires, around clock changes included, for a part of the
// cost.
func TestFiresAreTheFiresOneAfterAnotherAndCheaper(t *testing.T) {
	oneByOne := func(p *schedule.Parsed, start, end int64) []int64 {
		var out []int64
		for at := start; ; {
			fire, ok := schedule.FireAfter(p, at)
			if !ok || fire > end {
				return out
			}
			out = append(out, fire)
			at = fire
		}
	}
	starts := []time.Time{
		time.Date(2026, 3, 6, 0, 0, 0, 0, time.UTC),    // New York springs forward on the 8th
		time.Date(2026, 3, 27, 0, 0, 0, 0, time.UTC),   // London on the 29th
		time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC),   // Lord Howe Island, by half an hour
		time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC), // New York falls back
		time.Date(2027, 2, 27, 0, 0, 0, 0, time.UTC),
	}
	for _, zone := range []string{"America/New_York", "Europe/London", "Australia/Lord_Howe", "UTC"} {
		for _, expr := range []string{"*/45 * * * * 1-5", "30 1,2,3 * * *", "*/20 * * * *", "0 0 2 * * *", "15 2 29 2 *", "0 */5 1-3 * * *"} {
			p, err := schedule.Parse(expr, zone)
			if err != nil {
				t.Fatal(expr, err)
			}
			c := &checker{parsed: p}
			for _, at := range starts {
				start := at.UnixMilli()
				end := start + 2*changeWindowMs
				if got, want := c.fires(start, end), oneByOne(p, start, end); !slices.Equal(got, want) {
					t.Errorf("%s in %s from %s: %d fires, want %d", expr, zone, at.Format(time.DateOnly), len(got), len(want))
				}
			}
		}
	}

	// Every second for the day New York springs forward: 86,400 fires.
	p, err := schedule.Parse("* * * * * *", "America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	c := &checker{parsed: p}
	start := time.Date(2026, 3, 8, 0, 0, 0, 0, time.UTC).UnixMilli()
	end := start + dayMs
	began := time.Now()
	want := oneByOne(p, start, end)
	alone := time.Since(began)
	began = time.Now()
	got := c.fires(start, end)
	batched := time.Since(began)
	if !slices.Equal(got, want) || len(got) != 86_400 {
		t.Fatalf("%d fires, want %d", len(got), len(want))
	}
	if batched > alone/2 {
		t.Errorf("in batches took %v, one at a time %v", batched, alone)
	}
}
