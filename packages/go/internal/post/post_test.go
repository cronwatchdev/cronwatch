package post

// The URL and header checks, the error body's cut, and the transport error's
// URL taken out, case by case. The channels' tests (alerts) cover the
// request itself against real servers.

import (
	"net/url"
	"strings"
	"testing"
)

func TestPostable(t *testing.T) {
	for _, c := range []struct{ raw, want, err string }{
		{"https://hooks.example.com/T/B/x", "https://hooks.example.com/T/B/x", ""},
		{"  http://hooks.example.com/in\n", "http://hooks.example.com/in", ""},
		{"https://hooks.exa\tmple.com/a\r\nb", "https://hooks.example.com/ab", ""},
		{"HTTPS://hooks.example.com/", "HTTPS://hooks.example.com/", ""},
		{"ftp://files.example.com/x", "", "only http and https URLs can be posted to, not ftp:"},
		{"javascript://x/alert(1)", "", "only http and https URLs can be posted to, not javascript:"},
		{"hooks.example.com/no-scheme", "", "only http and https URLs can be posted to, not this URL"},
		{"https:///no-host", "", "only http and https URLs can be posted to, not this URL"},
		{"https://user:pw@hooks.example.com/", "", "only http and https URLs can be posted to, not this URL"},
		{"https://hooks.example.com/a b", "", "only http and https URLs can be posted to, not this URL"},
		{"https://hooks.example.com/\x7f", "", "only http and https URLs can be posted to, not this URL"},
		{"mailto:someone@example.com", "", "only http and https URLs can be posted to, not this URL"},
		{"", "", "only http and https URLs can be posted to, not this URL"},
	} {
		got, err := Postable(c.raw)
		if c.err != "" {
			if err == nil || err.Error() != c.err {
				t.Errorf("Postable(%q): %q, %v; want the error %q", c.raw, got, err, c.err)
			}
			if err != nil && strings.Contains(err.Error(), "example.com") {
				t.Errorf("Postable(%q) quoted the URL: %v", c.raw, err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("Postable(%q): %q, %v; want %q", c.raw, got, err, c.want)
		}
	}
}

func TestHeaders(t *testing.T) {
	h, err := Headers([]Header{{"content-type", "application/json"}, {"Authorization", " \tBearer abc\r\n"}, {"X-Two", "a"}, {"x-two", "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := h.Get("Content-Type"); got != "application/json" {
		t.Errorf("content-type %q", got)
	}
	if got := h.Get("Authorization"); got != "Bearer abc" {
		t.Errorf("a value is trimmed as fetch trims it: %q", got)
	}
	if got := h.Values("X-Two"); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("names that differ only in case are both sent: %q", got)
	}
	for _, c := range []struct {
		header Header
		err    string
	}{
		{Header{"", "x"}, "a header name must be a token (letters, digits, and !#$%&'*+.^_`|~-)"},
		{Header{"Bad Name", "x"}, "a header name must be a token (letters, digits, and !#$%&'*+.^_`|~-)"},
		{Header{"X-Bad:", "x"}, "a header name must be a token (letters, digits, and !#$%&'*+.^_`|~-)"},
		{Header{"X-Smuggle", "secret\r\nX-Other: 1"}, "the X-Smuggle header's value may not contain a line break"},
		{Header{"X-Smuggle", "secret\nmore"}, "the X-Smuggle header's value may not contain a line break"},
		{Header{"X-Nul", "a\x00b"}, "the X-Nul header's value may not contain a line break"},
	} {
		_, err := Headers([]Header{c.header})
		if err == nil || err.Error() != c.err {
			t.Errorf("Headers(%q): %v, want %q", c.header, err, c.err)
		}
		if err != nil && c.header.Value != "x" && strings.Contains(err.Error(), "secret") {
			t.Errorf("the error quoted the value: %v", err)
		}
	}
}

func TestWithoutURL(t *testing.T) {
	target := "https://hooks.slack.example/services/T0/B0/xyzzy?key=s3cret"
	u, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	origin := Origin(target)
	for _, c := range []struct {
		text, want string
		cut        bool
	}{
		{"dial tcp: lookup hooks.slack.example: no such host", "dial tcp: lookup hooks.slack.example: no such host", false},
		{`Post "` + target + `": EOF`, `Post "` + origin + `": EOF`, true},
		{"proxy refused /services/T0/B0/xyzzy?key=s3cret", "proxy refused ", true},
		{"saw path /services/T0/B0/xyzzy and query key=s3cret", "saw path  and query ", true},
	} {
		got, cut := withoutURL(c.text, origin, target, u)
		if got != c.want || cut != c.cut {
			t.Errorf("withoutURL(%q): %q, %v; want %q, %v", c.text, got, cut, c.want, c.cut)
		}
		if strings.Contains(got, "xyzzy") || strings.Contains(got, "s3cret") {
			t.Errorf("withoutURL(%q) kept the credential: %q", c.text, got)
		}
	}
	// A URL with no path or query of its own has nothing to cut but itself.
	bare := "https://hooks.example.com"
	b, _ := url.Parse(bare)
	if got, cut := withoutURL("connect to https://hooks.example.com refused", Origin(bare), bare, b); cut || got != "connect to https://hooks.example.com refused" {
		t.Errorf("a bare origin: %q, %v", got, cut)
	}
}

func TestErrorBody(t *testing.T) {
	long := strings.Repeat("a", 300)
	for _, c := range []struct {
		name    string
		text    string
		secrets []string
		want    string
	}{
		{"short and clean", "bad request", nil, "bad request"},
		{"cut to 200", long, nil, long[:200]},
		{"a secret redacted", "token xoxb-1234 refused", []string{"xoxb-1234"}, "token [redacted] refused"},
		{"short secrets are left", "id abc", []string{"abc", ""}, "id abc"},
		{"every secret", "k1=alpha1 k2=bravo2", []string{"alpha1", "bravo2"}, "k1=[redacted] k2=[redacted]"},
		// A secret that starts inside the first 200 characters and runs past
		// them is still found: no part of it survives at the edge.
		{"straddling the cut", strings.Repeat("x", 195) + "SECRETVALUE" + strings.Repeat("y", 50), []string{"SECRETVALUE"}, strings.Repeat("x", 195) + "[reda"},
		// Cut in UTF-16 code units, never through a surrogate pair.
		{"surrogate pairs", strings.Repeat("😀", 150), nil, strings.Repeat("😀", 100)},
		{"odd cut", "a" + strings.Repeat("😀", 150), nil, "a" + strings.Repeat("😀", 99)},
	} {
		if got := ErrorBody(c.text, c.secrets...); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
