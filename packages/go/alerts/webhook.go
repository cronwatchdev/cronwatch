package alerts

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	cronwatch "cronwatch.dev/go"
	"cronwatch.dev/go/internal/js"
	"cronwatch.dev/go/internal/post"
)

// WebhookOptions configure Webhook.
type WebhookOptions struct {
	// URL is where the alert is posted. Errors name only its origin, since
	// a webhook URL's path or query is often the credential.
	URL string
	// Headers are extra request headers, an Authorization header say.
	// Values are trimmed of the spaces and newlines a paste leaves. A name
	// is matched without regard to case, so a Content-Type given here
	// replaces the default one rather than going beside it; the signature
	// headers are always the channel's own.
	Headers map[string]string
	// Secret, when set, signs each request with this secret (HMAC-SHA256,
	// as lowercase hex), so the receiver can verify it:
	//
	//   - X-CronWatch-Signature: sha256=<hex> signs the raw body alone, as
	//     it always has (see Signature).
	//   - X-CronWatch-Timestamp: <unix seconds> is when it was sent, and
	//     X-CronWatch-Timestamp-Signature: sha256=<hex> signs
	//     <timestamp>.<raw body>, so a receiver that checks this one with a
	//     constant-time compare, and refuses a timestamp more than five
	//     minutes from its own clock, cannot be sent a captured alert again
	//     later.
	Secret string
	// Now is the clock the timestamp is read from, in epoch milliseconds.
	// For tests; nil for the time now.
	Now func() int64
	// HTTPClient sends the request; nil for Go's default transport. It is
	// used as a copy that never follows a redirect.
	HTTPClient *http.Client
}

// webhookSchema is the payload's version, sent as its first field. It goes
// up only if a major release changes the payload in a way that is not
// additive; see https://cronwatch.dev/schemas/webhook/1.json.
const webhookSchema = 1

// Webhook posts each alert as JSON to any URL. The body is the Alert as
// the SDK writes it after a version: {schema: 1, type, run, details, job,
// definition, title, message, at, triage}. A redirect is an error: point
// the URL at where the receiver really is.
func Webhook(o WebhookOptions) (cronwatch.Channel, error) {
	if o.URL == "" {
		return nil, errors.New("alerts.Webhook needs a URL")
	}
	names := make([]string, 0, len(o.Headers))
	for name := range o.Headers {
		names = append(names, name)
	}
	sort.Strings(names)
	now := o.Now
	if now == nil {
		now = func() int64 { return time.Now().UnixMilli() }
	}
	return &channel{name: "webhook", send: func(ctx context.Context, a cronwatch.Alert, _ cronwatch.ChannelContext) error {
		body := webhookBody(a)
		headers := []header{{Name: "content-type", Value: "application/json"}, {Name: "user-agent", Value: "cronwatch"}}
		for _, name := range names {
			// A pasted Authorization value often carries a stray space or newline, which fetch would refuse.
			headers = assign(headers, name, js.Trim(o.Headers[name]))
		}
		if o.Secret != "" {
			timestamp := strconv.FormatInt(js.FloorDiv(now(), 1000), 10)
			headers = assign(headers, "x-cronwatch-signature", "sha256="+Signature(o.Secret, body))
			headers = assign(headers, "x-cronwatch-timestamp", timestamp)
			headers = assign(headers, "x-cronwatch-timestamp-signature", "sha256="+Signature(o.Secret, timestamp+"."+body))
		}
		// A redirect is refused, not followed: the headers (and the signature) would go with it.
		resp, err := post.Do(ctx, o.HTTPClient, o.URL, headers, body)
		if err != nil {
			return err
		}
		if !resp.OK() {
			// Only the origin: a webhook URL's path or query often is the credential.
			return fmt.Errorf("%s %s answered %d", "Webhook", post.Origin(o.URL), resp.Status)
		}
		return nil
	}}, nil
}

// webhookBody is the alert's JSON with "schema" as its first key, then the
// alert's own keys in the SDK's order, written exactly as they are.
func webhookBody(a cronwatch.Alert) string {
	body := js.Stringify(a)
	return `{"schema":` + strconv.Itoa(webhookSchema) + "," + strings.TrimPrefix(body, "{")
}

// assign sets a header, first taking out any of the same name in another
// case, and puts it last, as the SDK's setHeader deletes a JavaScript
// object's keys and sets the new one.
func assign(headers []header, name, value string) []header {
	lower := strings.ToLower(name)
	kept := headers[:0]
	for _, h := range headers {
		if strings.ToLower(h.Name) != lower {
			kept = append(kept, h)
		}
	}
	return append(kept, header{Name: name, Value: value})
}

// Signature is the webhook's signature of a body: the HMAC-SHA256 of the
// body with the secret, as lowercase hex. The request carries it as
// X-CronWatch-Signature: sha256=<Signature>, so a receiver checks one with
// the raw body it got (before any JSON parsing) and a constant-time
// compare. The timestamped one is the signature of <timestamp>.<body>,
// with the X-CronWatch-Timestamp value as sent.
func Signature(secret, body string) string {
	return hex.EncodeToString(hmacSHA256([]byte(js.WellFormed(secret)), js.WellFormed(body)))
}
