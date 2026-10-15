package cronwatch_test

//lint:file-ignore SA1019 storetest's helpers are this module's own test kit, deprecated only for apps

// outbox.test.ts's claimed retries and pending runs: a queued alert is
// retried by one process only, and a finish whose evaluation was lost is
// evaluated by a later check.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	cronwatch "cronwatch.dev/go"
	"cronwatch.dev/go/storetest"
)

// flaky is a channel that fails while down and keeps what it sent.
type flaky struct {
	down atomic.Bool
	mu   sync.Mutex
	sent []string
}

func (f *flaky) Name() string { return "flaky" }

func (f *flaky) Send(_ context.Context, alert cronwatch.Alert, _ cronwatch.ChannelContext) error {
	if f.down.Load() {
		return errors.New("down")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, string(alert.Type))
	return nil
}

func (f *flaky) types() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.sent...)
}

func TestTwoProcessesCheckingAtOnceRetryAQueuedAlertOnce(t *testing.T) {
	clock := storetest.NewClock(T0)
	shared := cronwatch.NewMemoryStore()
	channel := &flaky{}
	channel.down.Store(true)
	make := func() *cronwatch.Client {
		return cronwatch.MustNew(cronwatch.WithStore(shared), cronwatch.WithClock(clock.Now), cronwatch.WithAlerts(channel),
			cronwatch.WithoutCronSecret(), cronwatch.WithErrorHandler(quiet))
	}
	a, b := make(), make()
	if err := a.Run(bg, "nightly", fails("disk full")); err == nil {
		t.Fatal("the run did not fail")
	}
	sameList(t, "queued", alertTypes(must[*cronwatch.JobState](t)(shared.GetState(bg, "nightly")).Undelivered), []string{"failed"})
	channel.down.Store(false)
	clock.Advance(MIN)
	var wg sync.WaitGroup
	for _, cw := range []*cronwatch.Client{a, b} {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = cw.Check(bg) }()
	}
	wg.Wait()
	sameList(t, "sent once", channel.types(), []string{"failed"})
	state := must[*cronwatch.JobState](t)(shared.GetState(bg, "nightly"))
	eq(t, "queue empty", len(state.Undelivered), 0)
	eq(t, "nothing sending", state.Sending == nil, true)
}

func TestWhileSilencedARetryClaimsOnlyRecoveries(t *testing.T) {
	clock := storetest.NewClock(T0)
	shared := cronwatch.NewMemoryStore()
	channel := &flaky{}
	channel.down.Store(true)
	cw := cronwatch.MustNew(cronwatch.WithStore(shared), cronwatch.WithClock(clock.Now), cronwatch.WithAlerts(channel),
		cronwatch.WithoutCronSecret(), cronwatch.WithErrorHandler(quiet))
	if err := cw.Run(bg, "nightly", fails("disk full")); err == nil {
		t.Fatal("the run did not fail")
	}
	must[cronwatch.JobState](t)(cw.Silence(bg, "nightly", hour))
	channel.down.Store(false)
	clock.Advance(MIN)
	checkNow(t, cw)
	eq(t, "nothing sent", len(channel.types()), 0)
	sameList(t, "still queued", alertTypes(must[*cronwatch.JobState](t)(shared.GetState(bg, "nightly")).Undelivered), []string{"failed"})
	must[cronwatch.JobState](t)(cw.Unsilence(bg, "nightly"))
	checkNow(t, cw)
	sameList(t, "sent after", channel.types(), []string{"failed"})
}

func TestAFinishWhoseEvaluationWasLostIsEvaluatedByALaterCheckOnce(t *testing.T) {
	clock := storetest.NewClock(T0)
	shared := cronwatch.NewMemoryStore()
	store := newTestStore()
	store.inner = shared
	// The finish's state write keeps losing, as if the process stopped
	// between the run's write and its evaluation.
	var refuse atomic.Bool
	store.cas = func(s cronwatch.JobState, version int64) (bool, error) {
		if refuse.Load() {
			return false, nil
		}
		return shared.CompareAndSetState(bg, s, version)
	}
	sent := &storetest.Capture{}
	cw := cronwatch.MustNew(cronwatch.WithStore(store), cronwatch.WithClock(clock.Now), cronwatch.WithAlerts(sent),
		cronwatch.WithoutCronSecret(), cronwatch.WithErrorHandler(quiet))
	job := cw.MustJob("nightly")
	handle := must[*cronwatch.RunHandle](t)(job.Start(bg))
	refuse.Store(true)
	handle.Fail(bg, errors.New("disk full"))
	refuse.Store(false)
	eq(t, "stored failed", must[*cronwatch.Run](t)(shared.GetRun(bg, handle.ID())).Status, cronwatch.StatusFailed)
	eq(t, "nothing sent", len(sent.List()), 0)
	sameList(t, "pending", must[*cronwatch.JobState](t)(shared.GetState(bg, "nightly")).PendingRuns, []string{handle.ID()})
	// Left alone at first, in case its finish is still being written elsewhere.
	clock.Advance(MIN)
	checkNow(t, cw)
	eq(t, "still nothing", len(sent.List()), 0)
	clock.Advance(sendLease)
	checkNow(t, cw)
	sameList(t, "evaluated", alertTypes(sent.List()), []string{"failed"})
	eq(t, "released", must[*cronwatch.JobState](t)(shared.GetState(bg, "nightly")).PendingRuns == nil, true)
	checkNow(t, cw)
	sameList(t, "once", alertTypes(sent.List()), []string{"failed"})
}

func TestAPendingRunGoneIsLetGoAndOneStoredUnjudgedIsNotEvaluated(t *testing.T) {
	clock := storetest.NewClock(T0)
	shared := cronwatch.NewMemoryStore()
	sent := &storetest.Capture{}
	cw := cronwatch.MustNew(cronwatch.WithStore(shared), cronwatch.WithClock(clock.Now), cronwatch.WithAlerts(sent), cronwatch.WithoutCronSecret())
	cw.MustJob("nightly")
	var seeded cronwatch.JobState
	check(t, seeded.UnmarshalJSON([]byte(`{"job":"nightly","open":{},"consecutiveFailures":0,"silencedUntil":null,"lastAlertAt":null,"pendingRuns":["gone"]}`)))
	check(t, shared.SetState(bg, seeded))
	checkNow(t, cw)
	eq(t, "let go", must[*cronwatch.JobState](t)(shared.GetState(bg, "nightly")).PendingRuns == nil, true)
	run := cronwatch.Run{ID: "imported", Job: "nightly", Status: cronwatch.StatusRunning, StartedAt: clock.Now(), Metrics: cronwatch.Metrics{}, Trigger: "x"}
	must[[]cronwatch.Alert](t)(cw.RecordRun(bg, run))
	sameList(t, "held", must[*cronwatch.JobState](t)(shared.GetState(bg, "nightly")).PendingRuns, []string{"imported"})
	finished := run
	finished.Status, finished.FinishedAt, finished.DurationMs, finished.Error = cronwatch.StatusFailed, ptr(clock.Now()), ptr(int64(0)), ptr("x")
	must[[]cronwatch.Alert](t)(cw.RecordRun(bg, finished, cronwatch.WithoutEvaluation()))
	eq(t, "released", must[*cronwatch.JobState](t)(shared.GetState(bg, "nightly")).PendingRuns == nil, true)
	clock.Advance(sendLease + MIN)
	checkNow(t, cw)
	eq(t, "not evaluated", len(sent.List()), 0)
}

func TestImportedFinishRetriesPendingMarkerFailure(t *testing.T) {
	clock := storetest.NewClock(T0)
	shared := cronwatch.NewMemoryStore()
	store := newTestStore()
	store.inner = shared
	fail := true
	store.cas = func(s cronwatch.JobState, version int64) (bool, error) {
		if fail {
			fail = false
			return false, errors.New("state unavailable")
		}
		return shared.CompareAndSetState(bg, s, version)
	}
	sent := &storetest.Capture{}
	cw := cronwatch.MustNew(cronwatch.WithStore(store), cronwatch.WithClock(clock.Now), cronwatch.WithAlerts(sent), cronwatch.WithErrorHandler(quiet))
	cw.MustJob("imported")
	run := cronwatch.Run{ID: "failed-import", Job: "imported", Status: cronwatch.StatusFailed, StartedAt: T0, FinishedAt: ptr(T0), DurationMs: ptr(int64(0)), Error: ptr("failed"), Metrics: cronwatch.Metrics{}, Trigger: "source"}
	if _, err := cw.RecordRun(bg, run); err == nil {
		t.Fatal("expected state write failure")
	}
	eq(t, "retryable", must[*cronwatch.Run](t)(shared.GetRun(bg, run.ID)).Status, cronwatch.StatusRunning)
	clock.Advance(24 * 60 * MIN)
	checkNow(t, cw)
	eq(t, "completed import is not stuck", len(sent.List()), 0)
	must[[]cronwatch.Alert](t)(cw.RecordRun(bg, run))
	must[[]cronwatch.Alert](t)(cw.RecordRun(bg, run))
	clock.Advance(sendLease + MIN)
	checkNow(t, cw)
	sameList(t, "evaluated once", alertTypes(sent.List()), []string{"failed"})
	eq(t, "released", must[*cronwatch.JobState](t)(shared.GetState(bg, run.Job)).PendingRuns == nil, true)
	cw.MustJob("history")
	run.ID, run.Job = "history", "history"
	must[[]cronwatch.Alert](t)(cw.RecordRun(bg, run, cronwatch.WithoutEvaluation()))
	must[[]cronwatch.Alert](t)(cw.RecordRun(bg, run))
	clock.Advance(sendLease + MIN)
	checkNow(t, cw)
	sameList(t, "history stays quiet", alertTypes(sent.List()), []string{"failed"})
}
