package cronwatch

// conformance/health.json: health, summaries, percentiles, state
// normalization, silence, and which queued alerts a retry drops.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"testing"

	"cronwatch.dev/go/internal/js"
	"cronwatch.dev/go/internal/schedule"
)

func fixtureRun(t *testing.T, v any) *Run {
	if v == nil {
		return nil
	}
	r, err := runFrom(v)
	if err != nil {
		t.Fatal(err)
	}
	return &r
}

func fixtureRuns(t *testing.T, v any) []Run {
	out := []Run{}
	list, _ := v.([]any)
	for _, r := range list {
		out = append(out, *fixtureRun(t, r))
	}
	return out
}

func fixtureState(t *testing.T, v any) JobState {
	s, err := stateFrom(v)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func fixtureStored(t *testing.T, v any) StoredJob {
	o := v.(*js.Object)
	def, err := definitionFrom(field(o, "definition"))
	if err != nil {
		t.Fatal(err)
	}
	return StoredJob{Name: field(o, "name").(string), Definition: def, CreatedAt: int64(field(o, "createdAt").(float64)), UpdatedAt: int64(field(o, "updatedAt").(float64))}
}

// sorted is a JSON value with every object's keys in order, for comparing
// alerts, whose key order is the fixture's own and not a writer's.
func sorted(v any) any {
	switch t := v.(type) {
	case *js.Object:
		keys := append([]string(nil), t.Keys()...)
		sort.Strings(keys)
		out := &js.Object{}
		for _, k := range keys {
			x, _ := t.Get(k)
			out.Set(k, sorted(x))
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = sorted(e)
		}
		return out
	}
	return v
}

// sameState compares a delivery result: the state's own keys in the
// fixture's order (the writer controls those), and everything in it
// whatever its key order.
func sameState(t *testing.T, what string, state JobState, dropped int, want any) {
	t.Helper()
	w := want.(*js.Object)
	got := js.NewObject("state", state.JSValue(), "dropped", dropped)
	if sameJSON(t, what, sorted(got), sorted(w)) {
		gotKeys := state.JSValue().(*js.Object).Keys()
		wantKeys := field(w, "state").(*js.Object).Keys()
		sameJSON(t, what+" key order", gotKeys, wantKeys)
	}
}

func fixtureAlerts(t *testing.T, v any) []Alert {
	out := []Alert{}
	list, _ := v.([]any)
	for _, a := range list {
		alert, err := alertFrom(a)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, alert)
	}
	return out
}

func TestConformanceDelivery(t *testing.T) {
	f := field(fixture(t, "health"), "delivery").(*js.Object)
	if int(field(f, "maxUndelivered").(float64)) != maxUndelivered || int64(field(f, "sendLeaseMs").(float64)) != sendLeaseMs {
		t.Fatalf("constants: %v %v", field(f, "maxUndelivered"), field(f, "sendLeaseMs"))
	}
	count := 0
	for i, c := range objects(f, "alertKey") {
		a, err := alertFrom(field(c, "alert"))
		if err != nil {
			t.Fatal(err)
		}
		if got := alertKey(a); got != field(c, "key") {
			t.Errorf("alertKey %d: %q, want %q", i, got, field(c, "key"))
		}
		sameJSON(t, fmt.Sprintf("alertKey %d written back", i), sorted(a.JSValue()), sorted(field(c, "alert")))
		count++
	}
	for i, c := range objects(f, "normalizeState") {
		s := normalizeState(ptr(fixtureState(t, field(c, "state"))), "j")
		sameJSON(t, fmt.Sprintf("normalizeState %d", i), sorted(s.JSValue()), sorted(field(c, "normalized")))
		sameJSON(t, fmt.Sprintf("normalizeState %d key order", i), s.JSValue().(*js.Object).Keys(), field(c, "normalized").(*js.Object).Keys())
		count++
	}
	for i, c := range objects(f, "queueUndelivered") {
		s, dropped := queueUndelivered(fixtureState(t, field(c, "state")), fixtureAlerts(t, field(c, "alerts")))
		sameState(t, fmt.Sprintf("queueUndelivered %d", i), s, dropped, field(c, "result"))
		count++
	}
	for i, c := range objects(f, "holdAlerts") {
		s, dropped := holdAlerts(fixtureState(t, field(c, "state")), fixtureAlerts(t, field(c, "alerts")), int64(field(c, "until").(float64)), field(c, "deferred").(bool))
		sameState(t, fmt.Sprintf("holdAlerts %d", i), s, dropped, field(c, "result"))
		count++
	}
	for i, c := range objects(f, "releaseSending") {
		s, dropped := releaseSending(fixtureState(t, field(c, "state")), caseNow(c))
		sameState(t, fmt.Sprintf("releaseSending %d", i), s, dropped, field(c, "result"))
		count++
	}
	for i, c := range objects(f, "recordSent") {
		delivered := fixtureAlerts(t, field(c, "delivered"))
		// reached, when not given, is the SDK's default: some alert delivered.
		reached := len(delivered) > 0
		if r, ok := field(c, "reached").(bool); ok {
			reached = r
		}
		s, dropped := recordSent(fixtureState(t, field(c, "state")), delivered, fixtureAlerts(t, field(c, "failed")),
			fixtureAlerts(t, field(c, "stale")), caseNow(c), reached)
		sameState(t, fmt.Sprintf("recordSent %d", i), s, dropped, field(c, "result"))
		count++
	}
	if int(field(f, "maxPendingRuns").(float64)) != maxPendingRuns {
		t.Errorf("maxPendingRuns: %v", field(f, "maxPendingRuns"))
	}
	alertList := func(alerts []Alert) []any {
		out := []any{}
		for _, a := range alerts {
			out = append(out, a.JSValue())
		}
		return out
	}
	sameStateKeys := func(what string, state JobState, want any) {
		t.Helper()
		if sameJSON(t, what, sorted(state.JSValue()), sorted(want)) {
			sameJSON(t, what+" key order", state.JSValue().(*js.Object).Keys(), want.(*js.Object).Keys())
		}
	}
	for i, c := range objects(f, "claimUndelivered") {
		s, claimed, stale := claimUndelivered(fixtureState(t, field(c, "state")), caseNow(c), int64(field(c, "until").(float64)))
		result := field(c, "result").(*js.Object)
		sameStateKeys(fmt.Sprintf("claimUndelivered %d", i), s, field(result, "state"))
		sameJSON(t, fmt.Sprintf("claimUndelivered %d claimed", i), sorted(alertList(claimed)), sorted(field(result, "claimed")))
		sameJSON(t, fmt.Sprintf("claimUndelivered %d stale", i), sorted(alertList(stale)), sorted(field(result, "stale")))
		count++
	}
	for i, c := range objects(f, "holdRun") {
		s := holdRun(fixtureState(t, field(c, "state")), field(c, "runId").(string))
		sameStateKeys(fmt.Sprintf("holdRun %d", i), s, field(c, "result"))
		count++
	}
	for i, c := range objects(f, "releaseRun") {
		s := releaseRun(fixtureState(t, field(c, "state")), field(c, "runId").(string))
		sameStateKeys(fmt.Sprintf("releaseRun %d", i), s, field(c, "result"))
		count++
	}
	count += heldCases(t, f)
	if count < 48 {
		t.Errorf("only %d delivery cases", count)
	}
	t.Logf("%d delivery cases", count)
}

// expand is the fixture's recipe for long text: a string, or
// { parts: [[piece, times], ...] } joined.
func expand(spec any) string {
	if s, ok := spec.(string); ok {
		return s
	}
	var b strings.Builder
	parts, _ := field(spec.(*js.Object), "parts").([]any)
	for _, p := range parts {
		pair := p.([]any)
		b.WriteString(strings.Repeat(pair[0].(string), int(pair[1].(float64))))
	}
	return b.String()
}

// digest is the fixture's form of a text: itself up to 400 UTF-16 code
// units, else its length and the SHA-256 of its UTF-8.
func digest(s string) any {
	if js.Length16(s) <= 400 {
		return js.NewObject("text", s)
	}
	sum := sha256.Sum256([]byte(s))
	return js.NewObject("length", js.Length16(s), "sha256", hex.EncodeToString(sum[:]))
}

// heldDigest is a held run as the fixture writes it: its error and output
// as digest writes text, or {value: null} for none.
func heldDigest(run any) any {
	o := run.(*js.Object).Clone()
	for _, k := range []string{"error", "output"} {
		v, _ := o.Get(k)
		if text, ok := v.(string); ok {
			o.Set(k, digest(text))
		} else {
			o.Set(k, js.NewObject("value", v))
		}
	}
	return o
}

// textSpec is a fixture's text spec as the value it stands for: null, a
// string, a {parts} recipe, or {value} for what is not a string.
func textSpec(spec any) (any, bool) {
	switch v := spec.(type) {
	case nil:
		return nil, true
	case string:
		return v, true
	case *js.Object:
		if v.Has("parts") {
			return expand(v), true
		}
		x, _ := v.Get("value")
		return x, false
	}
	return spec, false
}

func strings16(v any) []string {
	out := []string{}
	list, _ := v.([]any)
	for _, s := range list {
		out = append(out, s.(string))
	}
	return out
}

// heldCases are the held copies' trims and the per-channel bookkeeping.
func heldCases(t *testing.T, f *js.Object) int {
	t.Helper()
	if int(field(f, "heldTextCap").(float64)) != heldTextCap || int(field(f, "heldTextSlack").(float64)) != heldTextSlack ||
		int(field(f, "heldMessageCap").(float64)) != heldMessageCap {
		t.Fatalf("held constants: %v %v %v", field(f, "heldTextCap"), field(f, "heldTextSlack"), field(f, "heldMessageCap"))
	}
	count := 0
	base := func(c *js.Object) *js.Object {
		// The fixture's run, error and output aside, is its result's.
		return field(c, "result").(*js.Object).Clone()
	}
	for i, c := range objects(f, "heldRun") {
		errorValue, errorText := textSpec(field(c, "error"))
		outputValue, outputText := textSpec(field(c, "output"))
		if (!errorText && errorValue != nil) || (!outputText && outputValue != nil) {
			// A foreign row's error or output that is not a string: a Run
			// holds only strings, so it is read as null, as it always was.
			continue
		}
		o := base(c)
		o.Set("error", errorValue)
		o.Set("output", outputValue)
		run := fixtureRun(t, o)
		sameJSON(t, fmt.Sprintf("heldRun %d", i), heldDigest(heldRun(*run).JSValue()), field(c, "result"))
		count++
	}
	for i, c := range objects(f, "heldAlert") {
		in := field(c, "alert").(*js.Object).Clone()
		if m, ok := in.Get("message"); ok {
			text, _ := textSpec(m)
			in.Set("message", text)
		}
		a, err := alertFrom(in)
		if err != nil {
			t.Fatal(err)
		}
		got := heldAlert(a).JSValue().(*js.Object)
		if r, ok := got.Get("run"); ok {
			if _, isObject := r.(*js.Object); isObject {
				got.Set("run", heldDigest(r))
			}
		}
		if m, ok := field(got, "message").(string); ok {
			got.Set("message", digest(m))
		}
		sameJSON(t, fmt.Sprintf("heldAlert %d", i), sorted(got), sorted(field(c, "result")))
		count++
	}
	for i, c := range objects(f, "channelKeys") {
		sameJSON(t, fmt.Sprintf("channelKeys %d", i), channelKeys(strings16(field(c, "names"))), field(c, "keys"))
		count++
	}
	for i, c := range objects(f, "owedChannels") {
		a := fixtureAlerts(t, []any{field(c, "alert")})[0]
		sameJSON(t, fmt.Sprintf("owedChannels %d", i), owedChannels(a, strings16(field(c, "configured"))), field(c, "owed"))
		count++
	}
	for i, c := range objects(f, "afterSend") {
		a := fixtureAlerts(t, []any{field(c, "alert")})[0]
		left, owes := afterSend(a, strings16(field(c, "tried")), strings16(field(c, "failed")))
		var got any
		if owes {
			got = sorted(left.JSValue())
		}
		sameJSON(t, fmt.Sprintf("afterSend %d", i), got, sorted(field(c, "result")))
		count++
	}
	for i, c := range objects(f, "withoutOwed") {
		a := fixtureAlerts(t, []any{field(c, "alert")})[0]
		sameJSON(t, fmt.Sprintf("withoutOwed %d", i), sorted(withoutOwed(a).JSValue()), sorted(field(c, "result")))
		count++
	}
	return count
}

func caseNow(c *js.Object) int64 { return int64(field(c, "now").(float64)) }

func numbers(v any) []float64 {
	out := []float64{}
	for _, n := range v.([]any) {
		out = append(out, n.(float64))
	}
	return out
}

func TestConformanceHealth(t *testing.T) {
	f := fixture(t, "health")
	now := func(c *js.Object) int64 { return int64(field(c, "now").(float64)) }
	for i, c := range objects(f, "jobHealth") {
		def, _ := definitionFrom(field(c, "definition"))
		h, err := jobHealth(def, fixtureRun(t, field(c, "lastRun")), fixtureState(t, field(c, "state")), now(c))
		if err != nil || string(h) != field(c, "health") {
			t.Errorf("jobHealth %d: %s %v, want %v", i, h, err, field(c, "health"))
		}
	}
	for i, c := range objects(f, "summarize") {
		s, err := summarize(fixtureStored(t, field(c, "stored")), fixtureRuns(t, field(c, "recent")), fixtureState(t, field(c, "state")), optInt(field(c, "nextExpectedAt")), now(c))
		if err != nil {
			t.Fatal(err)
		}
		sameJSON(t, fmt.Sprintf("summarize %d", i), s.JSValue(), field(c, "summary"))
	}
	for _, c := range objects(f, "percentile") {
		v, ok := percentile(numbers(field(c, "values")), field(c, "p").(float64))
		var got any
		if ok {
			got = v
		}
		sameJSON(t, fmt.Sprintf("percentile(%s, %v)", js.Stringify(field(c, "values")), field(c, "p")), got, field(c, "percentile"))
	}
	for _, c := range objects(f, "median") {
		v, ok := median(numbers(field(c, "values")))
		var got any
		if ok {
			got = v
		}
		sameJSON(t, "median", got, field(c, "median"))
	}
	// Any JSON value, as a store may hold: one that is not an object reads
	// as no state (readState), and the fields of one are read leniently.
	normalized := 0
	for i, c := range objects(f, "normalizeState") {
		sameJSON(t, fmt.Sprintf("normalizeState %d", i), sorted(normalizeState(readState(field(c, "state")), "j").JSValue()), sorted(field(c, "normalized")))
		sameJSON(t, fmt.Sprintf("normalizeState %d key order", i), normalizeState(readState(field(c, "state")), "j").JSValue().(*js.Object).Keys(), field(c, "normalized").(*js.Object).Keys())
		normalized++
	}
	if normalized < 10 {
		t.Errorf("only %d normalizeState cases", normalized)
	}
	for i, c := range objects(f, "muteOpens") {
		got := muteOpens(fixtureState(t, field(c, "previous")), fixtureState(t, field(c, "next")))
		sameJSON(t, fmt.Sprintf("muteOpens %d", i), got.JSValue(), field(c, "muted"))
	}
	for i, c := range objects(f, "isStuck") {
		def, _ := definitionFrom(field(c, "definition"))
		stuck, err := isStuck(def, *fixtureRun(t, field(c, "run")), now(c))
		if err != nil || stuck != field(c, "stuck") {
			t.Errorf("isStuck %d: %v %v", i, stuck, err)
		}
	}
	durations := 0
	for i, c := range objects(f, "runDuration") {
		got := runDuration(int64(field(c, "startedAt").(float64)), int64(field(c, "finishedAt").(float64)))
		if want := int64(field(c, "durationMs").(float64)); got != want {
			t.Errorf("runDuration %d: %d, want %d", i, got, want)
		}
		durations++
	}
	versions := 0
	for i, c := range objects(f, "stateVersion") {
		parsed, err := js.Parse(field(c, "state").(string))
		if err != nil {
			t.Fatal(err)
		}
		// The state reads (a foreign version reads as none), and counts as the SDK counts it.
		s := fixtureState(t, parsed)
		if want := int64(field(c, "version").(float64)); s.version() != want {
			t.Errorf("stateVersion %d (%s): %d, want %d", i, field(c, "state"), s.version(), want)
		}
		versions++
	}
	if durations == 0 || versions == 0 {
		t.Error("no runDuration or stateVersion cases")
	}
	// The failures in a row a foreign state counts as, read as a store reads
	// it, then a failed run from it: held at 2^53 - 1, never negative.
	failedDef, _ := definitionFrom(js.NewObject("name", "j", "failuresBeforeAlert", 3.0))
	t0 := int64(1767605400000)
	failedRun := Run{ID: "f", Job: "j", Status: StatusFailed, StartedAt: t0 - 60000, FinishedAt: ptr(t0 - 59000), DurationMs: ptr(int64(1000)),
		Error: ptr("Error: boom"), Metrics: Metrics{}, Trigger: "run"}
	counts := 0
	for i, c := range objects(f, "failureCount") {
		parsed, err := js.Parse(field(c, "state").(string))
		if err != nil {
			t.Fatal(err)
		}
		s := normalizeState(ptr(fixtureState(t, parsed)), "j")
		if want := int(field(c, "consecutiveFailures").(float64)); s.ConsecutiveFailures != want {
			t.Errorf("failureCount %d (%s): %d, want %d", i, field(c, "state"), s.ConsecutiveFailures, want)
		}
		e, err := onRunFinish(failedDef, failedRun, s, nil, t0)
		if err != nil {
			t.Fatal(err)
		}
		alerts := []any{}
		for _, d := range e.alerts {
			alerts = append(alerts, d.JSValue())
		}
		sameJSON(t, fmt.Sprintf("failureCount %d failed", i), js.NewObject("state", e.state.JSValue(), "alerts", alerts), field(c, "failed"))
		counts++
	}
	if counts == 0 {
		t.Error("no failureCount cases")
	}
	for i, c := range objects(f, "unevaluableSummary") {
		s := unevaluableSummary(fixtureStored(t, field(c, "stored")), fixtureRuns(t, field(c, "recent")), fixtureState(t, field(c, "state")), now(c))
		sameJSON(t, fmt.Sprintf("unevaluableSummary %d", i), s.JSValue(), field(c, "summary"))
	}
	for i, c := range objects(f, "applySilence") {
		e := field(c, "evaluation").(*js.Object)
		in := evaluation{state: fixtureState(t, field(e, "state"))}
		for _, d := range field(e, "alerts").([]any) {
			in.alerts = append(in.alerts, draftFrom(t, d))
		}
		out := applySilence(fixtureState(t, field(c, "previous")), in, now(c))
		alerts := []any{}
		for _, d := range out.alerts {
			alerts = append(alerts, d.JSValue())
		}
		sameJSON(t, fmt.Sprintf("applySilence %d", i), js.NewObject("state", out.state.JSValue(), "alerts", alerts), field(c, "result"))
	}
	ends := objects(f, "silenceEnd")
	if len(ends) == 0 {
		t.Error("no silenceEnd cases")
	}
	for i, c := range ends {
		ms, err := schedule.ParseDuration(field(c, "duration"), "silence duration")
		if err != nil {
			t.Fatalf("silenceEnd %d: %v", i, err)
		}
		if got, want := silenceEnd(now(c), ms), int64(field(c, "silencedUntil").(float64)); got != want {
			t.Errorf("silenceEnd %d (%v from %d): %d, want %d", i, field(c, "duration"), now(c), got, want)
		}
	}
	stale := 0
	for i, c := range objects(f, "staleAlert") {
		a, err := alertFrom(field(c, "alert"))
		if err != nil {
			t.Fatal(err)
		}
		if got := staleAlert(a, fixtureState(t, field(c, "state"))); got != field(c, "stale") {
			t.Errorf("staleAlert %d: %v", i, got)
		}
		// One no state can match is written back as it was read, until a
		// retry drops it.
		if a.malformed {
			sameJSON(t, fmt.Sprintf("staleAlert %d written back", i), sorted(a.JSValue()), sorted(field(c, "alert")))
		}
		alertKey(a)
		stale++
	}
	if stale < 14 {
		t.Errorf("only %d staleAlert cases", stale)
	}
}
