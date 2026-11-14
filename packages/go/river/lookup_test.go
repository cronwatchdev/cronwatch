package river_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cronwatch "cronwatch.dev/go"
	cwriver "cronwatch.dev/go/river"
	"github.com/riverqueue/river/rivertype"
)

// hungStore is a memory store whose GetJob, while hung, answers only when
// its context ends, as a connection half open does.
type hungStore struct {
	*cronwatch.MemoryStore
	hung  atomic.Bool
	reads atomic.Int64
}

func (s *hungStore) GetJob(ctx context.Context, name string) (*cronwatch.StoredJob, error) {
	s.reads.Add(1)
	if s.hung.Load() {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return s.MemoryStore.GetJob(ctx, name)
}

// The review: a worker that makes no periodic jobs read the store for a
// job's definition with the job's own context, so while the store did not
// answer the attempt waited out its whole timeout and then ran with none
// left. The read has a short deadline of its own, attempts of one job share
// it, and the work gets the job's context as River gave it.
func TestAStoreThatHangsCostsAnAttemptAMomentNotItsTimeout(t *testing.T) {
	t.Setenv("CRONWATCH_APP_ID", "billing")
	store := &hungStore{MemoryStore: cronwatch.NewMemoryStore()}
	working, err := cronwatch.New(cronwatch.WithStore(store), cronwatch.WithErrorHandler(func(error, string) {}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := working.Check(context.Background()); err != nil { // the store is ready before it hangs
		t.Fatal(err)
	}
	middleware := cwriver.New(working, cwriver.Options{}).Middleware()
	store.hung.Store(true)
	store.reads.Store(0)

	var mu sync.Mutex
	var left []time.Duration
	var ended int
	began := time.Now()
	var wg sync.WaitGroup
	for i := range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			row := &rivertype.JobRow{ID: int64(i), Kind: "report", Attempt: 1, Metadata: []byte(`{"cronwatch":"nightly-report"}`)}
			_ = middleware.Work(ctx, row, func(ctx context.Context) error {
				deadline, _ := ctx.Deadline()
				mu.Lock()
				left = append(left, time.Until(deadline))
				if ctx.Err() != nil {
					ended++
				}
				mu.Unlock()
				return nil
			})
		}()
	}
	wg.Wait()
	if took := time.Since(began); took > 1500*time.Millisecond {
		t.Errorf("ten attempts took %v", took)
	}
	eq(t, "worked", len(left), 10)
	eq(t, "begun with an ended context", ended, 0)
	for _, l := range left {
		if l < 500*time.Millisecond {
			t.Errorf("the work began with %v left of two seconds", l)
		}
	}
	if reads := store.reads.Load(); reads > 1 {
		t.Errorf("the store was read %d times for one job", reads)
	}
}

// The review: the middleware ran the work inside job.Run, which ended its
// context at the job's timeout, an hour unless set, so a worker whose
// Timeout is three hours was cancelled after one. The deadline River gave
// the job is left as it is when it is the later one; a job River gives no
// deadline (a Timeout of -1) gets the job's timeout.
func TestAJobsOwnLaterDeadlineIsNotCutToTheJobsTimeout(t *testing.T) {
	t.Setenv("CRONWATCH_APP_ID", "billing")
	working, err := cronwatch.New()
	if err != nil {
		t.Fatal(err)
	}
	middleware := cwriver.New(working, cwriver.Options{Kinds: map[string][]cronwatch.JobOption{"report": nil}}).Middleware()
	row := &rivertype.JobRow{ID: 1, Kind: "report", Attempt: 1, Metadata: []byte(`{}`)}
	var got time.Time
	var has bool
	work := func(ctx context.Context) error {
		got, has = ctx.Deadline()
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Hour)
	defer cancel()
	own, _ := ctx.Deadline()
	if err := middleware.Work(ctx, row, work); err != nil {
		t.Fatal(err)
	}
	eq(t, "River's deadline is kept", has && got.Equal(own), true)

	if err := middleware.Work(context.Background(), row, work); err != nil {
		t.Fatal(err)
	}
	if left := time.Until(got); !has || left < 59*time.Minute || left > time.Hour {
		t.Errorf("a job with no deadline got %v", left)
	}
	runs, _ := working.Runs(context.Background(), "report", 5)
	eq(t, "both recorded", len(runs), 2)
}
