package cronwatch_test

// Replays client.json's outbox: per-channel delivery through a client on a
// memory store, step by step, as scripts/conformance.mjs drives the SDK's.
// Each channel records what it was given; the state's queue and outbox, its
// lastAlertAt, and the errors reported are compared after every step.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	cronwatch "cronwatch.dev/go"
	"cronwatch.dev/go/internal/js"
)

// outboxSend is one alert a fake channel was given.
type outboxSend struct {
	position, order int
	entry           *js.Object
}

// expandText is the fixture's recipe for long text: a string, or
// { parts: [[piece, times], ...] } joined.
func expandText(spec any) string {
	if s, ok := spec.(string); ok {
		return s
	}
	var b strings.Builder
	for _, p := range jsList(jsGet(spec, "parts")) {
		pair := p.([]any)
		b.WriteString(strings.Repeat(pair[0].(string), int(pair[1].(float64))))
	}
	return b.String()
}

// channelKeysOf is the SDK's channelKeys, for the replay's own channels.
func channelKeysOf(names []string) []string {
	seen, taken := map[string]int{}, map[string]bool{}
	keys := make([]string, len(names))
	for i, name := range names {
		n := seen[name] + 1
		seen[name] = n
		key := name
		if n != 1 {
			key = fmt.Sprintf("%s#%d", name, n)
		}
		for taken[key] {
			n++
			key = fmt.Sprintf("%s#%d", name, n)
		}
		taken[key] = true
		keys[i] = key
	}
	return keys
}

// sortedChannelErrors is the error wheres with each run of "alert channel"
// reports (one send's failures) sorted, everything else where it stands.
func sortedChannelErrors(wheres []string) []string {
	out := append([]string{}, wheres...)
	for i := 0; i < len(out); {
		j := i
		for j < len(out) && strings.HasPrefix(out[j], "alert channel ") {
			j++
		}
		sort.Strings(out[i:j])
		i = max(j, i+1)
	}
	return out
}

func strings16(v any) []string {
	out := []string{}
	for _, s := range jsList(v) {
		out = append(out, s.(string))
	}
	return out
}

func TestConformanceClientOutbox(t *testing.T) {
	root := readJS(t, clientFixture)
	scenarios := jsList(jsGet(root, "outbox"))
	if len(scenarios) == 0 {
		t.Fatal("client.json has no outbox")
	}
	ctx := context.Background()
	for _, sc := range scenarios {
		t.Run(jsGet(sc, "name").(string), func(t *testing.T) {
			var mu sync.Mutex
			now := int64(0)
			store := cronwatch.NewMemoryStore()
			down := map[string]bool{}
			var log []outboxSend
			var reported []string
			// What each failed run's error was in the SDK (Fail writes Go's
			// "Error: <message>"), so a length the SDK measured on a whole
			// error is compared with the whole error here.
			sdkErrors := map[int]bool{}
			names := func(v any) []string {
				out := []string{}
				for _, n := range jsList(v) {
					out = append(out, n.(string))
				}
				return out
			}
			make := func(channels []string) *cronwatch.Client {
				keys := channelKeysOf(channels)
				list := []cronwatch.Channel{}
				for i, name := range channels {
					list = append(list, cronwatch.ChannelFunc(name, func(_ context.Context, a cronwatch.Alert) error {
						mu.Lock()
						defer mu.Unlock()
						accepted := !down[keys[i]]
						var run, errorLength, outputLength any
						if a.Run != nil {
							run = a.Run.ID
							if a.Run.Error != nil {
								errorLength = js.Length16(*a.Run.Error)
							}
							if a.Run.Output != nil {
								outputLength = js.Length16(*a.Run.Output)
							}
						}
						log = append(log, outboxSend{i, len(log), js.NewObject("channel", keys[i], "type", string(a.Type), "run", run,
							"accepted", accepted, "errorLength", errorLength, "outputLength", outputLength)})
						if !accepted {
							return errors.New(keys[i] + " down")
						}
						return nil
					}))
				}
				return cronwatch.MustNew(cronwatch.WithStore(store), cronwatch.WithClock(func() int64 { mu.Lock(); defer mu.Unlock(); return now }),
					cronwatch.WithoutCronSecret(), cronwatch.WithAlerts(list...),
					cronwatch.WithErrorHandler(func(_ error, where string) { mu.Lock(); defer mu.Unlock(); reported = append(reported, where) }))
			}
			setNow := func(at int64) { mu.Lock(); now = at; mu.Unlock() }
			cw := make(names(jsGet(sc, "channels")))
			stateJSON := func() *js.Object {
				s, err := store.GetState(ctx, "nightly")
				check(t, err)
				if s == nil {
					return &js.Object{}
				}
				data, err := s.MarshalJSON()
				check(t, err)
				v, err := js.Parse(string(data))
				check(t, err)
				return v.(*js.Object)
			}
			brief := func(a any) any {
				o := js.NewObject("type", jsGet(a, "type"))
				run := jsGet(a, "run")
				if run != nil {
					run = jsGet(run, "id")
				}
				o.Set("run", run)
				if owed, has := a.(*js.Object).Get("owed"); has {
					o.Set("owed", owed)
				}
				return o
			}
			for i, st := range jsList(jsGet(sc, "steps")) {
				op := jsGet(st, "op").(string)
				switch op {
				case "fail", "ok":
					at := int64(jsGet(st, "at").(float64))
					setNow(at)
					h, err := cw.MustJob("nightly").Start(ctx, cronwatch.WithRunID(jsGet(st, "id").(string)))
					check(t, err)
					setNow(at + 1000)
					if op == "ok" {
						h.FinishWith(ctx, "done")
					} else {
						text := "boom"
						if spec := jsGet(st, "error"); spec != nil {
							text = expandText(spec)
						}
						sdkErrors[js.Length16(text)] = true
						h.Fail(ctx, errors.New(text))
					}
				case "check":
					setNow(int64(jsGet(st, "at").(float64)))
					_, err := cw.Check(ctx)
					check(t, err)
				case "down", "up":
					mu.Lock()
					for _, key := range names(jsGet(st, "channels")) {
						down[key] = op == "down"
					}
					mu.Unlock()
				case "configure":
					check(t, cw.Close())
					cw = make(names(jsGet(st, "channels")))
				case "legacy":
					o := stateJSON()
					for _, a := range jsList(jsGet(o, "undelivered")) {
						a.(*js.Object).Delete("owed")
					}
					var s cronwatch.JobState
					check(t, s.UnmarshalJSON([]byte(js.Stringify(o))))
					check(t, store.SetState(ctx, s))
				default:
					t.Fatalf("unknown op %q", op)
				}
				mu.Lock()
				sort.SliceStable(log, func(a, b int) bool {
					if log[a].position != log[b].position {
						return log[a].position < log[b].position
					}
					return log[a].order < log[b].order
				})
				sent := []any{}
				for _, s := range log {
					sent = append(sent, s.entry)
				}
				errs := append([]string{}, reported...)
				log, reported = nil, nil
				mu.Unlock()
				o := stateJSON()
				undelivered, sending := []any{}, []any{}
				for _, a := range jsList(jsGet(o, "undelivered")) {
					undelivered = append(undelivered, brief(a))
				}
				for _, e := range jsList(jsGet(o, "sending")) {
					sending = append(sending, brief(jsGet(e, "alert")))
				}
				last, _ := o.Get("lastAlertAt")
				got := js.NewObject("sent", sent, "undelivered", undelivered, "sending", sending, "lastAlertAt", last, "errors", errs)
				want := jsGet(st, "expect").(*js.Object).Clone()
				// Go sends to the channels concurrently and reports each
				// channel's failure as soon as it answers, so the order those
				// reports arrive in means nothing: they are compared as a
				// multiset, sorted on both sides. The rest keeps its order.
				got.Set("errors", sortedChannelErrors(errs))
				want.Set("errors", sortedChannelErrors(strings16(jsGet(want, "errors"))))
				// The SDK's fail(text) stores text as it is; Go's Fail is
				// given an error, stored as "Error: <text>". A whole error
				// is that much longer here; a trimmed one is as long.
				for _, s := range jsList(jsGet(want, "sent")) {
					if n, ok := jsGet(s, "errorLength").(float64); ok && sdkErrors[int(n)] {
						s.(*js.Object).Set("errorLength", n+float64(len("Error: ")))
					}
				}
				if g, w := js.Stringify(got), js.Stringify(want); g != w {
					t.Errorf("step %d (%s):\n got %s\nwant %s", i, op, g, w)
				}
			}
			check(t, cw.Close())
		})
	}
}
