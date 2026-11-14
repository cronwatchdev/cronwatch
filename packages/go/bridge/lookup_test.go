package bridge_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cronwatch "cronwatch.dev/go"
	"cronwatch.dev/go/bridge"
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

// The review: a worker's middleware read the store with the task's own
// context before the work ran, so a store that never answered held each
// task for its whole deadline and then ran it with none left. The lookup's
// context ends soon, and leaves the task's alone.
func TestLookupContextEndsSoonAndLeavesTheTasksContextAlone(t *testing.T) {
	bridge.SetWaits(t, 50*time.Millisecond, time.Minute, time.Minute)
	task, cancelTask := context.WithTimeout(context.WithValue(context.Background(), ctxKey{}, "trace"), time.Hour)
	defer cancelTask()
	lookup, cancel := bridge.LookupContext(task)
	defer cancel()
	eq(t, "the task's values reach the store", lookup.Value(ctxKey{}) == "trace", true)
	select {
	case <-lookup.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the lookup's context did not end")
	}
	eq(t, "the task's context is not ended", task.Err() == nil, true)

	// A task with less left than the lookup may take gives it half of that.
	short, cancelShort := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelShort()
	half, cancelHalf := bridge.LookupContext(short)
	defer cancelHalf()
	deadline, _ := half.Deadline()
	if left := time.Until(deadline); left > 15*time.Millisecond {
		t.Errorf("a task with 20ms left gave its lookup %v", left)
	}

	// The task being cancelled does not end the lookup.
	gone, cancelGone := context.WithCancel(context.Background())
	cancelGone()
	still, cancelStill := bridge.LookupContext(gone)
	defer cancelStill()
	eq(t, "a lookup outlives a task that was cancelled", still.Err() == nil, true)
}

type ctxKey struct{}

// A store that hangs holds the callers of one moment once, together: the
// read of a name is shared, and after one ran out of time none is made for
// a while, where each caller once waited out its own deadline in turn.
func TestAStoreThatHangsHoldsFallbackOnce(t *testing.T) {
	bridge.SetWaits(t, 100*time.Millisecond, time.Minute, time.Minute)
	store := &hungStore{MemoryStore: cronwatch.NewMemoryStore()}
	worker, errs := newClient(t, store)
	check(t, worker) // the store is ready before it hangs
	w := bridge.NewWatch(worker, "river", "billing", "River")
	store.hung.Store(true)
	store.reads.Store(0)

	began := time.Now()
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := bridge.LookupContext(context.Background())
			defer cancel()
			// Ten callers for one name, and one each for ten more.
			name := "report"
			if i >= 10 {
				name = fmt.Sprintf("job-%d", i)
			}
			if job := w.Fallback(ctx, name, nil); job != nil {
				t.Errorf("%s was declared without the store", name)
			}
		}()
	}
	wg.Wait()
	if took := time.Since(began); took > 2*time.Second {
		t.Errorf("twenty lookups of a hung store took %v", took)
	}
	if reads := store.reads.Load(); reads > 11 {
		t.Errorf("the store was read %d times for 11 names", reads)
	}

	// Until the wait is over nothing more is read, and nothing reported.
	store.reads.Store(0)
	reported := len(errs.List())
	began = time.Now()
	for range 50 {
		if job := w.Fallback(context.Background(), "report", nil); job != nil {
			t.Fatal("declared without the store")
		}
	}
	eq(t, "reads while waiting", store.reads.Load(), 0)
	eq(t, "reports while waiting", len(errs.List()), reported)
	if took := time.Since(began); took > time.Second {
		t.Errorf("fifty more took %v", took)
	}
}

// A store that fails at once is asked again by the next caller, as before.
func TestAStoreThatFailsAtOnceIsAskedAgain(t *testing.T) {
	store := &flakyStore{MemoryStore: cronwatch.NewMemoryStore()}
	worker, errs := newClient(t, store)
	w := bridge.NewWatch(worker, "river", "billing", "River")
	store.failing = true
	for range 3 {
		if job := w.Fallback(context.Background(), "report", nil); job != nil {
			t.Fatal("declared without the store")
		}
	}
	eq(t, "each failure reported", len(errs.List()), 3)
	store.failing = false
	if job := w.Fallback(context.Background(), "report", nil); job == nil {
		t.Fatal("no job once the store answers")
	}
}

// The review: Fallback kept the definition it first read for the life of
// the process, so a worker went on judging runs (and ending their
// contexts) by options the scheduling process had since changed. The store
// is read again after a while, and the job declared again when it changed.
func TestFallbackFollowsADefinitionChangedInTheStore(t *testing.T) {
	bridge.SetWaits(t, time.Second, 30*time.Millisecond, time.Minute)
	store := &hungStore{MemoryStore: cronwatch.NewMemoryStore()}
	ctx := context.Background()
	declare := func(options ...cronwatch.JobOption) {
		t.Helper()
		scheduler, _ := newClient(t, store)
		options = append(options, cronwatch.Schedule("0 2 * * *"), cronwatch.Tags("asynq", "asynq:billing"))
		if _, err := scheduler.Job("report", options...); err != nil {
			t.Fatal(err)
		}
		if _, err := scheduler.SyncJob(ctx, "report"); err != nil {
			t.Fatal(err)
		}
	}
	declare(cronwatch.Timeout("1h"))

	worker, errs := newClient(t, store)
	w := bridge.NewWatch(worker, "asynq", "billing", "Asynq")
	first := w.Fallback(ctx, "report", nil)
	if first == nil {
		t.Fatal("no job")
	}
	eq(t, "the stored timeout", fmt.Sprint(first.Definition().Get("timeout")), "1htrue")

	// Kept while it is fresh: the store is not read again.
	reads := store.reads.Load()
	eq(t, "the same job", w.Fallback(ctx, "report", nil) == first, true)
	eq(t, "no read", store.reads.Load(), reads)

	// Unchanged in the store: read again after a while, and the job kept.
	time.Sleep(40 * time.Millisecond)
	eq(t, "the same job once read again", w.Fallback(ctx, "report", nil) == first, true)
	if store.reads.Load() == reads {
		t.Error("the store was not read again")
	}

	// Changed by the scheduling process: the worker's job follows.
	declare(cronwatch.Timeout("3h"), cronwatch.Expect("done"))
	eq(t, "kept until it is read again", w.Fallback(ctx, "report", nil) == first, true)
	time.Sleep(40 * time.Millisecond)
	second := w.Fallback(ctx, "report", nil)
	if second == nil || second == first {
		t.Fatal("the job was not declared again")
	}
	eq(t, "the new timeout", fmt.Sprint(second.Definition().Get("timeout")), "3htrue")
	eq(t, "the new definition", string(must(second.Definition().MarshalJSON())), stored(t, store.MemoryStore, "report"))
	check2(t, second.Run(ctx, func(context.Context, *cronwatch.JobContext) error { return nil }))
	eq(t, "the new expect rule judges the worker's runs", func() string {
		runs, _ := worker.Runs(ctx, "report", 1)
		return *runs[0].Error
	}(), `Output did not contain "done"`)
	eq(t, "the store keeps the scheduler's definition", strings.Contains(stored(t, store.MemoryStore, "report"), `"timeout":"3h"`), true)

	// A store that stops answering leaves the worker the job it has.
	store.hung.Store(true)
	time.Sleep(40 * time.Millisecond)
	lookup, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	eq(t, "the job is kept while the store hangs", w.Fallback(lookup, "report", nil) == second, true)
	eq(t, "and the failure reported", len(errs.List()) > 0, true)
}

// Stored declares nothing for a name the store does not hold as this
// app's job.
func TestStoredIsOnlyAJobTheStoreHoldsAsThisApps(t *testing.T) {
	store := cronwatch.NewMemoryStore()
	ctx := context.Background()
	scheduler, _ := newClient(t, store)
	for name, tag := range map[string]string{"mine": "asynq:billing", "theirs": "asynq:search"} {
		if _, err := scheduler.Job(name, cronwatch.Schedule("0 2 * * *"), cronwatch.Tags("asynq", tag)); err != nil {
			t.Fatal(err)
		}
	}
	check(t, scheduler)
	before := stored(t, store, "mine")

	worker, errs := newClient(t, store)
	w := bridge.NewWatch(worker, "asynq", "billing", "Asynq")
	job := w.Stored(ctx, "mine")
	if job == nil {
		t.Fatal("this app's job was not found")
	}
	eq(t, "its stored definition", string(must(job.Definition().MarshalJSON())), before)
	for _, name := range []string{"theirs", "nobodys"} {
		if w.Stored(ctx, name) != nil {
			t.Errorf("%s was taken for this app's job", name)
		}
		eq(t, name+" is not declared", worker.Declares(name), false)
	}
	eq(t, "nothing reported", len(errs.List()), 0)
}

// The review: a report was remembered by its text, which named the id the
// scheduler gives each entry, so a problem every one-time gocron job has
// was reported once per job and remembered for good. An entry's Key stands
// for its Where in what is remembered.
func TestAProblemIsReportedOnceForAJobHoweverManyEntries(t *testing.T) {
	cw, errs := newClient(t, cronwatch.NewMemoryStore())
	w := bridge.NewWatch(cw, "gocron", "billing", "gocron")
	for i := range 200 {
		where := fmt.Sprintf("gocron job %04d (jobs.Remind)", i)
		w.Declare([]bridge.Entry{{Name: "jobs.Remind", Where: where, Key: "gocron job (jobs.Remind)",
			Problem: errors.New("cronwatch: " + where + " runs once, so it has no schedule")}})
	}
	w.Settle()
	eq(t, "reported", len(errs.List()), 1)
	eq(t, "with the first entry's own words", strings.Contains(errs.List()[0], "gocron job 0000 (jobs.Remind) runs once"), true)
	eq(t, "remembered", w.ReportedForTest(), 1)

	// Another problem for the same job is another report.
	w.Declare([]bridge.Entry{{Name: "jobs.Remind", Where: "gocron job 9999 (jobs.Remind)", Key: "gocron job (jobs.Remind)",
		Problem: errors.New("cronwatch: gocron job 9999 (jobs.Remind) runs every 3 days, which a cron cannot say")}})
	eq(t, "a second problem", len(errs.List()), 2)

	// Without a Key the Where is the key, as it always was.
	w.Declare([]bridge.Entry{{Name: "other", Where: "entry 1", Problem: errors.New("entry 1 is odd")}})
	w.Declare([]bridge.Entry{{Name: "other", Where: "entry 2", Problem: errors.New("entry 2 is odd")}})
	eq(t, "by where", len(errs.List()), 4)

	for i := range 50 {
		w.ReportOnceAs("declaring gocron job\x00is a function literal (main.main.func1)", fmt.Errorf("gocron job %d is a function literal (main.main.func1)", i), fmt.Sprintf("declaring gocron job %d", i))
	}
	eq(t, "a keyed report", len(errs.List()), 5)
}

// Signature is the same for entries that declare the same jobs, whatever
// their order, number, or ids, and differs when a job's schedule does.
func TestSignatureTellsWhatWouldBeDeclared(t *testing.T) {
	a := []bridge.Entry{
		{Name: "report", Where: "entry 1", Schedule: "0 2 * * *", Timezone: "UTC"},
		{Name: "remind", Where: "entry 2", Problem: errors.New("runs once")},
	}
	b := []bridge.Entry{
		{Name: "remind", Where: "entry 7", Problem: errors.New("runs once")},
		{Name: "remind", Where: "entry 8", Problem: errors.New("runs once")},
		{Name: "report", Where: "entry 9", Schedule: "0 2 * * *", Timezone: "UTC"},
	}
	eq(t, "the same jobs", bridge.Signature(a) == bridge.Signature(b), true)
	for what, changed := range map[string][]bridge.Entry{
		"another schedule": {{Name: "report", Schedule: "0 3 * * *", Timezone: "UTC"}, a[1]},
		"another zone":     {{Name: "report", Schedule: "0 2 * * *", Timezone: "Europe/London"}, a[1]},
		"an option":        {{Name: "report", Schedule: "0 2 * * *", Timezone: "UTC", Options: []cronwatch.JobOption{cronwatch.Grace("5m")}}, a[1]},
		"a job gone":       {a[0]},
		"a job more":       {a[0], a[1], {Name: "new"}},
	} {
		eq(t, what, bridge.Signature(a) == bridge.Signature(changed), false)
	}
}
