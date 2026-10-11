package cronwatch

// Pure decisions about a job's health (evaluate.ts). Each function takes the
// current state and returns the new state plus the alerts that should go
// out. Nothing here touches a store or a network, which is what makes it
// testable, and what lets conformance/evaluate.json replay a job's life
// through it event by event.

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"

	"cronwatch.dev/go/internal/js"
	"cronwatch.dev/go/internal/schedule"
)

type evaluation struct {
	state  JobState
	alerts []alertDraft
}

// alertDraft is an alert before it has a title and message (composeAlert).
type alertDraft struct {
	Type    AlertType
	Run     *Run
	Details AlertDetails
}

const (
	defaultGraceMs   = 10 * 60_000
	defaultTimeoutMs = 60 * 60_000
	// Runs faster than this are never called slow, whatever the baseline says.
	slowFloorMs = 10_000
	// How many earlier runs a baseline needs before it is trusted.
	baselineMinRuns = 5
	// How many successful runs a baseline looks at, and how many runs a summary covers.
	baselineWindow = 20
)

func emptyState(job string) JobState {
	return JobState{Job: job, Open: []OpenCondition{}, PendingRecovery: []Condition{}, Undelivered: []Alert{}}
}

// normalizeState is a stored state with every field present, or a fresh
// one. State written by an older version lacks the newer fields. Sending and
// UnderFloor are the exceptions: each is there only while it holds
// something, and then last, after any key another writer added.
func normalizeState(state *JobState, job string) JobState {
	if state == nil {
		return emptyState(job)
	}
	s := state.clone()
	s.ConsecutiveFailures = failureCount(s.ConsecutiveFailures)
	if s.Job == "" {
		s.Job = job
	}
	if s.Open == nil {
		s.Open = []OpenCondition{}
	}
	if s.PendingRecovery == nil {
		s.PendingRecovery = []Condition{}
	}
	if s.Undelivered == nil {
		s.Undelivered = []Alert{}
	}
	tail := s.tail[:0:0]
	for _, k := range s.tail {
		if k != "sending" && k != "underFloor" && k != "pendingRuns" {
			tail = append(tail, k)
		}
	}
	delete(s.extra, "sending")
	delete(s.extra, "underFloor")
	delete(s.extra, "pendingRuns")
	if len(s.Sending) > 0 {
		tail = append(tail, "sending")
	} else {
		s.Sending = nil
	}
	if len(s.UnderFloor) > 0 {
		tail = append(tail, "underFloor")
	} else {
		s.UnderFloor = nil
	}
	if len(s.PendingRuns) > 0 {
		tail = append(tail, "pendingRuns")
	} else {
		s.PendingRuns = nil
	}
	s.tail = tail
	return s
}

// ---------------------------------------------------------------- delivery

// sendLeaseMs is how long an alert in Sending is left to the process
// sending it. Longer than any send takes: at most three alerts go out
// together, each with 25 seconds of triage and 15 of channels.
const sendLeaseMs = 5 * 60_000

// alertKey identifies an alert across retries, and in Sending.
func alertKey(a Alert) string {
	id := ""
	if a.Run != nil {
		id = a.Run.ID
	}
	at := strconv.FormatInt(a.At, 10)
	if raw, ok := a.at().(float64); ok {
		at = js.FormatNumber(raw)
	}
	return string(a.Type) + "|" + at + "|" + id
}

// heldTextCap is how much of a held alert's run error and output is kept:
// 2048 UTF-16 code units of each.
const heldTextCap = 2048

// heldTextSlack is how far past its cap a held text may run and still be
// kept whole: longer than any marker, so a text trimmed once (its kept part
// and its marker) is never trimmed again, and the count in its marker
// stays true.
const heldTextSlack = 64

// heldMessageCap is how much of a held alert's message is kept: its first
// 4096 UTF-16 code units.
const heldMessageCap = 4096

// heldRun is a run as an alert held in a job's state keeps it (Sending and
// Undelivered), so the state stays small however much a run logged. Its
// error keeps its first heldTextCap units and its output its last, each cut
// so as not to split a surrogate pair, with a line saying how many units
// were left out: "<head>\n[N more characters trimmed]" and "[N earlier
// characters trimmed]\n<tail>". A text of at most heldTextCap +
// heldTextSlack units is kept as it is, and so is every other field. The
// first send of an alert uses the alert as composed; only a retry reads the
// held copy.
func heldRun(run Run) Run {
	out := run.clone()
	if run.Error != nil && js.Length16(*run.Error) > heldTextCap+heldTextSlack {
		out.Error = ptr(keepHead(*run.Error, heldTextCap))
	}
	if run.Output != nil && js.Length16(*run.Output) > heldTextCap+heldTextSlack {
		out.Output = ptr(keepTail(*run.Output))
	}
	return out
}

// keepHead is the first max units of text, one fewer when a surrogate pair
// would be cut, and how many units went.
func keepHead(text string, max int) string {
	end := max
	if pairStartsAt(text, end-1) {
		end--
	}
	return js.Slice16(text, 0, end) + "\n[" + strconv.Itoa(js.Length16(text)-end) + " more characters trimmed]"
}

// keepTail is the last heldTextCap units of text, one fewer when a
// surrogate pair would be cut, and how many units went before them.
func keepTail(text string) string {
	n := js.Length16(text)
	start := n - heldTextCap
	if pairStartsAt(text, start-1) {
		start++
	}
	return "[" + strconv.Itoa(start) + " earlier characters trimmed]\n" + js.Slice16(text, start, n)
}

// pairStartsAt is whether a character of two UTF-16 units (a surrogate
// pair) starts at unit i of text.
func pairStartsAt(text string, i int) bool {
	at := 0
	for _, r := range text {
		if at == i {
			return r >= 0x10000
		}
		if at > i {
			return false
		}
		if r >= 0x10000 {
			at += 2
		} else {
			at++
		}
	}
	return false
}

// heldAlert is an alert as it is held in a job's state: its run trimmed
// (heldRun), and its message, when longer than heldMessageCap +
// heldTextSlack units, cut to its first heldMessageCap like a run's error
// ("<head>\n[N more characters trimmed]"). Everything else, the title and
// owed included, as it is. The first send uses the alert as composed.
func heldAlert(alert Alert) Alert {
	out := alert.clone()
	if alert.Run != nil {
		r := heldRun(*alert.Run)
		out.Run = &r
	}
	if js.Length16(alert.Message) > heldMessageCap+heldTextSlack {
		out.Message = keepHead(alert.Message, heldMessageCap)
	}
	return out
}

// channelKeys is each configured channel's key, in the order configured:
// its name, with "#2", "#3", and so on for the second and later channels of
// the same name, so two webhooks are "webhook" and "webhook#2". A key
// already given to an earlier channel (one named "webhook#2" itself, say)
// is skipped for the next number up, so every key is different. A held
// alert names the channels that still owe it by these keys.
func channelKeys(names []string) []string {
	seen := map[string]int{}
	taken := map[string]bool{}
	keys := make([]string, len(names))
	for i, name := range names {
		n := seen[name] + 1
		seen[name] = n
		key := name
		if n != 1 {
			key = name + "#" + strconv.Itoa(n)
		}
		for taken[key] {
			n++
			key = name + "#" + strconv.Itoa(n)
		}
		taken[key] = true
		keys[i] = key
	}
	return keys
}

// owedChannels is the channels a held alert is still owed by, of those
// configured now (channelKeys order): those in its owed that are still
// configured, or every configured one when it has no owed list (an alert
// held by a release before per-channel delivery, or one never sent). An
// entry of owed that is not a string is ignored. None left means the alert
// is done.
func owedChannels(alert Alert, configured []string) []string {
	owed, ok := alert.owed.([]any)
	if !alert.hasOwed || !ok {
		return append([]string{}, configured...)
	}
	out := []string{}
	for _, key := range configured {
		if slices.Contains(owed, any(key)) {
			out = append(out, key)
		}
	}
	return out
}

// afterSend is an alert after a send to the channels in tried, of which
// those in failed refused it: false when none is left owing it, else the
// alert with owed set to those that refused, in configured order.
func afterSend(alert Alert, tried, failed []string) (Alert, bool) {
	owed := []any{}
	for _, key := range tried {
		if slices.Contains(failed, key) {
			owed = append(owed, key)
		}
	}
	if len(owed) == 0 {
		return Alert{}, false
	}
	out := alert.clone()
	out.owed, out.hasOwed = owed, true
	return out, true
}

// withoutOwed is an alert without its owed list, as a channel is given it.
func withoutOwed(alert Alert) Alert {
	out := alert.clone()
	out.owed, out.hasOwed = nil, false
	if out.read != nil && out.read.Has("owed") {
		out.read.Delete("owed")
	}
	return out
}

// queueUndelivered adds alerts to the undelivered queue, each with its run
// trimmed (heldAlert) and its owed list as given: one with the same key as
// a queued alert replaces it where it stands, the rest go at the end, and
// only the newest maxUndelivered stay. It returns how many went.
func queueUndelivered(state JobState, alerts []Alert) (JobState, int) {
	next := cloneState(state)
	byKey := map[string]Alert{}
	for _, a := range alerts {
		byKey[alertKey(a)] = heldAlert(a)
	}
	queue := []Alert{}
	known := map[string]bool{}
	for _, a := range next.Undelivered {
		key := alertKey(a)
		if r, ok := byKey[key]; ok {
			a = r
		}
		queue = append(queue, a.clone())
		known[key] = true
	}
	for _, a := range alerts {
		if key := alertKey(a); !known[key] {
			// Once each, as the SDK's map of them keeps one per key.
			known[key] = true
			queue = append(queue, byKey[key])
		}
	}
	dropped := max(0, len(queue)-maxUndelivered)
	next.Undelivered = queue[dropped:]
	return next, dropped
}

// holdAlerts is the outbox. Alerts just composed are written with the
// state that opens their condition, before any is sent, so a process that
// stops part way does not lose them: into Sending, each with its lease
// ending at until, when this process sends them, or (deferred, the
// client's DeliverAtCheck) straight into the undelivered queue for a check
// elsewhere. Either way the copy held has its run trimmed (heldAlert) and
// no owed list, so every configured channel owes it; the first send uses
// the alerts as composed. It returns how many alerts past maxUndelivered
// went.
func holdAlerts(state JobState, alerts []Alert, until int64, deferred bool) (JobState, int) {
	if len(alerts) == 0 {
		return state, 0
	}
	if deferred {
		return queueUndelivered(state, alerts)
	}
	next := cloneState(state)
	list := next.Sending
	for _, a := range alerts {
		list = append(list, SendingAlert{Until: until, Alert: heldAlert(a)})
	}
	dropped := max(0, len(list)-maxUndelivered)
	next.Sending = list[dropped:]
	return next, dropped
}

// releaseSending takes the alerts in Sending whose lease ran out by now
// (the process sending them stopped before it recorded how the send went)
// to the undelivered queue, where the retry sends them (with triage, which
// is never stored with them there) or drops them as stale, owed by the
// channels its owed names. An entry with no alert is dropped; one with no
// number for until counts as run out.
func releaseSending(state JobState, now int64) (JobState, int) {
	var held []SendingAlert
	var released []Alert
	lapsed := false
	for _, e := range state.Sending {
		if !e.lapsed(now) {
			held = append(held, e)
			continue
		}
		lapsed = true
		if !e.noAlert {
			released = append(released, e.Alert)
		}
	}
	if !lapsed {
		return state, 0
	}
	next := state.clone()
	next.Sending = held
	return queueUndelivered(next, released)
}

// recordSent is how a send went. Delivered alerts (no channel owes them any
// more) and stale ones leave the queue; failed ones (some channel still
// owes them, named by their owed) replace their queued copy, so a triage
// made on this attempt and the shrunken owed are kept, or join the queue.
// Every one of them leaves Sending. LastAlertAt moves to now when reached
// (some channel accepted an alert on this attempt). It returns how many
// alerts past maxUndelivered went.
func recordSent(state JobState, delivered, failed, stale []Alert, now int64, reached bool) (JobState, int) {
	next := cloneState(state)
	done := map[string]bool{}
	sent := map[string]bool{}
	for _, list := range [][]Alert{delivered, stale} {
		for _, a := range list {
			done[alertKey(a)] = true
			sent[alertKey(a)] = true
		}
	}
	for _, a := range failed {
		sent[alertKey(a)] = true
	}
	queue := []Alert{}
	for _, a := range next.Undelivered {
		if !done[alertKey(a)] {
			queue = append(queue, a)
		}
	}
	next.Undelivered = queue
	var held []SendingAlert
	for _, e := range next.Sending {
		if e.noAlert || !sent[alertKey(e.Alert)] {
			held = append(held, e)
		}
	}
	next.Sending = held
	if reached {
		next.LastAlertAt = ptr(now)
	}
	return queueUndelivered(next, failed)
}

// claimUndelivered is a check's claim on the undelivered queue, written
// before it sends, so two processes checking at once never retry the same
// alert: the queued alerts that still describe the job move into Sending,
// oldest first, each with its lease ending at until, as many as Sending has
// room for under maxUndelivered (the rest wait for the next check); stale
// ones (staleAlert) leave the queue. While the job is silenced only
// recoveries are claimed. The process whose write lands sends claimed.
func claimUndelivered(state JobState, now, until int64) (next JobState, claimed, stale []Alert) {
	next = cloneState(state)
	claimed, stale = []Alert{}, []Alert{}
	silenced := isSilenced(next, now)
	room := max(0, maxUndelivered-len(next.Sending))
	left := []Alert{}
	for _, a := range next.Undelivered {
		switch {
		case staleAlert(a, next):
			stale = append(stale, a)
		case len(claimed) < room && (!silenced || a.Type == AlertRecovered):
			claimed = append(claimed, a)
		default:
			left = append(left, a)
		}
	}
	if len(stale) == 0 && len(claimed) == 0 {
		return state, claimed, stale
	}
	next.Undelivered = left
	for _, a := range claimed {
		next.Sending = append(next.Sending, SendingAlert{Until: until, Alert: a.clone()})
	}
	return next, claimed, stale
}

// maxPendingRuns is how many runs PendingRuns holds per job; past it the
// oldest go.
const maxPendingRuns = 100

// holdRun is a run just started, held in PendingRuns until its finish is
// evaluated (releaseRun), so a check can evaluate it if the process that
// finished it stops before writing the evaluation. Written with
// onRunStart's state. Only the newest maxPendingRuns are kept.
func holdRun(state JobState, runID string) JobState {
	next := cloneState(state)
	held := slices.DeleteFunc(slices.Clone(next.PendingRuns), func(id string) bool { return id == runID })
	held = append(held, runID)
	next.PendingRuns = held[max(0, len(held)-maxPendingRuns):]
	if !slices.Contains(next.tail, "pendingRuns") {
		next.tail = append(next.tail, "pendingRuns")
	}
	return next
}

// releaseRun takes a run out of PendingRuns, as its evaluation is written
// or once it is gone from the store.
func releaseRun(state JobState, runID string) JobState {
	if !slices.Contains(state.PendingRuns, runID) {
		return state
	}
	next := cloneState(state)
	next.PendingRuns = slices.DeleteFunc(next.PendingRuns, func(id string) bool { return id == runID })
	if len(next.PendingRuns) == 0 {
		next.PendingRuns = nil
		next.tail = slices.DeleteFunc(next.tail, func(k string) bool { return k == "pendingRuns" })
	}
	return next
}

func cloneState(s JobState) JobState { return normalizeState(&s, s.Job) }

func openCondition(s *JobState, c Condition, now int64) bool {
	if _, ok := s.openAt(c); ok {
		return false
	}
	s.Open = append(s.Open, OpenCondition{c, now})
	return true
}

// closeCondition closes c. Every open condition has alerted, so closing one
// owes a recovered message; it is remembered until a successful run leaves
// nothing open and sends it.
func closeCondition(s *JobState, c Condition) bool {
	if !deleteOpen(s, c) {
		return false
	}
	if !hasCondition(s.PendingRecovery, c) {
		s.PendingRecovery = append(s.PendingRecovery, c)
	}
	return true
}

func deleteOpen(s *JobState, c Condition) bool {
	for i, o := range s.Open {
		if o.Condition == c {
			s.Open = append(s.Open[:i:i], s.Open[i+1:]...)
			return true
		}
	}
	return false
}

func hasCondition(list []Condition, c Condition) bool {
	for _, x := range list {
		if x == c {
			return true
		}
	}
	return false
}

func openConditions(s JobState) []Condition {
	out := make([]Condition, len(s.Open))
	for i, o := range s.Open {
		out[i] = o.Condition
	}
	return out
}

// durationField reads a duration option of a definition, or the default
// when it is absent. A present null is an error, as JavaScript's
// parseDuration(null) throws.
func durationField(def Definition, key string, fallback float64) (float64, error) {
	v, ok := def.get(key)
	if !ok {
		return fallback, nil
	}
	return schedule.ParseDuration(v, key)
}

func graceMs(def Definition) (float64, error) { return durationField(def, "grace", defaultGraceMs) }

func timeoutMs(def Definition) (float64, error) {
	return durationField(def, "timeout", defaultTimeoutMs)
}

// slowThreshold is the slow threshold for a successful run, or ok false
// when there is nothing to compare against yet.
func slowThreshold(def Definition, history []Run) (thresholdMs float64, basis string, ok bool, err error) {
	if v, has := def.get("maxDuration"); has {
		ms, err := schedule.ParseDuration(v, "maxDuration")
		if err != nil {
			return 0, "", false, err
		}
		return ms, "maxDuration", true, nil
	}
	var durations []float64
	for _, r := range history {
		if r.Status == StatusOK && r.DurationMs != nil && len(durations) < baselineWindow {
			durations = append(durations, float64(*r.DurationMs))
		}
	}
	if len(durations) < baselineMinRuns {
		return 0, "", false, nil
	}
	p95, _ := percentile(durations, 95)
	return math.Max(2*p95, slowFloorMs), fmt.Sprintf("twice the p95 of the last %d runs (%s)", len(durations), schedule.FormatDuration(p95)), true, nil
}

// budgetBreaches are the run's metrics over their ceiling, or, without one,
// over three times the job's usual value.
func budgetBreaches(def Definition, run Run, history []Run) []BudgetBreach {
	breaches := []BudgetBreach{}
	budget, _ := get(def.o, "budget").(*js.Object)
	for _, m := range run.Metrics {
		if v, ok := budget.Get(m.Name); ok {
			if ceiling := jsNumber(v); m.Value > ceiling {
				breaches = append(breaches, BudgetBreach{Metric: m.Name, Value: m.Value, Limit: jsLimit(v), Basis: "budget"})
			}
			continue
		}
		var past []float64
		for _, r := range history {
			if len(past) >= baselineWindow {
				break
			}
			if r.Status != StatusOK {
				continue
			}
			if value, ok := r.Metrics.Get(m.Name); ok {
				past = append(past, value)
			}
		}
		if len(past) < baselineMinRuns {
			continue
		}
		usual, _ := median(past)
		if usual > 0 && m.Value > 3*usual {
			breaches = append(breaches, BudgetBreach{Metric: m.Name, Value: m.Value, Limit: 3 * usual, Basis: "three times the usual " + formatNumber(usual)})
		}
	}
	return breaches
}

// floorBreaches are the metrics of a successful run that fell below their
// floor. A metric with a floor breaches when it reports less than that
// floor. One without breaches when it reports 0 or less and either it did
// so on the job's last successful run too (previous, the metrics under
// their floor then), or the earlier successful runs that reported it (at
// least five, the newest twenty) all reported more than 0.
func floorBreaches(def Definition, run Run, history []Run, previous []string) []BudgetBreach {
	breaches := []BudgetBreach{}
	floor, _ := get(def.o, "floor").(*js.Object)
	for _, m := range run.Metrics {
		if v, ok := floor.Get(m.Name); ok {
			if m.Value < jsNumber(v) {
				breaches = append(breaches, BudgetBreach{Metric: m.Name, Value: m.Value, Limit: jsLimit(v), Basis: "floor"})
			}
			continue
		}
		if m.Value > 0 {
			continue
		}
		if slices.Contains(previous, m.Name) {
			breaches = append(breaches, BudgetBreach{Metric: m.Name, Value: m.Value, Limit: 0, Basis: "0 or less on the run before too"})
			continue
		}
		var past []float64
		for _, r := range history {
			if len(past) >= baselineWindow {
				break
			}
			if r.Status != StatusOK {
				continue
			}
			if value, ok := r.Metrics.Get(m.Name); ok {
				past = append(past, value)
			}
		}
		if len(past) < baselineMinRuns || slices.ContainsFunc(past, func(v float64) bool { return !(v > 0) }) {
			continue
		}
		lowest := slices.Min(past)
		breaches = append(breaches, BudgetBreach{Metric: m.Name, Value: m.Value, Limit: lowest,
			Basis: fmt.Sprintf("the last %d runs all reported more than 0, the lowest %s", len(past), formatNumber(lowest))})
	}
	return breaches
}

// jsLimit is a ceiling as it goes into an alert: the stored value when it is
// a number, which it is for every definition this package or the SDK writes.
func jsLimit(v any) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return jsNumber(v)
}

// jsNumber is JavaScript's Number(v) for a JSON value, as a comparison
// with > coerces one.
func jsNumber(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case nil:
		return 0
	case bool:
		if t {
			return 1
		}
		return 0
	case string:
		text := js.Trim(t)
		if text == "" {
			return 0
		}
		f, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return math.NaN()
		}
		return f
	}
	return math.NaN()
}

// hasFullBaseline reports whether history (newest first) holds a full
// baseline window of successful runs.
func hasFullBaseline(history []Run) bool {
	n := 0
	for _, r := range history {
		if r.Status == StatusOK {
			n++
		}
	}
	return n >= baselineWindow
}

// finishedAtOf is when a run finished, or nil while it runs (a run marked
// timeout has finished).
func finishedAtOf(run Run) *int64 {
	if run.Status == StatusRunning || run.FinishedAt == nil {
		return nil
	}
	return ptr(*run.FinishedAt)
}

// catchesUp is whether run brings the schedule up to date at now, so missed
// may close for it: judged with it as the job's last run, as onCheck
// judges, the run the schedule wants next is not yet past its deadline. A
// run that started before the fire missed opened for does not cover that
// fire, however late it finishes, and neither does a run a source reports
// only once it is over whose start was before it. Every run starting now
// covers it. A job with no schedule, or one that no longer parses, has
// nothing due, so any run does.
func catchesUp(def Definition, run Run, now int64) bool {
	if !truthy(get(def.o, "schedule")) {
		return true
	}
	parsed, err := parsedSchedule(def)
	if err != nil {
		return true
	}
	grace, err := graceMs(def)
	if err != nil {
		return true
	}
	exp, ok := schedule.Expect(parsed, ptr(run.StartedAt), run.StartedAt, grace, finishedAtOf(run))
	return !ok || float64(now) <= exp.Deadline
}

// onRunStart is called when a run starts, or when a run a source reports is
// recorded. Missed and stuck are about the absence of a run, so a run
// starting closes them without an alert; the recovered message waits for a
// successful finish. Missed closes only when the run catches the schedule
// up (catchesUp), which a run starting now always does.
func onRunStart(def Definition, run Run, state JobState, now int64) JobState {
	next := cloneState(state)
	if catchesUp(def, run, now) {
		closeCondition(&next, ConditionMissed)
	}
	closeCondition(&next, ConditionStuck)
	return next
}

// failuresBeforeAlert is Math.max(1, def.failuresBeforeAlert ?? 1).
func failuresBeforeAlert(def Definition) float64 {
	v, ok := def.get("failuresBeforeAlert")
	if !ok || v == nil {
		return 1
	}
	n := jsNumber(v)
	if math.IsNaN(n) {
		return n
	}
	return math.Max(1, n)
}

// onRunFinish is called when a run finishes with status ok, failed, or
// timeout. history is the job's earlier runs, newest first, not including
// this one. Missed closes only when the run catches the schedule up
// (catchesUp): a run that started before the fire missed opened for leaves
// it open.
func onRunFinish(def Definition, run Run, state JobState, history []Run, now int64) (evaluation, error) {
	next := cloneState(state)
	alerts := []alertDraft{}
	r := run.clone()

	if run.Status == StatusOK {
		next.ConsecutiveFailures = 0
		if catchesUp(def, run, now) {
			closeCondition(&next, ConditionMissed)
		}
		closeCondition(&next, ConditionStuck)
		closeCondition(&next, ConditionFailed)

		threshold, basis, ok, err := slowThreshold(def, history)
		if err != nil {
			return evaluation{}, err
		}
		if ok && run.DurationMs != nil && float64(*run.DurationMs) > threshold {
			if openCondition(&next, ConditionSlow, now) {
				alerts = append(alerts, alertDraft{AlertSlow, &r, SlowDetails{DurationMs: *run.DurationMs, ThresholdMs: threshold, Basis: basis}})
			}
		} else {
			closeCondition(&next, ConditionSlow)
		}

		if breaches := budgetBreaches(def, run, history); len(breaches) > 0 {
			if openCondition(&next, ConditionOverBudget, now) {
				alerts = append(alerts, alertDraft{AlertOverBudget, &r, OverBudgetDetails{Breaches: breaches}})
			}
		} else {
			closeCondition(&next, ConditionOverBudget)
		}

		if short := floorBreaches(def, run, history, next.UnderFloor); len(short) > 0 {
			next.setUnderFloor(short)
			if openCondition(&next, ConditionUnderFloor, now) {
				alerts = append(alerts, alertDraft{AlertUnderFloor, &r, UnderFloorDetails{Breaches: short}})
			}
		} else {
			next.setUnderFloor(nil)
			closeCondition(&next, ConditionUnderFloor)
		}

		if len(next.PendingRecovery) > 0 && len(next.Open) == 0 {
			after := append([]Condition{}, next.PendingRecovery...)
			alerts = append(alerts, alertDraft{AlertRecovered, &r, RecoveredDetails{After: after}})
			next.PendingRecovery = []Condition{}
		}
		return evaluation{next, alerts}, nil
	}

	// failed or timeout
	// Held at the top: a count at the limit neither loses precision nor wraps below a threshold.
	if next.ConsecutiveFailures < maxFailureCount {
		next.ConsecutiveFailures++
	}
	if catchesUp(def, run, now) {
		closeCondition(&next, ConditionMissed)
	}
	threshold := failuresBeforeAlert(def)
	condition, alertType := ConditionFailed, AlertFailed
	if run.Status == StatusTimeout {
		condition, alertType = ConditionStuck, AlertStuck
	}
	if float64(next.ConsecutiveFailures) >= threshold {
		if openCondition(&next, condition, now) {
			alerts = append(alerts, alertDraft{alertType, &r, failureDetails(next.ConsecutiveFailures, threshold)})
		}
	}
	return evaluation{next, alerts}, nil
}

// checkOutcome is what onCheck found besides the evaluation.
type checkOutcome struct {
	evaluation
	nextExpectedAt *int64
	dueAt          *int64
}

// truthy is JavaScript's truthiness of a JSON value.
func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case float64:
		return t != 0 && !math.IsNaN(t)
	case string:
		return t != ""
	}
	return true
}

// parsedSchedule is parseSchedule(def.schedule, def.timezone) for a stored
// definition, which may hold anything another writer put there.
func parsedSchedule(def Definition) (*schedule.Parsed, error) {
	text, ok := get(def.o, "schedule").(string)
	if !ok {
		return nil, errors.New("schedule.trim is not a function")
	}
	tz := ""
	if v, has := def.get("timezone"); has && v != nil {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("timezone %s is not an IANA timezone", js.Stringify(v))
		}
		tz = s
	}
	return schedule.Parse(text, tz)
}

// onCheck is called by a check. It decides whether the schedule has been
// missed: the run the schedule wants next (see schedule.Expect) has not
// started and its grace has run out. lastRun is the most recent run of any
// status. A job with no schedule is never missed, and one whose schedule
// was removed while missed was open gets a recovered alert (reason
// "unscheduled") for missed alone.
func onCheck(def Definition, stored StoredJob, lastRun *Run, state JobState, now int64) (checkOutcome, error) {
	next := cloneState(state)
	alerts := []alertDraft{}
	if !truthy(get(def.o, "schedule")) {
		if since, open := next.openAt(ConditionMissed); open {
			// The schedule went away while missed was open (the job was
			// declared again without one, or a source retired it), so
			// nothing is due any more. Missed closes now with a recovery of
			// its own; other open conditions keep their own rules. Missed is
			// taken out of the pending recovery too, so the next successful
			// run does not name it again.
			deleteOpen(&next, ConditionMissed)
			pending := []Condition{}
			for _, c := range next.PendingRecovery {
				if c != ConditionMissed {
					pending = append(pending, c)
				}
			}
			next.PendingRecovery = pending
			alerts = append(alerts, alertDraft{AlertRecovered, cloneRun(lastRun), RecoveredDetails{After: []Condition{ConditionMissed}, Reason: "unscheduled", Since: ptr(since)}})
		}
		return checkOutcome{evaluation: evaluation{next, alerts}}, nil
	}

	parsed, err := parsedSchedule(def)
	if err != nil {
		return checkOutcome{}, err
	}
	grace, err := graceMs(def)
	if err != nil {
		return checkOutcome{}, err
	}
	var lastRunAt *int64
	if lastRun != nil {
		lastRunAt = ptr(lastRun.StartedAt)
	}
	var lastFinishedAt *int64
	if lastRun != nil {
		lastFinishedAt = finishedAtOf(*lastRun)
	}
	exp, hasExp := schedule.Expect(parsed, lastRunAt, stored.CreatedAt, grace, lastFinishedAt)
	var nextAt int64
	var hasNext bool
	if parsed.Kind == "interval" {
		// An interval always has an expectation.
		nextAt, hasNext = exp.DueAt, true
	} else {
		nextAt, hasNext = schedule.NextFire(parsed, now, nil)
	}
	out := checkOutcome{}
	if hasNext {
		out.nextExpectedAt = ptr(nextAt)
	}
	if !hasExp {
		out.evaluation = evaluation{next, alerts}
		return out, nil
	}
	out.dueAt = ptr(exp.DueAt)

	// An interval's next run is due a period after the last one started, or
	// after it finished when it outlasted the period (schedule.IntervalDue).
	// If that run is still going, the job is busy, not late; stuck covers
	// one that never ends.
	if parsed.Kind == "interval" && lastRun != nil && lastRun.Status == StatusRunning {
		out.evaluation = evaluation{next, alerts}
		return out, nil
	}

	if float64(now) > exp.Deadline {
		if openCondition(&next, ConditionMissed, now) {
			alerts = append(alerts, alertDraft{AlertMissed, cloneRun(lastRun), MissedDetails{DueAt: exp.DueAt, Deadline: exp.Deadline, GraceMs: grace, LastRunAt: lastRunAt}})
		}
	} else {
		// A run has started since it opened, or the grace was widened.
		closeCondition(&next, ConditionMissed)
	}
	out.evaluation = evaluation{next, alerts}
	return out, nil
}

func cloneRun(r *Run) *Run {
	if r == nil {
		return nil
	}
	c := r.clone()
	return &c
}

// isStuck reports whether a running run has gone on longer than the job's timeout.
func isStuck(def Definition, run Run, now int64) (bool, error) {
	if run.Status != StatusRunning {
		return false, nil
	}
	timeout, err := timeoutMs(def)
	if err != nil {
		return false, err
	}
	return elapsedMs(run.StartedAt, now) > timeout, nil
}

// maxDurationMs is the longest duration written: 2^53 - 1, the largest
// integer JavaScript holds exactly, which every port and store reads back
// unchanged (the SDK's MAX_DURATION_MS).
const maxDurationMs = 9007199254740991

// silenceEnd is when a silence of ms milliseconds from now ends: a whole
// millisecond, never past maxDurationMs (2^53 - 1), however long the silence
// asked for, worked out without wrapping an int64. Every port sharing the
// store reads it back unchanged.
func silenceEnd(now int64, ms float64) int64 {
	d := int64(math.Floor(min(ms, maxDurationMs)))
	if now >= maxDurationMs-d {
		return maxDurationMs
	}
	return now + d
}

// runDuration is how long a run took, from startedAt to finishedAt: 0 when
// it started later, and never more than maxDurationMs (the SDK's
// runDuration). A foreign row's start near a 64-bit limit must not wrap, or
// make a duration no store can write.
func runDuration(startedAt, finishedAt int64) int64 {
	if finishedAt <= startedAt {
		return 0
	}
	// Exact: the difference of two int64s, the larger first, fits a uint64.
	return int64(min(uint64(finishedAt)-uint64(startedAt), maxDurationMs))
}

// elapsedMs is to minus from as the SDK computes it, in float64, so a
// foreign time near a 64-bit limit cannot wrap.
func elapsedMs(from, to int64) float64 { return float64(to) - float64(from) }

// muteOpens is next with nothing opened that was not open in previous.
// While a job is silenced nothing new is recorded as an incident:
// conditions may close (so a job that recovered during the silence shows
// as healthy) but none may open, so the first problem after the silence
// ends alerts normally.
func muteOpens(previous, next JobState) JobState {
	muted := cloneState(next)
	kept := []OpenCondition{}
	for _, o := range muted.Open {
		if _, was := previous.openAt(o.Condition); was {
			kept = append(kept, o)
		}
	}
	muted.Open = kept
	return muted
}

func isSilenced(state JobState, now int64) bool {
	return state.SilencedUntil != nil && *state.SilencedUntil > now
}

// applySilence is an evaluation as it is saved and sent: while the job was
// silenced when it began, nothing opens and no new alert is sent, but a
// recovery is. A condition can only close during a silence if it opened
// (and alerted) before it, so its recovery closes an incident someone was
// told about.
func applySilence(previous JobState, e evaluation, now int64) evaluation {
	if !isSilenced(previous, now) {
		return e
	}
	recoveries := []alertDraft{}
	for _, a := range e.alerts {
		if a.Type == AlertRecovered {
			recoveries = append(recoveries, a)
		}
	}
	return evaluation{muteOpens(previous, e.state), recoveries}
}

// staleAlert reports whether an alert waiting to be retried no longer
// describes the job, so it is dropped rather than sent late. An alert for a
// condition is stale once that condition has closed, or has closed and
// opened again (it opened at a time other than the alert's). A recovery is
// stale when any condition it names is open again; while they all stay
// closed it is kept. From a foreign or damaged row: an alert whose at is
// not a number, and a recovery whose details.after is not a list of
// strings, are stale.
func staleAlert(alert Alert, state JobState) bool {
	if alert.malformed {
		return true
	}
	if alert.Type == AlertRecovered {
		d, _ := alert.Details.(RecoveredDetails)
		for _, c := range d.After {
			if _, open := state.openAt(c); open {
				return true
			}
		}
		return false
	}
	at, open := state.openAt(Condition(alert.Type))
	return !open || at != alert.At
}

// jobHealth is how a job looks at a glance. Silence wins, then stuck,
// failing, and late.
func jobHealth(def Definition, lastRun *Run, state JobState, now int64) (JobHealth, error) {
	open := openConditions(state)
	if isSilenced(state, now) {
		return HealthSilenced, nil
	}
	if hasCondition(open, ConditionStuck) {
		return HealthStuck, nil
	}
	if lastRun != nil {
		stuck, err := isStuck(def, *lastRun, now)
		if err != nil {
			return "", err
		}
		if stuck {
			return HealthStuck, nil
		}
	}
	if hasCondition(open, ConditionFailed) || (lastRun != nil && (lastRun.Status == StatusFailed || lastRun.Status == StatusTimeout)) {
		return HealthFailing, nil
	}
	if hasCondition(open, ConditionMissed) {
		return HealthLate, nil
	}
	if lastRun == nil {
		return HealthNeverRan, nil
	}
	return HealthHealthy, nil
}

// summarize is a job's summary from its most recent runs (newest first;
// the first baselineWindow are used) and its state. Stats cover runs of any
// status; the percentiles are over the successful ones among them.
func summarize(stored StoredJob, recent []Run, state JobState, nextExpectedAt *int64, now int64) (JobSummary, error) {
	var herr error
	s := summary(stored, recent, state, nextExpectedAt, func(lastRun *Run) JobHealth {
		h, err := jobHealth(stored.Definition, lastRun, state, now)
		herr = err
		return h
	})
	return s, herr
}

// unevaluableSummary is the summary of a job that could not be evaluated,
// say because its stored schedule no longer parses. It reads nothing from
// the definition. The job shows as failing (or silenced, while it is),
// since it needs a look, and nothing is known about when it is next due.
func unevaluableSummary(stored StoredJob, recent []Run, state JobState, now int64) JobSummary {
	return summary(stored, recent, state, nil, func(*Run) JobHealth {
		if isSilenced(state, now) {
			return HealthSilenced
		}
		return HealthFailing
	})
}

func summary(stored StoredJob, recent []Run, state JobState, nextExpectedAt *int64, health func(*Run) JobHealth) JobSummary {
	window := recent
	if len(window) > baselineWindow {
		window = window[:baselineWindow]
	}
	var lastRun *Run
	if len(window) > 0 {
		lastRun = cloneRun(&window[0])
	}
	finished, ok := 0, 0
	var okDurations []float64
	for _, r := range window {
		if r.Status != StatusRunning {
			finished++
		}
		if r.Status == StatusOK {
			ok++
			if r.DurationMs != nil {
				okDurations = append(okDurations, float64(*r.DurationMs))
			}
		}
	}
	okRate := 1.0
	if finished > 0 {
		okRate = float64(ok) / float64(finished)
	}
	return JobSummary{
		Name:                stored.Name,
		Definition:          stored.Definition,
		Health:              health(lastRun),
		Open:                openConditions(state),
		LastRun:             lastRun,
		NextExpectedAt:      copyInt(nextExpectedAt),
		ConsecutiveFailures: state.ConsecutiveFailures,
		SilencedUntil:       copyInt(state.SilencedUntil),
		Stats:               Stats{Runs: finished, OkRate: okRate, P50Ms: percentileInt(okDurations, 50), P95Ms: percentileInt(okDurations, 95)},
	}
}

func percentileInt(values []float64, p float64) *int64 {
	v, ok := percentile(values, p)
	if !ok {
		return nil
	}
	n := int64(v)
	return &n
}
