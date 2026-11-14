package cronwatch

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The review: String(value) of a list joined each level's parts, copying a
// nested list's text once per level, so a megabyte of lists inside lists
// posted to the sign-in form took half a second. It is written once now.
func TestTextOfANestedListIsWrittenOnce(t *testing.T) {
	eq := func(what, got, want string) {
		t.Helper()
		if got != want {
			t.Errorf("%s: got %q, want %q", what, got, want)
		}
	}
	// As Array.prototype.join writes them: null as nothing, a list inside
	// a list flattened, an object as [object Object].
	eq("a list", jsText([]any{1.0, "a", nil, true, []any{2.0, []any{}, []any{nil, nil}}, map[string]any{}}, true), "1,a,,true,2,,,,[object Object]")
	eq("an empty list", jsText([]any{}, true), "")
	eq("null alone", jsText(nil, true), "null")

	// A long list inside 9,000 others costs what the long list alone costs.
	const depth, leaves = 9000, 1_000_000
	leaf := make([]any, leaves)
	for i := range leaf {
		leaf[i] = 1.0
	}
	start := time.Now()
	flat := jsText(leaf, true)
	alone := time.Since(start)
	var value any = leaf
	for range depth {
		value = []any{value, 1.0}
	}
	start = time.Now()
	text := jsText(value, true)
	nested := time.Since(start)
	if want := len(flat) + 2*depth; len(text) != want || !strings.HasPrefix(text, flat) {
		t.Errorf("%d bytes, want %d", len(text), want)
	}
	// Joined level by level this copies some 18 GB.
	if nested > 3*alone+500*time.Millisecond {
		t.Errorf("a list %d deep took %v, the list inside it alone %v", depth, nested, alone)
	}
}

// A JSON body gives the routes the fields they read and no others, so
// nothing else in it is written out as text.
func TestAJSONBodyGivesOnlyTheFieldsTheRoutesRead(t *testing.T) {
	read := func(body string) map[string]string {
		r := httptest.NewRequest("POST", "/cronwatch/signin", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		return readBody(r, []byte(body))
	}
	got := read(`{"other":[[1,2],[3]],"token":["a",["b"]],"for":90000,"more":{"x":1}}`)
	if len(got) != 2 || got["token"] != "a,b" || got["for"] != "90000" {
		t.Errorf("got %v", got)
	}
	if got := read(`{"other":1}`); got == nil || len(got) != 0 {
		t.Errorf("an object without them: %v", got)
	}
	if got := read(`[1]`); got != nil {
		t.Errorf("not an object: %v", got)
	}
}

// The whole of it through the routes, with no credentials: the two bodies
// that were slow, a megabyte each, are answered like any wrong token.
func TestASignInBodyOfAMegabyteIsReadInLinearTime(t *testing.T) {
	cw, err := New(WithErrorHandler(func(error, string) {}))
	if err != nil {
		t.Fatal(err)
	}
	rt, err := cw.Routes(WithToken("the-token"), WithBasePath("/cronwatch"))
	if err != nil {
		t.Fatal(err)
	}
	var keys strings.Builder
	keys.WriteByte('{')
	for i := 0; keys.Len() < maxBody-40; i++ {
		if i > 0 {
			keys.WriteByte(',')
		}
		keys.WriteString(`"` + num(4_000_000_000-i) + `":0`)
	}
	keys.WriteByte('}')
	const depth = 9900
	nested := `{"token":` + strings.Repeat("[", depth) + strings.Repeat("1,", (maxBody-100-depth*4)/2) + "1" + strings.Repeat(",1]", depth) + "}"
	post := func(body string) time.Duration {
		t.Helper()
		if len(body) > maxBody {
			t.Fatalf("the body is %d bytes", len(body))
		}
		r := httptest.NewRequest("POST", "/cronwatch/signin", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		start := time.Now()
		rt.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Errorf("answered %d", w.Code)
		}
		return time.Since(start)
	}
	plain := post(`{"token":"` + strings.Repeat("a", maxBody-20) + `"}`)
	for what, body := range map[string]string{"descending index keys": keys.String(), "nested lists": nested} {
		if took := post(body); took > 25*plain+250*time.Millisecond {
			t.Errorf("%s took %v, a plain body of the same size %v", what, took, plain)
		}
	}
}
