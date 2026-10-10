package cronwatch

import "testing"

// The SDK's cases (routes-security.test.ts): the compare matches only the
// secret itself, whatever is sent, and takes time by what was sent.
func TestTheSecretCompareMatchesOnlyTheSecret(t *testing.T) {
	for _, c := range []struct {
		sent, secret string
		equal        bool
	}{
		{"tok", "tok", true}, {"tok", "tok2", false}, {"tok2", "tok", false}, {"toktok", "tok", false}, {"", "tok", false},
		{"", "", true}, {"x", "", false}, {"\U0001F600", "\U0001F600", true}, {"\U0001F600", "\U0001F601", false},
	} {
		if got := constantTimeEqual(c.sent, c.secret); got != c.equal {
			t.Errorf("%q vs %q: %v", c.sent, c.secret, got)
		}
	}
}
