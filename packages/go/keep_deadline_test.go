package cronwatch_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	cronwatch "cronwatch.dev/go"
)

// The review: a queue's worker middleware runs the work inside job.Run,
// which ended its context at the job's timeout, an hour unless set, so an
// Asynq task allowed three hours was cancelled after one. With
// KeepDeadline a context that already ends later than the timeout is left
// to its own deadline.
func TestKeepDeadlineLeavesALaterDeadlineToEndTheJob(t *testing.T) {
	// How the job's context stood after working past the job's timeout.
	worked := func(t *testing.T, deadline time.Duration, options ...cronwatch.RunOption) (err, cause error) {
		t.Helper()
		k := newKit(t)
		job := must[*cronwatch.Job](t)(k.cw.Job("long", cronwatch.Timeout("1h")))
		ctx := context.Background()
		if deadline > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, deadline)
			defer cancel()
		}
		_ = job.Run(ctx, func(ctx context.Context, _ *cronwatch.JobContext) error {
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Hour):
			}
			err, cause = ctx.Err(), context.Cause(ctx)
			return nil
		}, options...)
		return err, cause
	}
	synctest.Test(t, func(t *testing.T) {
		if err, _ := worked(t, 3*time.Hour, cronwatch.KeepDeadline()); err != nil {
			t.Errorf("a context with three hours left was ended after one: %v", err)
		}
	})
	synctest.Test(t, func(t *testing.T) {
		// Without the option the job's timeout ends it, as it always has.
		err, cause := worked(t, 3*time.Hour)
		if !errors.Is(err, context.DeadlineExceeded) || cause == nil || cause.Error() != `job "long" passed its timeout of 1h` {
			t.Errorf("got %v (%v)", err, cause)
		}
	})
	synctest.Test(t, func(t *testing.T) {
		// A context with no deadline gets the job's timeout.
		_, cause := worked(t, 0, cronwatch.KeepDeadline())
		if cause == nil || cause.Error() != `job "long" passed its timeout of 1h` {
			t.Errorf("no deadline: %v", cause)
		}
	})
	synctest.Test(t, func(t *testing.T) {
		// And so does one that ends sooner than the timeout, which then
		// ends it first.
		start := time.Now()
		err, _ := worked(t, 30*time.Minute, cronwatch.KeepDeadline())
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != 30*time.Minute {
			t.Errorf("an earlier deadline: %v after %v", err, time.Since(start))
		}
	})
}

// A run that outlives the job's timeout under KeepDeadline is still marked
// stuck by a check, and recovers when it finishes.
func TestARunKeptPastItsTimeoutIsStillReportedStuck(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		k := newKit(t, cronwatch.WithClock(func() int64 { return time.Now().UnixMilli() }))
		job := must[*cronwatch.Job](t)(k.cw.Job("long", cronwatch.Timeout("1h")))
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Hour)
		defer cancel()
		var during []string
		check(t, job.Run(ctx, func(ctx context.Context, _ *cronwatch.JobContext) error {
			time.Sleep(90 * time.Minute)
			result, err := k.cw.Check(context.Background())
			if err != nil {
				return err
			}
			during = alertTypes(result.Alerts)
			return ctx.Err()
		}, cronwatch.KeepDeadline()))
		sameList(t, "the check while it ran", during, []string{"stuck"})
		eq(t, "it finished ok", string(runs(t, k.cw, "long")[0].Status), "ok")
		eq(t, "and is healthy again", string(summary(t, k.cw, "long").Health), "healthy")
	})
}
