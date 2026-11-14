package bridge

import (
	"context"
	"errors"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"

	cronwatch "cronwatch.dev/go"
	"cronwatch.dev/go/internal/js"
)

// wholeFailures is a stored failuresBeforeAlert as a count that alerts on
// the same failure: a job alerts once its failures reach the threshold, and
// failures are whole, so 2.5 (another port's, or a hand edit) alerts on the
// third, as 3 does. Cut down to 2 it would alert one failure early.
func wholeFailures(n float64) int {
	if math.IsNaN(n) || n < math.MinInt32 || n > math.MaxInt32 {
		return int(n)
	}
	return int(math.Ceil(n))
}

// OptionsOf are the job options that declare a stored definition again, in
// its order: schedule, timezone, grace, timeout, maxDuration, budget,
// floor, failuresBeforeAlert, description, tags, and expect ("contains" as
// Expect, a pattern Go wrote as ExpectMatch, a custom function as one that
// passes every output, since the function is the other process's). Fields
// no option gives are left out.
func OptionsOf(def cronwatch.Definition) []cronwatch.JobOption {
	var options []cronwatch.JobOption
	raw, _ := js.ValueOf(def).(*js.Object)
	for _, key := range def.Keys() {
		value, _ := def.Get(key)
		switch key {
		case "schedule":
			if s, ok := value.(string); ok {
				options = append(options, cronwatch.Schedule(s))
			}
		case "timezone":
			if s, ok := value.(string); ok {
				options = append(options, cronwatch.Timezone(s))
			}
		case "grace":
			options = appendDuration(options, cronwatch.Grace[string], cronwatch.Grace[float64], value)
		case "timeout":
			options = appendDuration(options, cronwatch.Timeout[string], cronwatch.Timeout[float64], value)
		case "maxDuration":
			options = appendDuration(options, cronwatch.MaxDuration[string], cronwatch.MaxDuration[float64], value)
		case "budget":
			if raw == nil {
				continue
			}
			v, _ := raw.Get("budget")
			if budget, ok := v.(*js.Object); ok {
				for _, metric := range budget.Keys() {
					m, _ := budget.Get(metric)
					if ceiling, ok := m.(float64); ok {
						options = append(options, cronwatch.Budget(metric, ceiling))
					}
				}
			}
		case "floor":
			if raw == nil {
				continue
			}
			v, _ := raw.Get("floor")
			if floor, ok := v.(*js.Object); ok {
				for _, metric := range floor.Keys() {
					m, _ := floor.Get(metric)
					if limit, ok := m.(float64); ok {
						options = append(options, cronwatch.Floor(metric, limit))
					}
				}
			}
		case "failuresBeforeAlert":
			if n, ok := value.(float64); ok {
				options = append(options, cronwatch.FailuresBeforeAlert(wholeFailures(n)))
			}
		case "description":
			if s, ok := value.(string); ok {
				options = append(options, cronwatch.Description(s))
			}
		case "tags":
			options = append(options, cronwatch.Tags(def.Tags()...))
		case "expect":
			if option := expectOf(value); option != nil {
				options = append(options, option)
			}
		}
	}
	return options
}

// expectOf is the expect option a stored description came from.
func expectOf(value any) cronwatch.JobOption {
	text, _ := value.(string)
	switch {
	case strings.HasPrefix(text, "contains "):
		if s, err := js.Parse(strings.TrimPrefix(text, "contains ")); err == nil {
			if want, ok := s.(string); ok {
				return cronwatch.Expect(want)
			}
		}
	case strings.HasPrefix(text, "matches /") && strings.HasSuffix(text, "/"):
		if re, err := regexp.Compile(strings.TrimSuffix(strings.TrimPrefix(text, "matches /"), "/")); err == nil {
			return cronwatch.ExpectMatch(re)
		}
	case text == "custom function":
		return cronwatch.ExpectFunc(func(string) bool { return true })
	}
	return nil
}

// The waits, variables only so the package's own tests can shorten them.
var (
	// lookupTimeout bounds a read of the store made before a job runs
	// (LookupContext).
	lookupTimeout = 5 * time.Second
	// fallbackRefresh is how long a job declared from the store's
	// definition is kept before the store is read again.
	fallbackRefresh = time.Minute
	// lookupRetry is how long after a read ran out of time the next is
	// made: until then a job not declared yet goes without one.
	lookupRetry = 15 * time.Second
)

// LookupContext is the context a worker's middleware reads the store with
// before the work runs: ctx's values without its cancellation, ending
// after five seconds, or half of what ctx has left when that is less. So a
// store that never answers costs an attempt a moment, never its whole
// deadline, and the work still gets ctx itself, with time left on it.
func LookupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	limit := lookupTimeout
	if deadline, ok := ctx.Deadline(); ok {
		limit = min(limit, max(time.Until(deadline)/2, 0))
	}
	return context.WithTimeout(context.WithoutCancel(ctx), limit)
}

// errLookupSkipped is a read not made: one a moment ago ran out of time,
// or another caller's read of the same name did not end in this one's time.
var errLookupSkipped = errors.New("the store was not read")

// lookup is a read of one name's stored job under way, which callers
// asking for that name meanwhile share.
type lookup struct {
	done chan struct{}
	def  *cronwatch.Definition
	err  error
}

// fallbackJob is a job Fallback declared: the definition it was declared
// with (key, its JSON), and when the store was last read for it.
type fallbackJob struct {
	job     *cronwatch.Job
	key     string
	checked time.Time
}

// stored is the definition the store holds for name when it is this app's
// (tagged with its app tag), or nil. One read at a time per name: callers
// asking meanwhile wait for it, each no longer than its own ctx. After a
// read that ran out of time, none is made for lookupRetry
// (errLookupSkipped), so a store that hangs holds each caller once, not
// every caller for its whole timeout.
func (w *Watch) stored(ctx context.Context, name string) (*cronwatch.Definition, error) {
	w.mu.Lock()
	if time.Now().Before(w.lookupsFrom) {
		w.mu.Unlock()
		return nil, errLookupSkipped
	}
	if l, ok := w.lookups[name]; ok {
		w.mu.Unlock()
		select {
		case <-l.done:
			if l.err != nil {
				// Reported by the caller that made the read.
				return nil, errLookupSkipped
			}
			return l.def, nil
		case <-ctx.Done():
			return nil, errLookupSkipped
		}
	}
	l := &lookup{done: make(chan struct{}), err: errLookupSkipped}
	w.lookups[name] = l
	w.mu.Unlock()
	// Ended whatever happens (a store's panic carries on up the stack), so
	// no caller waits on it for good.
	defer func() {
		w.mu.Lock()
		delete(w.lookups, name)
		if l.err != nil && ctx.Err() != nil {
			w.lookupsFrom = time.Now().Add(lookupRetry)
		}
		w.mu.Unlock()
		close(l.done)
	}()
	summary, err := w.cw.JobSummary(ctx, name)
	if err != nil {
		l.err = err
		return nil, err
	}
	l.err = nil
	if summary != nil && slices.Contains(summary.Definition.Tags(), w.appTag) {
		l.def = &summary.Definition
	}
	return l.def, nil
}

// Fallback is the job a run in this process belongs to when this process
// has not declared it from a scheduler of its own (a worker whose app
// schedules the job in another process): declared again from the
// definition the store holds, when that is this app's (tagged with its app
// tag), so the schedule another process stored is kept, else with options
// and this watch's tags. The store is read again once a minute, and the job
// declared again when the definition it holds has changed (the scheduling
// process was deployed with other options), so a worker never judges runs
// by a definition it read long ago. Nil, with the reason reported, when
// the client refuses it or the store cannot be read (the run then goes
// unrecorded, and the next one asks again), since a declaration made
// without the stored one would overwrite its schedule; a job already
// declared is kept while the store cannot be read. A job Declare has
// declared meanwhile is Declare's. Give it a context that ends soon
// (LookupContext): it reads the store before the work runs.
func (w *Watch) Fallback(ctx context.Context, name string, options []cronwatch.JobOption) *cronwatch.Job {
	return w.fromStore(ctx, name, options, true)
}

// Stored is Fallback for a name that is a job only when the store holds
// it as this app's (an Asynq task type a scheduler in another process
// declared): nil, with nothing declared and nothing reported, when it does
// not.
func (w *Watch) Stored(ctx context.Context, name string) *cronwatch.Job {
	return w.fromStore(ctx, name, nil, false)
}

func (w *Watch) fromStore(ctx context.Context, name string, options []cronwatch.JobOption, always bool) *cronwatch.Job {
	w.mu.Lock()
	if d, ok := w.jobs[name]; ok {
		w.mu.Unlock()
		return d.job
	}
	// Not one forgotten since (the dashboard's forget): that is made again.
	held, ok := w.fallback[name]
	if ok && !w.cw.Declares(name) {
		delete(w.fallback, name)
		held, ok = nil, false
	}
	if ok && time.Since(held.checked) < fallbackRefresh {
		w.mu.Unlock()
		return held.job
	}
	w.mu.Unlock()
	// Read outside the turn Declare takes, so a store slow to answer for
	// one name holds no other's declaration.
	def, err := w.stored(ctx, name)
	if err != nil {
		if !errors.Is(err, errLookupSkipped) {
			w.cw.ReportError(err, "declaring "+name)
		}
		if !ok {
			return nil
		}
		// The job as it was last read, asked for again in a while.
		w.mu.Lock()
		held.checked = time.Now()
		w.mu.Unlock()
		return held.job
	}
	var made []cronwatch.JobOption
	switch {
	case def != nil:
		made = OptionsOf(*def)
	case always:
		made = w.tagged(name, options)
	default:
		w.mu.Lock()
		if w.fallback[name] == held {
			delete(w.fallback, name)
		}
		w.mu.Unlock()
		return nil
	}
	key := js.Stringify(cronwatch.DescribeJob(name, made...))
	// In turn with Declare, so the two never declare one name at once and
	// leave the client holding the one without the schedule.
	w.declaring.Lock()
	defer w.declaring.Unlock()
	w.mu.Lock()
	if d, ok := w.jobs[name]; ok {
		w.mu.Unlock()
		return d.job
	}
	if existing, ok := w.fallback[name]; ok && existing.key == key && w.cw.Declares(name) {
		existing.checked = time.Now()
		w.mu.Unlock()
		return existing.job
	}
	w.mu.Unlock()
	job, err := w.cw.Job(name, made...)
	if err != nil {
		w.ReportOnce(err, "declaring "+name)
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.fallback[name] = &fallbackJob{job: job, key: key, checked: time.Now()}
	return job
}
