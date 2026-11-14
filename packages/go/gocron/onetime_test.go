package gocron_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	cronwatch "cronwatch.dev/go"
	cwgocron "cronwatch.dev/go/gocron"
	"cronwatch.dev/go/storetest"
	"github.com/go-co-op/gocron/v2"
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

// The review: an app that makes a one-time job per reminder had each one
// reported ("gocron job <uuid> ... runs once, so it has no schedule"),
// remembered for good, and followed by a sync that read every job in the
// store. A problem is reported once for the job's name, and a sync an
// added job starts leaves the store alone when the jobs to declare are
// what the last one saw.
func TestOneTimeJobsAreReportedOnceAndDoNotReadTheStoreEach(t *testing.T) {
	t.Setenv("CRONWATCH_APP_ID", "billing")
	store := &listingStore{MemoryStore: cronwatch.NewMemoryStore()}
	errs := &storetest.Errors{}
	cw, err := cronwatch.New(cronwatch.WithStore(store), cronwatch.WithErrorHandler(errs.Add))
	check(t, err)
	w := cwgocron.New(cw, cwgocron.Options{Location: time.UTC})
	s, err := gocron.NewScheduler(w.Option(), gocron.WithLocation(time.UTC))
	check(t, err)
	defer func() { _ = s.Shutdown() }()

	_, err = s.NewJob(gocron.CronJob("0 2 * * *", false), gocron.NewTask(NightlyReport))
	check(t, err)
	w.Wait()
	const reminders = 8
	for i := range reminders {
		at := gocron.OneTimeJobStartDateTime(time.Now().Add(time.Duration(i+1) * time.Hour))
		_, err := s.NewJob(gocron.OneTimeJob(at), gocron.NewTask(func() {}), gocron.WithName("remind"))
		check(t, err)
		// And one of a function with no name to give a job.
		_, err = s.NewJob(gocron.OneTimeJob(at), gocron.NewTask(func() {}))
		check(t, err)
		w.Wait()
	}

	var once, literal int
	for _, e := range errs.List() {
		switch {
		case strings.Contains(e, "runs once, so it has no schedule"):
			once++
		case strings.Contains(e, "function literal"):
			literal++
		default:
			t.Errorf("reported: %s", e)
		}
	}
	eq(t, "reports of the one-time job", once, 1)
	eq(t, "reports of the function literal", literal, 1)
	// The cron job's sync, and the first reminder's, which changed what
	// is declared; the rest changed nothing.
	if lists := store.lists.Load(); lists > 3 {
		t.Errorf("the store's jobs were read %d times for %d jobs added", lists, 2*reminders+1)
	}

	// Sync, called by the app, still does it all.
	before := store.lists.Load()
	check(t, w.Sync(context.Background()))
	eq(t, "a whole sync reads the store", store.lists.Load() > before, true)
	job, err := store.GetJob(context.Background(), "remind")
	check(t, err)
	eq(t, "the one-time job is watched without a schedule", job != nil && job.Definition.Schedule() == "", true)
}
