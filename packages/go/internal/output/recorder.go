package output

import (
	"fmt"
	"math"
	"strings"
	"sync"

	"cronwatch.dev/go/internal/js"
)

// window is how much logged text a recorder holds, in code units, before
// it drops lines from the front. RedactAndCap trims exactly at the end, so
// this only bounds memory: well past the cap, so the kept tail is whole.
const window = OutputCap + RedactEdge + 2

// Recorder is createRecorder in job.ts: the lines a run logs and the
// numbers it reports. It is safe for concurrent use, since a job may log
// from goroutines of its own.
type Recorder struct {
	mu sync.Mutex
	// lines is what is stored; the front is dropped once it passes window.
	lines lineWindow
	// head is the first lines logged, until they reach OutputCap, kept for
	// expect even after the window has dropped them.
	head     []string
	headSize int
	// whole is everything logged while it fits in 2*OutputCap, with its
	// length in code units, so a line costs only its own length.
	whole     strings.Builder
	wholeSize int
	wholeLost bool
	// raw is the lines behind ExpectText's tail, joined only when it is read.
	raw     lineWindow
	logged  bool
	metrics *js.Object
}

// lineWindow is lines kept newest last, each counted in code units with the
// newline that joins it. The oldest are dropped by reslicing, never by
// copying what is left (append moves only the live lines when it grows).
type lineWindow struct {
	lines   []string
	lengths []int
	size    int
}

func (w *lineWindow) push(line string, n int) {
	w.lines = append(w.lines, line)
	w.lengths = append(w.lengths, n)
	w.size += n + 1
}

// keepAtMost drops the oldest lines while over max, always keeping the newest.
func (w *lineWindow) keepAtMost(max int) {
	for w.size > max && len(w.lines) > 1 {
		w.drop()
	}
}

// keepAtLeast drops the oldest lines only while what is left still holds min.
func (w *lineWindow) keepAtLeast(min int) {
	for len(w.lines) > 1 && w.size-(w.lengths[0]+1) > min {
		w.drop()
	}
}

func (w *lineWindow) drop() {
	w.size -= w.lengths[0] + 1
	w.lines[0] = ""
	w.lines = w.lines[1:]
	w.lengths = w.lengths[1:]
}

func (w *lineWindow) join() string { return strings.Join(w.lines, "\n") }

// NewRecorder is an empty recorder.
func NewRecorder() *Recorder {
	return &Recorder{metrics: &js.Object{}}
}

// Log appends a line: the parts written as LogText and joined by spaces.
func (r *Recorder) Log(parts ...any) {
	texts := make([]string, len(parts))
	for i, p := range parts {
		texts[i] = LogText(p)
	}
	line := strings.Join(texts, " ")
	n := js.Length16(line)
	r.mu.Lock()
	defer r.mu.Unlock()
	separator := ""
	if r.logged {
		separator = "\n"
	}
	r.logged = true
	if !r.wholeLost {
		if r.wholeSize+len(separator)+n <= 2*OutputCap {
			r.whole.WriteString(separator)
			r.whole.WriteString(line)
			r.wholeSize += len(separator) + n
		} else {
			r.wholeLost = true
			r.whole = strings.Builder{}
		}
	}
	kept := tail(line, OutputCap)
	r.raw.push(strings.Clone(kept), js.Length16(kept))
	r.raw.keepAtLeast(OutputCap)
	if r.headSize < OutputCap {
		r.head = append(r.head, strings.Clone(js.Head16(line, OutputCap-r.headSize)))
		r.headSize += n + 1
	}
	line = StripNul(line)
	n = js.Length16(line)
	if n >= window {
		line = strings.Clone(js.Tail16(line, window-1))
		n = js.Length16(line)
	}
	r.lines.push(line, n)
	// Drop from the front once well past the cap; RedactAndCap trims exactly at the end.
	r.lines.keepAtMost(window)
}

// Output is the lines still held (past the window the oldest are let go),
// joined and not yet capped: the client redacts them first, then caps them
// (RedactAndCap). Nil when nothing was logged.
func (r *Recorder) Output() *string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.lines.lines) == 0 {
		return nil
	}
	s := r.lines.join()
	return &s
}

// ExpectText is what an expect rule is checked against: everything logged,
// or when that ran long, the first 16 KB and the last 16 KB. The stored
// output keeps only the tail, so a "done" line printed early would
// otherwise be lost. Nil when nothing was logged.
func (r *Recorder) ExpectText() *string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.lines.lines) == 0 {
		return nil
	}
	if !r.wholeLost {
		s := r.whole.String()
		return &s
	}
	s := js.Head16(strings.Join(r.head, "\n"), OutputCap) + "\n" + tail(r.raw.join(), OutputCap)
	return &s
}

// Metric reports a number for the run; a later value for the same name
// replaces an earlier one.
func (r *Recorder) Metric(name string, value float64) error {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return fmt.Errorf("metric \"%s\" must be a finite number", name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.metrics.Set(name, value)
	return nil
}

// Metrics is a copy of the numbers reported, in JavaScript's key order.
func (r *Recorder) Metrics() *js.Object {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.metrics.Clone()
}

func tail(s string, n int) string {
	if js.Length16(s) <= n {
		return s
	}
	return js.Tail16(s, n)
}
