package js

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// An object's keys are read in JavaScript's order however they come: the
// order setting each in turn gives.
func TestParseOrdersKeysAsSetDoes(t *testing.T) {
	for _, text := range []string{
		`{"b":1,"2":2,"a":3,"1":4,"b":5,"10":6,"01":7,"4294967295":8,"0":9,"2":10}`,
		`{"3":0,"2":0,"1":0}`,
		`{"x":0,"y":0,"x":1}`,
		`{}`,
	} {
		v, err := Parse(text)
		if err != nil {
			t.Fatal(text, err)
		}
		got := v.(*Object)
		// The same pairs, set in turn.
		want := &Object{}
		inner := strings.Trim(text, "{}")
		for _, pair := range strings.Split(inner, ",") {
			if pair == "" {
				continue
			}
			k, val, _ := strings.Cut(pair, ":")
			n, _ := Parse(val)
			want.Set(strings.Trim(k, `"`), n)
		}
		if !slices.Equal(got.Keys(), want.Keys()) {
			t.Errorf("%s: keys %v, want %v", text, got.Keys(), want.Keys())
		}
		if Stringify(got) != Stringify(want) {
			t.Errorf("%s: %s, want %s", text, Stringify(got), Stringify(want))
		}
	}
}

// The review: an index key was put in its place as it was read, shifting
// every key after it, so a megabyte of descending ones ("4000000000",
// "3999999999", ...) took half a second where ascending ones took a
// fiftieth of that, in a body anyone may post to the sign-in form. Four
// megabytes of them now cost what ascending ones cost.
func TestParseDescendingIndexKeysIsNotQuadratic(t *testing.T) {
	const n = 400_000
	object := func(key func(i int) int) string {
		var b strings.Builder
		b.WriteByte('{')
		for i := range n {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `"%d":0`, key(i))
		}
		b.WriteByte('}')
		return b.String()
	}
	timed := func(text string) (time.Duration, *Object) {
		start := time.Now()
		v, err := Parse(text)
		if err != nil {
			t.Fatal(err)
		}
		return time.Since(start), v.(*Object)
	}
	ascending, up := timed(object(func(i int) int { return i }))
	descending, down := timed(object(func(i int) int { return 4_000_000_000 - i }))
	if up.Len() != n || down.Len() != n {
		t.Fatalf("%d and %d keys, want %d", up.Len(), down.Len(), n)
	}
	keys := down.Keys()
	if keys[0] != fmt.Sprint(4_000_000_000-n+1) || keys[n-1] != "4000000000" {
		t.Errorf("descending keys read as %s to %s", keys[0], keys[n-1])
	}
	if descending > 10*ascending+500*time.Millisecond {
		t.Errorf("descending index keys took %v, ascending ones %v", descending, ascending)
	}
}
