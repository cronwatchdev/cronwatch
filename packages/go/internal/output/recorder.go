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
	// lines and their lengths in code units; the front is dropped once the
	// total passes window.
	lines   []string
	lengths []int
	size    int
	// head is the first lines logged, until they reach OutputCap, kept for
	// expect even after the window has dropped them.
	head      []string
	headSize  int
	whole     string
	wholeLost bool
	tail      string
	logged    bool
	metrics   *js.Object
}

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
		if js.Length16(r.whole)+len(separator)+n <= 2*OutputCap {
			r.whole += separator + line
		} else {
			r.wholeLost = true
			r.whole = ""
		}
	}
	r.tail = strings.Clone(tail(r.tail+separator+tail(line, OutputCap), OutputCap))
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
	r.lines = append(r.lines, line)
	r.lengths = append(r.lengths, n)
	r.size += n + 1
	// Drop from the front once well past the cap; RedactAndCap trims exactly at the end.
	for r.size > window && len(r.lines) > 1 {
		r.size -= r.lengths[0] + 1
		r.lines[0] = ""
		r.lines = r.lines[1:]
		r.lengths = r.lengths[1:]
	}
}

// Output is the lines still held (past the window the oldest are let go),
// joined and not yet capped: the client redacts them first, then caps them
// (RedactAndCap). Nil when nothing was logged.
func (r *Recorder) Output() *string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.lines) == 0 {
		return nil
	}
	s := strings.Join(r.lines, "\n")
	return &s
}

// ExpectText is what an expect rule is checked against: everything logged,
// or when that ran long, the first 16 KB and the last 16 KB. The stored
// output keeps only the tail, so a "done" line printed early would
// otherwise be lost. Nil when nothing was logged.
func (r *Recorder) ExpectText() *string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.lines) == 0 {
		return nil
	}
	if !r.wholeLost {
		s := r.whole
		return &s
	}
	s := js.Head16(strings.Join(r.head, "\n"), OutputCap) + "\n" + r.tail
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
