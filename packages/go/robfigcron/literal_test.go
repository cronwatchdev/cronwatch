package robfigcron_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	cronwatch "cronwatch.dev/go"
	"cronwatch.dev/go/robfigcron"
	"cronwatch.dev/go/storetest"
	"github.com/robfig/cron/v3"
)

// listingStore counts the reads of every job in the store.
type listingStore struct {
	*cronwatch.MemoryStore
	lists atomic.Int64
}

func (s *listingStore) ListJobs(ctx context.Context) ([]cronwatch.StoredJob, error) {
	s.lists.Add(1)
	return s.MemoryStore.ListJobs(ctx)
}

// The review: an entry whose job is a function literal was reported with
// its entry id ("robfig/cron entry 7 is a function literal ..."), so a cron
// that adds such entries as it runs had each reported and remembered for
// good, and each followed by a sync that read every job in the store. It
// is reported once for the function, and a sync an entry starts leaves the
// store alone when the entries to declare are what the last one saw.
func TestEntriesOfOneFunctionLiteralAreReportedOnce(t *testing.T) {
	t.Setenv("CRONWATCH_APP_ID", "billing")
	store := &listingStore{MemoryStore: cronwatch.NewMemoryStore()}
	errs := &storetest.Errors{}
	cw, err := cronwatch.New(cronwatch.WithStore(store), cronwatch.WithErrorHandler(errs.Add))
	check(t, err)
	w := robfigcron.New(cw, robfigcron.Options{Logger: quiet})
	c := cron.New(w.Option(), cron.WithLocation(time.UTC))
	_, err = c.AddFunc("0 2 * * *", NightlyReport)
	check(t, err)
	c.Start()
	defer c.Stop()
	settled := func() {
		// The cron logs an entry added from its own goroutine, a moment after AddFunc returns.
		time.Sleep(20 * time.Millisecond)
		w.Wait()
	}
	settled()
	const entries = 10
	for range entries {
		_, err := c.AddFunc("0 3 * * *", func() {})
		check(t, err)
		settled()
	}
	literal := 0
	for _, e := range errs.List() {
		if !strings.Contains(e, "function literal") {
			t.Errorf("reported: %s", e)
		}
		literal++
	}
	eq(t, "reports of the function literal", literal, 1)
	if lists := store.lists.Load(); lists > 2 {
		t.Errorf("the store's jobs were read %d times for %d entries added", lists, entries)
	}

	// An entry that changes what is declared is synced to the store.
	before := store.lists.Load()
	_, err = c.AddJob("0 4 * * *", robfigcron.Named("hourly-sweep", cron.FuncJob(func() {})))
	check(t, err)
	settled()
	eq(t, "a new job is synced", store.lists.Load() > before, true)
	job, err := store.GetJob(context.Background(), "hourly-sweep")
	check(t, err)
	if job == nil || job.Definition.Schedule() != "0 4 * * *" {
		t.Errorf("stored as %+v", job)
	}
}
