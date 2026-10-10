package cronwatch

import (
	"os"
	"strings"

	"cronwatch.dev/go/internal/js"
)

// envVariables name the environment, the first one set to more than
// spaces wins (trimmed as JavaScript trims). The SDK reads
// NODE_ENV; Go has no one convention, so CronWatch's own variable comes
// first, then APP_ENV (as the PHP port reads it) and GO_ENV.
var envVariables = []string{"CRONWATCH_ENV", "APP_ENV", "GO_ENV"}

// environmentOf is the environment's name from the values of the
// variables that name it, in order, as every CronWatch library reads it
// (env.ts's environmentOf): the first that holds more than whitespace,
// trimmed and lowercased, with "prod" read as "production" and "dev" and
// "local" as "development"; "" when none is set. "test" and "testing" are
// not development: a test run or CI needs CRON_SECRET or a token, as
// production does.
func environmentOf(values []string) string {
	for _, value := range values {
		if v := strings.ToLower(js.Trim(value)); v != "" {
			switch v {
			case "prod":
				return "production"
			case "dev", "local":
				return "development"
			}
			return v
		}
	}
	return ""
}

// environment is environmentOf the variables in envVariables.
func environment() string {
	values := make([]string, len(envVariables))
	for i, name := range envVariables {
		values[i] = os.Getenv(name)
	}
	return environmentOf(values)
}

// blank is whether a token or secret is empty or only whitespace, as
// String.prototype.trim sees it. Such a value counts as not set, so the
// routes and handlers fail closed instead of taking it as a password.
func blank(s string) bool { return js.Trim(s) == "" }

// secretEnv is a secret from the environment (CRONWATCH_TOKEN,
// CRON_SECRET), trimmed as String.prototype.trim trims: a value read from a
// file often ends in a newline that no header can carry, since HTTP strips
// the whitespace around a header's value. "" when the variable is unset,
// empty, or only whitespace, so the routes and handlers fail closed.
func secretEnv(name string) string { return js.Trim(os.Getenv(name)) }
