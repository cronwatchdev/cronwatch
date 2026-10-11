package cronwatch

// Sending alerts: channels, triage, and the queue of alerts some channel
// refused, retried once per check to those channels (client.ts dispatch, retryUndelivered,
// recordDelivery, deliver, addTriage).

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
)

// Channel is where alerts go.
type Channel interface {
	// Name names the channel in errors ("alert channel <name>").
	Name() string
	// Send returns once the alert went out (to at least one recipient), or
	// an error when it went nowhere, and then the next check sends it to
	// this channel again, and to no channel that took it. ctx ends after 15
	// seconds.
	Send(ctx context.Context, alert Alert, cc ChannelContext) error
}

// ChannelContext is what the client hands a channel with each alert.
type ChannelContext struct {
	report func(error)
}

// NewChannelContext is a channel context that reports to report, for
// sending to a channel outside a client (a test, say).
func NewChannelContext(report func(error)) ChannelContext {
	return ChannelContext{report: report}
}

// ReportError reports a problem that did not stop the alert going out,
// such as one of several recipients refusing it. It goes to the client's
// error handler; the zero ChannelContext writes it to standard error, as
// the SDK does for a channel called without one.
func (cc ChannelContext) ReportError(err error) {
	if cc.report != nil {
		cc.report(err)
		return
	}
	fmt.Fprintln(Stderr, "[cronwatch] alert channel:", err)
}

type funcChannel struct {
	name string
	send func(context.Context, Alert) error
}

func (f funcChannel) Name() string { return f.name }

func (f funcChannel) Send(ctx context.Context, alert Alert, _ ChannelContext) error {
	return f.send(ctx, alert)
}

// ChannelFunc wraps a function as a channel (the SDK's custom()).
func ChannelFunc(name string, send func(ctx context.Context, alert Alert) error) Channel {
	return funcChannel{name, send}
}

// Console is the default channel: it writes each alert to standard error,
// and recoveries to standard output.
func Console() Channel {
	return ChannelFunc("console", func(_ context.Context, alert Alert) error {
		line := "[cronwatch] " + alert.Title + "\n" + alert.Message
		if alert.Triage != nil && *alert.Triage != "" {
			line += "\nTriage: " + *alert.Triage
		}
		w := Stderr
		if alert.Type == AlertRecovered {
			w = Stdout
		}
		_, err := fmt.Fprintln(w, line)
		return err
	})
}

// TriageFunc diagnoses an alert in a few sentences, or answers "" for no
// diagnosis. ctx ends when the client stops waiting (25 seconds); pass it
// to any request made.
type TriageFunc func(ctx context.Context, tc TriageContext) (string, error)

// TriageContext is what a triage function is given.
type TriageContext struct {
	Alert Alert
	// The job's five newest runs.
	RecentRuns []Run
}

// Source is where runs this process does not wrap come from, such as
// pg_cron's jobs. Check syncs each one first, so what it records is
// evaluated in the same check.
type Source interface {
	Name() string
	// Sync declares the jobs and records their new runs, returning the
	// alerts recording them sent.
	Sync(ctx context.Context, host SourceHost) ([]Alert, error)
}

// SourceHost is what a Source may use of the client. A *Client is one.
type SourceHost interface {
	Job(name string, options ...JobOption) (*Job, error)
	RecordRun(ctx context.Context, run Run, options ...RecordOption) ([]Alert, error)
	Store() Store
	Now() int64
	ReportError(err error, where string)
}

var _ SourceHost = (*Client)(nil)

// held is the alerts written with the state that opened their conditions,
// and how many older ones the queue let go.
type held struct {
	alerts  []Alert
	dropped int
}

// outbox is an evaluation as it is written: its drafts composed into alerts
// and held in the same state (holdAlerts), so the write that opens a
// condition also keeps its alerts, and a process that stops before sending
// them does not lose them. Called inside updateState, so it only computes.
func (c *Client) outbox(state JobState, drafts []alertDraft, def Definition, now int64) (JobState, held) {
	alerts := make([]Alert, len(drafts))
	for i, d := range drafts {
		alerts[i] = composeAlert(d, def, now)
	}
	next, dropped := holdAlerts(state, alerts, c.now()+sendLeaseMs, c.deferDelivery)
	return next, held{alerts, dropped}
}

// reportDropped reports alerts let go because a job's queue was full.
func (c *Client) reportDropped(name string, dropped int) {
	if dropped <= 0 {
		return
	}
	plural := "s"
	if dropped == 1 {
		plural = ""
	}
	c.report(fmt.Errorf("%d undelivered alert%s for %s dropped: only the newest %d are kept for retry", dropped, plural, name, maxUndelivered), "alert queue for "+name)
}

// dispatch triages and sends each alert the outbox holds (see outbox). The
// state, with the alerts in it, was saved before this, so a slow channel
// holds up nothing else; afterwards only the delivery fields are written
// back, onto a fresh read of the state, and the alerts leave Sending.
// Triage is made here, never stored with the held alert: the write that
// opens a condition cannot wait for it, and a retry triages an alert that
// has none. The alerts sent are the ones composed, whole runs and all; only
// a retry reads the trimmed copy held in state. With DeliverAtCheck the
// alerts were queued for a check elsewhere instead.
func (c *Client) dispatch(ctx context.Context, name string, alerts []Alert, now int64) []Alert {
	if len(alerts) == 0 || c.deferDelivery {
		return alerts
	}
	sent := make([]Alert, len(alerts))
	var delivered, failed []Alert
	reached := false
	for i, alert := range alerts {
		alert = alert.clone()
		if c.triage != nil && alert.Type != AlertRecovered {
			c.addTriage(ctx, &alert, triageTimeout)
		}
		left, owes, took := c.deliver(ctx, alert)
		reached = reached || took
		if owes {
			failed = append(failed, left)
		} else {
			delivered = append(delivered, alert)
		}
		sent[i] = alert
	}
	c.recordDelivery(ctx, name, delivered, failed, nil, now, reached)
	return sent
}

// retryUndelivered sends the alerts some channel refused last time, once
// each, oldest first, to the channels that still owe them and no others,
// from the copy held in state, its run trimmed (heldRun). They are claimed
// first (claimUndelivered): moved into Sending with a lease, in one
// conditional write, so when several processes check at once only the one
// whose write lands sends them. An alert that no longer describes the job
// (staleAlert) is dropped instead. Retries across a check share
// retryBudget of wall-clock time; claimed alerts it has no time left for go
// back to the queue for the next check. It returns the alerts some channel
// took on this attempt.
func (c *Client) retryUndelivered(ctx context.Context, name string, state JobState, now int64, spent *time.Duration) []Alert {
	if len(state.Undelivered) == 0 || c.deferDelivery {
		return []Alert{}
	}
	_, claimed, err := updateState(ctx, c, name, func(s JobState) (JobState, []Alert, error) {
		next, claimed, _ := claimUndelivered(s, now, c.now()+sendLeaseMs)
		return next, claimed, nil
	})
	if err != nil {
		c.report(err, "claiming undelivered alerts for "+name)
		return []Alert{}
	}
	if len(claimed) == 0 {
		return []Alert{}
	}
	var delivered, failed []Alert
	took := []Alert{}
	for _, a := range claimed {
		left := retryBudget - *spent
		if left <= 0 {
			// Not tried: back to the queue as it was, owed and all.
			failed = append(failed, a)
			continue
		}
		started := time.Now()
		alert := a.clone()
		// An alert queued by a process that delivers at check time was never
		// triaged. One that was tried (TriageTried) is not tried again.
		if c.triage != nil && alert.Type != AlertRecovered && !alert.TriageTried {
			c.addTriage(ctx, &alert, min(triageTimeout, left))
		}
		rest, owes, reached := c.deliver(ctx, alert)
		if reached {
			took = append(took, withoutOwed(alert))
		}
		if owes {
			failed = append(failed, rest)
		} else {
			delivered = append(delivered, alert)
		}
		*spent += max(0, time.Since(started))
	}
	c.recordDelivery(ctx, name, delivered, failed, nil, now, len(took) > 0)
	return took
}

// recordDelivery marks delivered alerts done, drops stale ones, and keeps
// failed ones for the next check, taking them all out of Sending
// (recordSent). A failed alert replaces its stored copy, so a triage made
// on this attempt and the channels that still owe it are kept. LastAlertAt
// moves when reached, some channel accepted an alert on this attempt. When
// this write fails, alerts still in Sending are retried once their lease
// runs out.
func (c *Client) recordDelivery(ctx context.Context, name string, delivered, failed, stale []Alert, now int64, reached bool) {
	_, trimmed, err := updateState(ctx, c, name, func(previous JobState) (JobState, int, error) {
		state, dropped := recordSent(normalizeState(&previous, name), delivered, failed, stale, now, reached)
		return state, dropped, nil
	})
	if err != nil {
		c.report(err, "recording alert delivery for "+name)
		return
	}
	c.reportDropped(name, trimmed)
}

// deliver sends the alert to every channel that still owes it
// (owedChannels: all of them for an alert not sent before), at once, each
// failure reported under its channel's name. left is the alert with owed
// cut to the channels that refused it, when owes (some did); reached is
// whether any channel accepted it. With no channels at all it counts as
// delivered, as it always has. A channel that has not returned from an
// earlier send past its timeout gets nothing more until it does, so a hung
// channel holds one goroutine rather than one per alert.
func (c *Client) deliver(ctx context.Context, alert Alert) (left Alert, owes, reached bool) {
	if len(c.alerts) == 0 {
		return Alert{}, false, true
	}
	names := make([]string, len(c.alerts))
	for i, ch := range c.alerts {
		names[i] = ch.Name()
	}
	keys := channelKeys(names)
	owed := owedChannels(alert, keys)
	if len(owed) == 0 {
		return Alert{}, false, false
	}
	plain := withoutOwed(alert)
	// Each channel's failure is reported as soon as that channel answers,
	// so in the order they answer; owed keeps the configured order.
	refused := make([]bool, len(c.alerts))
	var wg sync.WaitGroup
	for i, ch := range c.alerts {
		if !slices.Contains(owed, keys[i]) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.sendOne(ctx, i, ch, plain); err != nil {
				refused[i] = true
				c.report(err, "alert channel "+ch.Name())
			}
		}()
	}
	wg.Wait()
	var failed []string
	for i, no := range refused {
		if no {
			failed = append(failed, keys[i])
		}
	}
	left, owes = afterSend(alert, owed, failed)
	return left, owes, len(failed) < len(owed)
}

// sendOne sends one alert to one channel within channelTimeout, and says
// why it did not go, which deliver reports at once. A channel whose earlier send
// ran past its timeout and has still not returned is skipped until it does,
// so a hung channel holds one goroutine rather than one per alert.
func (c *Client) sendOne(ctx context.Context, i int, ch Channel, alert Alert) error {
	where := "alert channel " + ch.Name()
	c.busyMu.Lock()
	stalled := c.channelBusy[i] > 0
	c.busyMu.Unlock()
	if stalled {
		return errors.New("still sending an earlier alert, past its timeout")
	}
	sendCtx, cancel := context.WithTimeout(storeCtx(ctx), channelTimeout)
	defer cancel()
	done := make(chan error, 1)
	// finished and counted are this send's own, under busyMu: only a send
	// counted as stalled uncounts itself, so another send to the channel
	// that returns in time never clears a hung one's mark.
	finished, counted := false, false
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- fmt.Errorf("panicked: %v", p)
			}
			c.busyMu.Lock()
			finished = true
			if counted {
				if c.channelBusy[i]--; c.channelBusy[i] <= 0 {
					delete(c.channelBusy, i)
				}
			}
			c.busyMu.Unlock()
		}()
		done <- ch.Send(sendCtx, alert.clone(), ChannelContext{report: func(err error) { c.report(err, where) }})
	}()
	timer := time.NewTimer(channelTimeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		c.busyMu.Lock()
		if !finished {
			counted = true
			c.channelBusy[i]++
		}
		c.busyMu.Unlock()
		return fmt.Errorf("timed out after %dms", channelTimeout.Milliseconds())
	}
}

// addTriage sets the alert's triage to the diagnosis, or to none, so it is
// tried once per alert. Reading the job's recent runs counts toward the
// timeout. A triage that ran past its timeout and has still
// not returned holds off the next one until it does, as a hung channel is.
func (c *Client) addTriage(ctx context.Context, alert *Alert, timeout time.Duration) {
	where := "triage for " + alert.Job
	alert.TriageTried = true
	alert.Triage = nil
	c.busyMu.Lock()
	stalled := c.triageBusy > 0
	c.busyMu.Unlock()
	if stalled {
		c.report(errors.New("an earlier triage is still running past its timeout"), where)
		return
	}
	// One deadline for the read of the job's runs and the triage both, so a
	// store that never answers cannot hold the check past it.
	tctx, cancel := context.WithTimeout(storeCtx(ctx), timeout)
	defer cancel()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	recent, err := c.store.ListRuns(tctx, alert.Job, 5)
	if err != nil {
		if tctx.Err() != nil {
			err = fmt.Errorf("timed out after %dms", timeout.Milliseconds())
		}
		c.report(err, where)
		return
	}
	type answer struct {
		text string
		err  error
	}
	done := make(chan answer, 1)
	// As for a channel: only a triage counted as stalled uncounts itself.
	finished, counted := false, false
	tc := TriageContext{Alert: alert.clone(), RecentRuns: recent}
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- answer{err: fmt.Errorf("panicked: %v", p)}
			}
			c.busyMu.Lock()
			finished = true
			if counted {
				c.triageBusy--
			}
			c.busyMu.Unlock()
		}()
		text, err := c.triage(tctx, tc)
		done <- answer{text, err}
	}()
	select {
	case a := <-done:
		if a.err != nil {
			c.report(a.err, where)
			return
		}
		if a.text != "" {
			alert.Triage = ptr(a.text)
		}
	case <-timer.C:
		cancel()
		c.busyMu.Lock()
		if !finished {
			counted = true
			c.triageBusy++
		}
		c.busyMu.Unlock()
		c.report(fmt.Errorf("timed out after %dms", timeout.Milliseconds()), where)
	}
}
