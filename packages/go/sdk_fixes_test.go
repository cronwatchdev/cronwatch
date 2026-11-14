package cronwatch_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	cronwatch "cronwatch.dev/go"
)

// partStore is a memory store whose ListRuns fails for the jobs named
// broken, whose Prune always fails, and that counts UpsertJob.
type partStore struct {
	*cronwatch.MemoryStore
	mu      sync.Mutex
	broken  map[string]bool
	upserts atomic.Int64
}

func (s *partStore) ListRuns(ctx context.Context, job string, limit int) ([]cronwatch.Run, error) {
	s.mu.Lock()
	down := s.broken[job]
	s.mu.Unlock()
	if down {
		return nil, errors.New("store down")
	}
	return s.MemoryStore.ListRuns(ctx, job, limit)
}

func (s *partStore) Prune(context.Context, int64) (int, error) {
	return 0, errors.New("prune down")
}

func (s *partStore) UpsertJob(ctx context.Context, def cronwatch.Definition, now int64) error {
	s.upserts.Add(1)
	return s.MemoryStore.UpsertJob(ctx, def, now)
}

type badSource struct{}

func (badSource) Name() string { return "bad" }

func (badSource) Sync(context.Context, cronwatch.SourceHost) ([]cronwatch.Alert, error) {
	return nil, errors.New("source down")
}

// The SDK's review: an error handler that panics does not stop a check,
// the jobs after the one it was called for, or a dashboard read. Every
// report in this port goes through one guarded call; this holds it there.
func TestAPanickingErrorHandlerDoesNotStopACheckOrARead(t *testing.T) {
	store := &partStore{MemoryStore: cronwatch.NewMemoryStore(), broken: map[string]bool{}}
	var calls atomic.Int32
	k := newKit(t, cronwatch.WithStore(store), cronwatch.WithSources(badSource{}),
		cronwatch.WithErrorHandler(func(error, string) {
			calls.Add(1)
			panic("the error handler panicked")
		}))
	must[*cronwatch.Job](t)(k.cw.Job("a-broken", cronwatch.Schedule("every 5m")))
	must[*cronwatch.Job](t)(k.cw.Job("b-late", cronwatch.Schedule("every 5m")))
	must[[]cronwatch.JobSummary](t)(k.cw.Jobs(bg)) // written to the store now, so both are due an hour on
	store.mu.Lock()
	store.broken["a-broken"] = true
	store.mu.Unlock()
	k.c.Advance(HOUR)
	result := checkNow(t, k.cw)
	if calls.Load() < 3 {
		t.Errorf("the source, the broken job, and the prune were each to be reported: %d reports", calls.Load())
	}
	names := []string{}
	for _, j := range result.Jobs {
		names = append(names, j.Name)
	}
	sameList(t, "jobs", names, []string{"a-broken", "b-late"})
	sent := []string{}
	for _, a := range k.alerts.List() {
		sent = append(sent, a.Job+" "+string(a.Type))
	}
	sameList(t, "the job after the broken one was still checked", sent, []string{"b-late missed"})
	// A dashboard read of the broken job answers, showing it as failing.
	eq(t, "health", string(summary(t, k.cw, "a-broken").Health), "failing")
}

// The SDK's review: Flush redacted each flush alone, so a secret that
// began in one and ended in the next (a private key logged a line at a
// time, a label and then its value) had its second part stored and shown.
func TestASecretThatStartsInOneFlushAndEndsInTheNextIsRedactedWhole(t *testing.T) {
	k := newKit(t)
	job := must[*cronwatch.Job](t)(k.cw.Job("deploy"))
	run := must[*cronwatch.RunHandle](t)(job.Start(bg))
	stored := func() string {
		t.Helper()
		r := must[*cronwatch.Run](t)(k.cw.GetRun(bg, run.ID()))
		return *r.Output
	}
	run.Log("starting")
	run.Log("-----BEGIN PRIVATE KEY-----")
	run.Log("MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQC7VJTUt9Us8cKj")
	run.Flush(bg)
	eq(t, "the first flush", stored(), "starting\n[redacted]")
	run.Log("MFNRV0X0e1nSecondHalfOfTheKeySecondHalfOfTheKeySecondHalfOfTheKe")
	run.Log("-----END PRIVATE KEY-----")
	run.Log("api_token:")
	run.Flush(bg)
	eq(t, "the rest of the key", stored(), "starting\n[redacted]\napi_token:")
	// A label flushed, then its value: at the next flush, and again at finish.
	run.Log("hunter2hunter2")
	run.Flush(bg)
	eq(t, "a label, then its value", stored(), "starting\n[redacted]\napi_token:\n[redacted]")
	run.Log("password:")
	run.Flush(bg)
	run.Log("correct-horse")
	finished := run.Finish(bg)
	if finished == nil {
		t.Fatal("not finished")
	}
	eq(t, "at finish", *finished.Output, "starting\n[redacted]\napi_token:\n[redacted]\npassword:\n[redacted]")
	whole := jsonOf(*must[*cronwatch.Run](t)(k.cw.GetRun(bg, run.ID())))
	for _, secret := range []string{"SecondHalf", "hunter2", "correct-horse"} {
		if strings.Contains(whole, secret) {
			t.Errorf("the stored run holds %s: %s", secret, whole)
		}
	}
}

// Flushes after another process wrote to the run still append, each
// redacted, and over a long run the remembered tail stays bounded and the
// output capped.
func TestFlushesAfterAnotherProcessWroteStillAppend(t *testing.T) {
	k := newKit(t)
	job := must[*cronwatch.Job](t)(k.cw.Job("deploy"))
	run := must[*cronwatch.RunHandle](t)(job.Start(bg))
	run.Log("one")
	run.Flush(bg)
	// Another handle on the same run (another process) appends in between.
	other := must[*cronwatch.RunHandle](t)(job.Resume(bg, run.ID()))
	other.Log("from elsewhere")
	other.Flush(bg)
	run.Log("token=abc123abc123")
	run.Flush(bg)
	eq(t, "appended", *must[*cronwatch.Run](t)(k.cw.GetRun(bg, run.ID())).Output, "one\nfrom elsewhere\ntoken=[redacted]")
	for i := range 6 {
		run.Log("line", i, strings.Repeat("x", 6000))
		run.Flush(bg)
	}
	run.Log("secret=tail-secret-value")
	finished := run.Finish(bg)
	if finished == nil {
		t.Fatal("not finished")
	}
	out := *finished.Output
	if len(out) > 16*1024+40 {
		t.Errorf("the output is %d bytes", len(out))
	}
	if !strings.HasSuffix(out, "secret=[redacted]") || strings.Contains(out, "tail-secret-value") {
		t.Errorf("the output ends %q", out[len(out)-60:])
	}
}

// The SDK's review: after a forget the name left the declarations, so a
// handle kept from before it wrote the job's row at every run and the job
// stayed out of the declared set. Its next run declares the job again, and
// writes it once.
func TestAHandleKeptAcrossAForgetDeclaresTheJobAgainAndWritesItOnce(t *testing.T) {
	store := &partStore{MemoryStore: cronwatch.NewMemoryStore(), broken: map[string]bool{}}
	k := newKit(t, cronwatch.WithStore(store))
	job := must[*cronwatch.Job](t)(k.cw.Job("nightly", cronwatch.Schedule("0 2 * * *"), cronwatch.Description("Nightly export")))
	check(t, job.Run(bg, ok))
	eq(t, "written", store.upserts.Load(), 1)
	check(t, k.cw.Forget(bg, "nightly"))
	eq(t, "forgotten", len(k.cw.DefinedJobs()), 0)

	check(t, job.Run(bg, ok))
	eq(t, "written again", store.upserts.Load(), 2)
	defined := k.cw.DefinedJobs()
	if len(defined) != 1 || defined[0].Name() != "nightly" {
		t.Fatalf("declared again, so checks and sources see it: %v", defined)
	}
	eq(t, "declares", k.cw.Declares("nightly"), true)
	check(t, job.Run(bg, ok))
	check(t, job.Run(bg, ok))
	eq(t, "written once, not on every run", store.upserts.Load(), 2)
	eq(t, "as it was declared", summary(t, k.cw, "nightly").Definition.Description(), "Nightly export")

	// Forgotten by another process since: this one's check writes it again.
	check(t, store.MemoryStore.DeleteJob(bg, "nightly"))
	checkNow(t, k.cw)
	if stored := must[*cronwatch.StoredJob](t)(store.GetJob(bg, "nightly")); stored == nil {
		t.Error("the check did not write it again")
	}
}

// The SDK's review, with the fix above: now that a run declares a
// forgotten job again, a forget (a dashboard delete) that landed while a
// check or a dashboard read was on its way to write that job's declaration
// was undone by it. Only a run brings a forgotten job back.
func TestAForgetThatLandsWhileAReadWritesDeclarationsIsNotUndone(t *testing.T) {
	for _, read := range []string{"check", "jobs", "jobSummary"} {
		t.Run(read, func(t *testing.T) {
			store := newTestStore()
			k := newKit(t, cronwatch.WithStore(store))
			must[*cronwatch.Job](t)(k.cw.Job("keep", cronwatch.Schedule("0 2 * * *")))
			must[*cronwatch.Job](t)(k.cw.Job("nightly", cronwatch.Schedule("0 2 * * *")))
			forget := func() { check(t, k.cw.Forget(bg, "nightly")) }
			switch read {
			case "check":
				// Neither is written yet: the check's own loop picked both
				// up, and the forget lands as it writes the first.
				store.hook("UpsertJob", forget)
				checkNow(t, k.cw)
			case "jobs":
				store.hook("UpsertJob", forget)
				must[[]cronwatch.JobSummary](t)(k.cw.Jobs(bg))
			case "jobSummary":
				// Written, then forgotten as the read looks for its row.
				must[[]cronwatch.JobSummary](t)(k.cw.Jobs(bg))
				store.hook("GetJob", forget)
				if got := must[*cronwatch.JobSummary](t)(k.cw.JobSummary(bg, "nightly")); got != nil {
					t.Errorf("a summary of a forgotten job: %v", got.Name)
				}
			}
			defined := k.cw.DefinedJobs()
			if len(defined) != 1 || defined[0].Name() != "keep" {
				t.Errorf("still forgotten here: %v", defined)
			}
			if job := must[*cronwatch.StoredJob](t)(store.inner.GetJob(bg, "nightly")); job != nil {
				t.Error("written again")
			}
			if st := must[*cronwatch.JobState](t)(store.inner.GetState(bg, "nightly")); st != nil {
				t.Error("state left behind")
			}
			// Its next run still brings it back.
			check(t, k.cw.Run(bg, "nightly", ok))
			if job := must[*cronwatch.StoredJob](t)(store.inner.GetJob(bg, "nightly")); job == nil {
				t.Error("a run did not bring it back")
			}
		})
	}
}
