package asynq_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cronwatch "cronwatch.dev/go"
	cwasynq "cronwatch.dev/go/asynq"
	"github.com/hibiken/asynq"
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

// The review: the middleware asked the store whether a task's type is a
// job with the task's own context, so while the store did not answer every
// task, of types CronWatch does not watch too, waited out its whole
// deadline and then ran with none left. The lookup has a short deadline of
// its own, tasks of one type share it, and the handler gets the task's
// context as it was given.
func TestAStoreThatHangsCostsATaskAMomentNotItsDeadline(t *testing.T) {
	store := &hungStore{MemoryStore: cronwatch.NewMemoryStore()}
	cw, err := cronwatch.New(cronwatch.WithStore(store), cronwatch.WithErrorHandler(func(error, string) {}))
	check(t, err)
	_, err = cw.Check(context.Background()) // the store is ready before it hangs
	check(t, err)
	w := cwasynq.New(cw, cwasynq.Options{App: "billing", Tasks: map[string][]cronwatch.JobOption{"report:build": nil}})
	store.hung.Store(true)
	store.reads.Store(0)

	type outcome struct {
		err  error
		left time.Duration
	}
	var mu sync.Mutex
	var seen []outcome
	handler := w.Middleware()(asynq.HandlerFunc(func(ctx context.Context, _ *asynq.Task) error {
		deadline, _ := ctx.Deadline()
		mu.Lock()
		seen = append(seen, outcome{ctx.Err(), time.Until(deadline)})
		mu.Unlock()
		return nil
	}))
	began := time.Now()
	var wg sync.WaitGroup
	// Ten tasks of a type nobody watches, and ten of one this server is
	// told to watch, each with two seconds to run.
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			taskType := "email:send"
			if i >= 10 {
				taskType = "report:build"
			}
			check(t, handler.ProcessTask(ctx, asynq.NewTask(taskType, nil)))
		}()
	}
	wg.Wait()
	if took := time.Since(began); took > 1500*time.Millisecond {
		t.Errorf("twenty tasks took %v", took)
	}
	eq(t, "handled", len(seen), 20)
	for _, o := range seen {
		if o.err != nil || o.left < 500*time.Millisecond {
			t.Errorf("a handler began with %v left and ctx.Err() = %v", o.left, o.err)
		}
	}
	if reads := store.reads.Load(); reads > 2 {
		t.Errorf("the store was read %d times for two types", reads)
	}

	// And the next ones do not wait at all.
	began = time.Now()
	for range 20 {
		check(t, handler.ProcessTask(context.Background(), asynq.NewTask("email:send", nil)))
	}
	if took := time.Since(began); took > 200*time.Millisecond {
		t.Errorf("twenty more took %v", took)
	}
}

// The review: the middleware ran the task inside job.Run, which ended its
// context at the job's timeout, an hour unless set, so a task given three
// hours (asynq.Timeout) was cancelled after one. A task's own later
// deadline is left as it is, whatever the job's timeout (Asynq ends the
// task, and a check marks the run stuck at the job's timeout meanwhile);
// one with less time than the job's timeout keeps its own too, and a task
// with no deadline gets the job's timeout.
func TestATasksOwnLaterDeadlineIsNotCutToTheJobsTimeout(t *testing.T) {
	k := newKit(t, nil)
	w := cwasynq.New(k.cw, cwasynq.Options{App: "billing", Tasks: map[string][]cronwatch.JobOption{
		"report:build": nil,                                // the default timeout, an hour
		"report:short": {cronwatch.Timeout("10m")},         // shorter than the task's
		"report:long":  {cronwatch.Timeout(5 * time.Hour)}, // longer than the task's
	}})
	var got time.Time
	handler := w.Middleware()(asynq.HandlerFunc(func(ctx context.Context, _ *asynq.Task) error {
		got, _ = ctx.Deadline()
		return nil
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Hour)
	defer cancel()
	own, _ := ctx.Deadline()
	for _, taskType := range []string{"report:build", "report:short", "report:long"} {
		check(t, handler.ProcessTask(ctx, asynq.NewTask(taskType, nil)))
		eq(t, taskType+" keeps the task's deadline", got.Equal(own), true)
		eq(t, taskType+" recorded", len(k.runs(t, taskType)), 1)
	}

	// A task with no deadline of its own gets the job's timeout.
	check(t, handler.ProcessTask(context.Background(), asynq.NewTask("report:short", nil)))
	if left := time.Until(got); left < 9*time.Minute || left > 10*time.Minute {
		t.Errorf("a task with no deadline got %v", left)
	}
}

// A type the store holds as this app's job is asked for once, not at each
// task (a run reads its job's row once, as any run does), and a type that
// is not one is looked up again only after a minute.
func TestATypeTheStoreHoldsIsNotLookedUpAtEachTask(t *testing.T) {
	store := &hungStore{MemoryStore: cronwatch.NewMemoryStore()}
	scheduling, err := cronwatch.New(cronwatch.WithStore(store))
	check(t, err)
	_, err = scheduling.Job("report:build", cronwatch.Schedule("0 2 * * *"), cronwatch.Tags("asynq", "asynq:billing"))
	check(t, err)
	_, err = scheduling.Check(context.Background())
	check(t, err)

	serving, err := cronwatch.New(cronwatch.WithStore(store))
	check(t, err)
	handler := cwasynq.New(serving, cwasynq.Options{App: "billing"}).Middleware()(asynq.HandlerFunc(func(context.Context, *asynq.Task) error { return nil }))
	check(t, handler.ProcessTask(context.Background(), asynq.NewTask("report:build", nil)))
	check(t, handler.ProcessTask(context.Background(), asynq.NewTask("email:send", nil)))
	store.reads.Store(0)
	for range 10 {
		check(t, handler.ProcessTask(context.Background(), asynq.NewTask("report:build", nil)))
		check(t, handler.ProcessTask(context.Background(), asynq.NewTask("email:send", nil)))
	}
	eq(t, "reads of the store's jobs for ten tasks of each", store.reads.Load(), 10)
	runs, err := serving.Runs(context.Background(), "report:build", 50)
	check(t, err)
	eq(t, "every task recorded", len(runs), 11)
}
