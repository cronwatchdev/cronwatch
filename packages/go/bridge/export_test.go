package bridge

import (
	"testing"
	"time"
)

// SetWaits sets how long a lookup may take, how long a job declared from
// the store is kept before the store is read again, and how long after a
// lookup ran out of time the next is made, until the test ends.
func SetWaits(t *testing.T, lookup, refresh, retry time.Duration) {
	a, b, c := lookupTimeout, fallbackRefresh, lookupRetry
	lookupTimeout, fallbackRefresh, lookupRetry = lookup, refresh, retry
	t.Cleanup(func() { lookupTimeout, fallbackRefresh, lookupRetry = a, b, c })
}

// ReportedForTest is how many reports the watch remembers having made.
func (w *Watch) ReportedForTest() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.reported)
}
