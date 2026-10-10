package cronwatch_test

// A store that never answers holds neither the check nor a run for good,
// and callers sharing one check each get a result of their own.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"

	cronwatch "cronwatch.dev/go"
)

// stallingStore is a memory store whose named method waits, until its
// context ends or the gate opens, before answering.
type stallingStore struct {
	*cronwatch.MemoryStore
	mu    sync.Mutex
	stall map[string]chan struct{}
}

func newStallingStore() *stallingStore {
	return &stallingStore{MemoryStore: cronwatch.NewMemoryStore(), stall: map[string]chan struct{}{}}
}

// hold makes name wait from now on, and returns what lets it go.
func (s *stallingStore) hold(name string) (release func()) {
	gate := make(chan struct{})
	s.mu.Lock()
	s.stall[name] = gate
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		delete(s.stall, name)
		s.mu.Unlock()
		close(gate)
	}
}

func (s *stallingStore) wait(ctx context.Context, name string) error {
	s.mu.Lock()
	gate := s.stall[name]
	s.mu.Unlock()
	if gate == nil {
		return nil
	}
	select {
	case <-gate:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *stallingStore) RunningRuns(ctx context.Context) ([]cronwatch.Run, error) {
	if err := s.wait(ctx, "RunningRuns"); err != nil {
		return nil, err
	}
	return s.MemoryStore.RunningRuns(ctx)
}

func (s *stallingStore) InsertRun(ctx context.Context, run cronwatch.Run) error {
	if err := s.wait(ctx, "InsertRun"); err != nil {
		return err
	}
	return s.MemoryStore.InsertRun(ctx, run)
}

func TestAStoreThatNeverAnswersFailsTheCheckAtItsDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newStallingStore()
		cw := cronwatch.MustNew(cronwatch.WithStore(store), cronwatch.WithoutCronSecret(), cronwatch.WithErrorHandler(func(error, string) {}))
		check(t, cw.Run(bg, "nightly", ok))
		store.hold("RunningRuns")
		_, err := cw.Check(bg)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("the check failed at its deadline: %v", err)
		}
		// A later check is a new one, not the stalled one joined.
		store.mu.Lock()
		delete(store.stall, "RunningRuns")
		store.mu.Unlock()
		if _, err := cw.Check(bg); err != nil {
			t.Fatalf("a later check ran: %v", err)
		}
		check(t, cw.Close())
	})
}

func TestCloseDoesNotWaitForeverOnAStalledCheck(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newStallingStore()
		cw := cronwatch.MustNew(cronwatch.WithStore(store), cronwatch.WithoutCronSecret(), cronwatch.WithErrorHandler(func(error, string) {}))
		check(t, cw.Run(bg, "nightly", ok))
		store.hold("RunningRuns")
		go func() { _, _ = cw.Check(bg) }()
		synctest.Wait()
		check(t, cw.Close())
	})
}

func TestARunIsNotHeldForeverByAStoreThatNeverAnswers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newStallingStore()
		var mu sync.Mutex
		var reported []error
		cw := cronwatch.MustNew(cronwatch.WithStore(store), cronwatch.WithoutCronSecret(), cronwatch.WithErrorHandler(func(err error, _ string) {
			mu.Lock()
			reported = append(reported, err)
			mu.Unlock()
		}))
		release := store.hold("InsertRun")
		defer release()
		ran := false
		err := cw.Run(bg, "nightly", func(context.Context, *cronwatch.JobContext) error { ran = true; return nil })
		check(t, err)
		eq(t, "the job ran", ran, true)
		mu.Lock()
		defer mu.Unlock()
		found := false
		for _, e := range reported {
			found = found || errors.Is(e, context.DeadlineExceeded)
		}
		eq(t, "the stalled insert was reported", found, true)
	})
}

func TestCallersSharingACheckEachGetTheirOwnResult(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newStallingStore()
		cw := cronwatch.MustNew(cronwatch.WithStore(store), cronwatch.WithoutCronSecret())
		check(t, cw.Run(bg, "nightly", ok))
		release := store.hold("RunningRuns")
		results := make([]*cronwatch.CheckResult, 2)
		var wg sync.WaitGroup
		for i := range results {
			wg.Add(1)
			go func() {
				defer wg.Done()
				results[i] = must[*cronwatch.CheckResult](t)(cw.Check(bg))
			}()
		}
		synctest.Wait()
		release()
		wg.Wait()
		a, b := results[0], results[1]
		if a == b {
			t.Fatal("both callers were given the same result")
		}
		eq(t, "each saw the job", len(a.Jobs) == 1 && len(b.Jobs) == 1, true)
		a.Jobs[0].Name = "changed"
		a.Jobs[0].Open = append(a.Jobs[0].Open, cronwatch.ConditionFailed)
		eq(t, "the other's job is its own", b.Jobs[0].Name, "nightly")
		eq(t, "the other's conditions are its own", len(b.Jobs[0].Open), 0)
		check(t, cw.Close())
	})
}
