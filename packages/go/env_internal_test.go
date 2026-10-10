package cronwatch

import (
	"os"
	"path/filepath"
	"testing"

	"cronwatch.dev/go/internal/js"
)

// clientCases are a section of conformance/client.json.
func clientCases(t *testing.T, section string) []*js.Object {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "conformance", "client.json"))
	if err != nil {
		t.Fatal(err)
	}
	root, err := js.Parse(string(data))
	if err != nil {
		t.Fatal(err)
	}
	list, _ := root.(*js.Object).Get(section)
	var out []*js.Object
	for _, c := range list.([]any) {
		out = append(out, c.(*js.Object))
	}
	if len(out) == 0 {
		t.Fatalf("no %s cases", section)
	}
	return out
}

// envValue is a case's value for a variable: unset for null.
func envValue(t *testing.T, c *js.Object, key, name string) {
	t.Helper()
	v, _ := c.Get(key)
	if s, ok := v.(string); ok {
		t.Setenv(name, s)
		return
	}
	t.Setenv(name, "")
	os.Unsetenv(name)
}

// client.json's environment: CRONWATCH_ENV, then APP_ENV, then the
// language's own variable (GO_ENV here, NODE_ENV in the SDK).
func TestConformanceEnvironment(t *testing.T) {
	for i, c := range clientCases(t, "environment") {
		envValue(t, c, "CRONWATCH_ENV", "CRONWATCH_ENV")
		envValue(t, c, "APP_ENV", "APP_ENV")
		envValue(t, c, "native", "GO_ENV")
		want, _ := c.Get("environment")
		development, _ := c.Get("development")
		production, _ := c.Get("production")
		got := environment()
		if got != want || (got == "development") != development || (got == "production") != production {
			t.Errorf("case %d (%s): %q", i, js.Stringify(c), got)
		}
	}
}

// client.json's secrets: CRONWATCH_TOKEN and CRON_SECRET, trimmed as
// JavaScript trims, blank meaning unset; a secret given in code the same.
func TestConformanceSecrets(t *testing.T) {
	for i, c := range clientCases(t, "secrets") {
		value, _ := c.Get("value")
		want, _ := c.Get("secret")
		wanted := ""
		if s, ok := want.(string); ok {
			wanted = s
		}
		t.Setenv("CRON_SECRET", value.(string))
		if got := secretEnv("CRON_SECRET"); got != wanted {
			t.Errorf("case %d env: %q, want %q", i, got, wanted)
		}
		cw := MustNew(WithStore(NewMemoryStore()), WithCronSecret(value.(string)))
		if cw.cronSecret != wanted {
			t.Errorf("case %d option: %q, want %q", i, cw.cronSecret, wanted)
		}
	}
}
