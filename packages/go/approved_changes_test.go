package cronwatch_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	cronwatch "cronwatch.dev/go"
)

func openOf(t *testing.T, cw *cronwatch.Client, name string) []string {
	t.Helper()
	out := []string{}
	for _, c := range summary(t, cw, name).Open {
		out = append(out, string(c))
	}
	slices.Sort(out)
	return out
}

// A run starting no longer closes stuck: a job that hangs on every run
// alerts stuck once, where each start closed it and each timeout opened
// it, and alerted, again; and it recovers on its first success.
func TestAJobThatHangsOnEveryRunAlertsStuckOnceAndRecoversOnItsFirstSuccess(t *testing.T) {
	k := newKit(t)
	job := must[*cronwatch.Job](t)(k.cw.Job("hangs", cronwatch.Schedule("*/5 * * * *"), cronwatch.Timezone("UTC"), cronwatch.Timeout("10m")))
	// Twelve runs, five minutes apart, none of which ever finishes.
	var handles []*cronwatch.RunHandle
	for range 12 {
		handles = append(handles, must[*cronwatch.RunHandle](t)(job.Start(bg)))
		k.c.Advance(5 * MIN)
		checkNow(t, k.cw)
	}
	sameList(t, "alerts", k.alerts.Types(), []string{"stuck"})
	eq(t, "health", string(summary(t, k.cw, "hangs").Health), "stuck")
	// A run that fails alerts as failed by its own rule; stuck stays open beside it.
	_ = job.Run(bg, fails("boom"))
	sameList(t, "a failure beside it", k.alerts.Types(), []string{"stuck", "failed"})
	sameList(t, "open", openOf(t, k.cw, "hangs"), []string{"failed", "stuck"})
	// A hung run that reports failing late is not counted again and alerts nothing.
	handles[0].Fail(bg, errors.New("late"))
	sameList(t, "a late failure", k.alerts.Types(), []string{"stuck", "failed"})
	// The first success closes both with one recovery.
	check(t, job.Run(bg, ok))
	sameList(t, "recovered", k.alerts.Types(), []string{"stuck", "failed", "recovered"})
	after := []string{}
	for _, c := range k.alerts.List()[2].Details.(cronwatch.RecoveredDetails).After {
		after = append(after, string(c))
	}
	slices.Sort(after)
	sameList(t, "after", after, []string{"failed", "stuck"})
	sameList(t, "nothing open", openOf(t, k.cw, "hangs"), []string{})
}

func TestAStuckRunThatFinishesOkLateRecoversThoughOtherRunsStartedMeanwhile(t *testing.T) {
	k := newKit(t)
	job := must[*cronwatch.Job](t)(k.cw.Job("slow-queue", cronwatch.Timeout("10m")))
	first := must[*cronwatch.RunHandle](t)(job.Start(bg))
	k.c.Advance(11 * MIN)
	checkNow(t, k.cw)
	sameList(t, "stuck", k.alerts.Types(), []string{"stuck"})
	second := must[*cronwatch.RunHandle](t)(job.Start(bg))
	sameList(t, "a start leaves stuck open", openOf(t, k.cw, "slow-queue"), []string{"stuck"})
	first.Finish(bg)
	sameList(t, "its late finish recovers", k.alerts.Types(), []string{"stuck", "recovered"})
	second.Finish(bg)
	sameList(t, "and nothing more", k.alerts.Types(), []string{"stuck", "recovered"})
}

// startStore is a memory store whose first InsertRun (a run's start) waits
// until released, and whose later ones fail while down is set.
type startStore struct {
	*cronwatch.MemoryStore
	mu      sync.Mutex
	inserts int
	down    bool
	gate    chan struct{}
}

func newStartStore() *startStore {
	return &startStore{MemoryStore: cronwatch.NewMemoryStore(), gate: make(chan struct{})}
}

func (s *startStore) InsertRun(ctx context.Context, run cronwatch.Run) error {
	s.mu.Lock()
	s.inserts++
	first, down := s.inserts == 1, s.down
	s.mu.Unlock()
	if first {
		<-s.gate
		return s.MemoryStore.InsertRun(ctx, run)
	}
	if down {
		return errors.New("store down")
	}
	return s.MemoryStore.InsertRun(ctx, run)
}

type startKit struct {
	cw     *cronwatch.Client
	store  *startStore
	alerts *captureTypes
	mu     sync.Mutex
	errors []string
}

// captureTypes keeps the types of the alerts sent.
type captureTypes struct {
	mu    sync.Mutex
	types []string
}

func (c *captureTypes) Name() string { return "capture" }

func (c *captureTypes) Send(_ context.Context, a cronwatch.Alert, _ cronwatch.ChannelContext) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.types = append(c.types, string(a.Type))
	return nil
}

func (c *captureTypes) list() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string{}, c.types...)
}

func newStartKit(t *testing.T) *startKit {
	t.Helper()
	k := &startKit{store: newStartStore(), alerts: &captureTypes{}}
	cw, err := cronwatch.New(cronwatch.WithStore(k.store), cronwatch.WithAlerts(k.alerts), cronwatch.WithoutCronSecret(),
		cronwatch.WithErrorHandler(func(err error, where string) {
			k.mu.Lock()
			defer k.mu.Unlock()
			k.errors = append(k.errors, where+": "+err.Error())
		}))
	check(t, err)
	k.cw = cw
	return k
}

func (k *startKit) reported() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string{}, k.errors...)
}

func (k *startKit) running(t *testing.T) int {
	t.Helper()
	return len(must[[]cronwatch.Run](t)(k.store.MemoryStore.RunningRuns(bg)))
}

const slowStart = "recording nightly: the store took longer than 5s to record the start of nightly; running it unrecorded"

// A store that hangs while it records a run's start holds the job five
// seconds, not for as long as it hangs, and the job then runs unrecorded.
// The write given up on is still under way: here it lands only once the
// run is finished and recorded, and changes nothing.
func TestAStoreThatHangsRecordingAStartDoesNotHoldTheJobBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		k := newStartKit(t)
		job := must[*cronwatch.Job](t)(k.cw.Job("nightly"))
		var ran atomic.Bool
		done := make(chan error, 1)
		go func() {
			_, err := cronwatch.RunValue(bg, job, func(_ context.Context, j *cronwatch.JobContext) (string, error) {
				ran.Store(true)
				j.Log("did the work")
				return "done", nil
			})
			done <- err
		}()
		synctest.Wait()
		eq(t, "still waiting on the store", ran.Load(), false)
		time.Sleep(4999 * time.Millisecond)
		synctest.Wait()
		eq(t, "a millisecond short", ran.Load(), false)
		time.Sleep(time.Millisecond)
		synctest.Wait()
		eq(t, "the job ran once the deadline passed", ran.Load(), true)
		check(t, <-done)
		sameList(t, "reported", k.reported(), []string{slowStart})
		// The start write lands at last: the row is there already, finished.
		close(k.store.gate)
		synctest.Wait()
		runs := must[[]cronwatch.Run](t)(k.cw.Runs(bg, "nightly", 10))
		eq(t, "one run", len(runs), 1)
		eq(t, "status", string(runs[0].Status), "ok")
		eq(t, "output", *runs[0].Output, "did the work")
		eq(t, "none left running", k.running(t), 0)
		eq(t, "nothing more reported", len(k.reported()), 1)
		sameList(t, "no alert", k.alerts.list(), []string{})
	})
}

// The start write lands while the job is still running: its finish is
// written over the row, as for any run.
func TestAStartWriteThatLandsBeforeTheFinishIsFinishedOver(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		k := newStartKit(t)
		job := must[*cronwatch.Job](t)(k.cw.Job("nightly"))
		working := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- job.Run(bg, func(_ context.Context, j *cronwatch.JobContext) error {
				j.Log("did the work")
				<-working
				return errors.New("boom")
			})
		}()
		time.Sleep(5 * time.Second)
		synctest.Wait()
		close(k.store.gate)
		synctest.Wait()
		eq(t, "the row landed while it ran", k.running(t), 1)
		close(working)
		if err := <-done; err == nil || err.Error() != "boom" {
			t.Fatalf("the job's own error: %v", err)
		}
		synctest.Wait()
		runs := must[[]cronwatch.Run](t)(k.cw.Runs(bg, "nightly", 10))
		eq(t, "one run", len(runs), 1)
		eq(t, "status", string(runs[0].Status), "failed")
		eq(t, "none left running", k.running(t), 0)
		sameList(t, "reported", k.reported(), []string{slowStart})
		sameList(t, "judged once", k.alerts.list(), []string{"failed"})
	})
}

// The start write lands after the finish failed to be recorded: the run is
// finished and judged then, not left running to be called stuck.
func TestAStartWriteThatLandsAfterTheFinishFailedIsFinishedThen(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		k := newStartKit(t)
		k.store.down = true // the finish finds the store down, not hanging
		job := must[*cronwatch.Job](t)(k.cw.Job("nightly"))
		done := make(chan error, 1)
		go func() { done <- job.Run(bg, fails("boom")) }()
		time.Sleep(5 * time.Second)
		if err := <-done; err == nil || err.Error() != "boom" {
			t.Fatalf("the job's own error is what Run returns: %v", err)
		}
		wheres := []string{}
		for _, e := range k.reported() {
			where, _, _ := strings.Cut(e, ":")
			wheres = append(wheres, where)
		}
		sameList(t, "the start, then the finish", wheres, []string{"recording nightly", "recording nightly"})
		eq(t, "nothing recorded yet", len(must[[]cronwatch.Run](t)(k.cw.Runs(bg, "nightly", 10))), 0)
		// The start write lands at last, after the finish gave up.
		close(k.store.gate)
		synctest.Wait()
		runs := must[[]cronwatch.Run](t)(k.cw.Runs(bg, "nightly", 10))
		eq(t, "one run", len(runs), 1)
		eq(t, "status", string(runs[0].Status), "failed")
		eq(t, "its error", strings.Contains(*runs[0].Error, "boom"), true)
		eq(t, "none left running", k.running(t), 0)
		sameList(t, "judged then", k.alerts.list(), []string{"failed"})
	})
}

// A run given back (DiscardWhen) whose start write lands afterwards has
// that row taken back as it lands.
func TestAStartWriteThatLandsAfterARunWasGivenBackIsTakenBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		k := newStartKit(t)
		job := must[*cronwatch.Job](t)(k.cw.Job("nightly"))
		snooze := errors.New("snoozed")
		done := make(chan error, 1)
		go func() {
			done <- job.Run(bg, func(context.Context, *cronwatch.JobContext) error { return snooze },
				cronwatch.DiscardWhen(func(err error) bool { return errors.Is(err, snooze) }))
		}()
		time.Sleep(5 * time.Second)
		if err := <-done; !errors.Is(err, snooze) {
			t.Fatalf("got %v", err)
		}
		close(k.store.gate)
		synctest.Wait()
		eq(t, "no row", len(must[[]cronwatch.Run](t)(k.cw.Runs(bg, "nightly", 10))), 0)
		sameList(t, "no alert", k.alerts.list(), []string{})
		sameList(t, "reported", k.reported(), []string{slowStart})
	})
}

// A store that fails, as opposed to hangs, is reported at once and the job
// runs unrecorded, as it always did; and a store's panic while the start is
// written still carries on up the caller's stack.
func TestAStoreThatFailsOrPanicsRecordingAStartIsAsItWas(t *testing.T) {
	store := newTestStore()
	k := newKit(t, cronwatch.WithStore(store))
	job := must[*cronwatch.Job](t)(k.cw.Job("nightly"))
	check(t, job.Run(bg, ok))
	store.breaks("InsertRun")
	began := time.Now()
	check(t, job.Run(bg, ok))
	if took := time.Since(began); took > time.Second {
		t.Errorf("a store that fails held the job %v", took)
	}
	eq(t, "reported", strings.Join(k.wheres(), ","), "recording nightly,recording nightly")
	store.mends("InsertRun")
	store.hook("InsertRun", func() { panic("store bug") })
	defer func() {
		if p := recover(); p != "store bug" {
			t.Errorf("recovered %v", p)
		}
	}()
	_ = job.Run(bg, ok)
	t.Error("the store's panic did not carry on")
}
