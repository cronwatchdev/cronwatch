package triage

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The default limit leaves room for the model's thinking as well as the
// paragraph, and an answer with no text because the limit ran out is an
// error naming the limit sent, so the client reports it rather than the
// alert going out with no diagnosis and no word of why. Text cut short is
// still a diagnosis; a refusal and an empty end_turn are none, without an
// error.
func TestAnAnswerCutOffBeforeAnyTextIsAnError(t *testing.T) {
	var sent []string
	answer := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sent = append(sent, string(body))
		w.Write([]byte(answer))
	}))
	defer server.Close()
	tc := contexts(t, fixture(t))["a missed run with no runs"]
	usual, _ := Anthropic(AnthropicOptions{APIKey: "k", BaseURL: server.URL})
	small, _ := Anthropic(AnthropicOptions{APIKey: "k", BaseURL: server.URL, MaxTokens: 1000})

	answer = `{"stop_reason":"max_tokens","content":[{"type":"thinking","thinking":"Let me see"}]}`
	if got, err := usual(bg, tc); got != "" || err == nil || err.Error() != "triage: the model reached max_tokens (4096) before writing a diagnosis; raise maxTokens or lower effort" {
		t.Errorf("the default limit: %q, %v", got, err)
	}
	if !strings.Contains(sent[0], `"max_tokens":4096`) {
		t.Errorf("sent %s", sent[0])
	}
	if _, err := small(bg, tc); err == nil || !strings.Contains(err.Error(), "max_tokens (1000)") {
		t.Errorf("a limit of the app's own: %v", err)
	}
	if !strings.Contains(sent[1], `"max_tokens":1000`) {
		t.Errorf("sent %s", sent[1])
	}

	answer = `{"stop_reason":"max_tokens","content":[{"type":"text","text":"The disk was full, and the"}]}`
	if got, err := usual(bg, tc); err != nil || got != "The disk was full, and the" {
		t.Errorf("text cut short: %q, %v", got, err)
	}
	for _, none := range []string{`{"stop_reason":"refusal","content":[]}`, `{"stop_reason":"end_turn","content":[]}`} {
		answer = none
		if got, err := usual(bg, tc); err != nil || got != "" {
			t.Errorf("%s: %q, %v", none, got, err)
		}
	}
}
