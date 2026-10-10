package cronwatch

import (
	"context"
	"testing"
)

// A process that sees many job names keeps no lock or turn for one that is
// idle (the SDK's serial() drops a job's queue once it is empty).
func TestLocksAndTurnsOfIdleJobsAreDropped(t *testing.T) {
	c := MustNew(WithStore(NewMemoryStore()), WithoutCronSecret())
	defer c.Close()
	for _, name := range []string{"a", "b", "c"} {
		job, err := c.Job(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := job.Run(context.Background(), func(context.Context, *JobContext) error { return nil }); err != nil {
			t.Fatal(err)
		}
		if _, err := c.SyncJob(context.Background(), name); err != nil {
			t.Fatal(err)
		}
	}
	if n := c.lockEntries(); n != 0 {
		t.Errorf("%d entries kept for idle jobs", n)
	}
}

// A wait given up (its context done) leaves no entry behind either.
func TestAGivenUpTurnLeavesNoEntry(t *testing.T) {
	c := MustNew(WithStore(NewMemoryStore()), WithoutCronSecret())
	defer c.Close()
	end, err := c.syncTurn(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.syncTurn(ctx, "a"); err == nil {
		t.Fatal("the second wait was not given up")
	}
	end()
	if n := c.lockEntries(); n != 0 {
		t.Errorf("%d entries kept", n)
	}
}
