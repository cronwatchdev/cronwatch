<?php

declare(strict_types=1);

namespace Cronwatch\Alerts;

use Cronwatch\Alert;
use Cronwatch\Js;

/**
 * POSTs the alert as JSON to any URL (alerts/webhook.ts). The body is the
 * Alert after a version: { schema: 1, type, run, details, job, definition,
 * title, message, at, triage }, described by
 * https://cronwatch.dev/schemas/webhook/1.json. With a secret, each request
 * is signed (HMAC-SHA256, as lowercase hex), so the receiver can verify it:
 *
 * - X-CronWatch-Signature: sha256=<hex> signs the raw body alone
 *   (signature()), as it always has.
 * - X-CronWatch-Timestamp: <unix seconds> is when it was sent, and
 *   X-CronWatch-Timestamp-Signature: sha256=<hex> signs
 *   "<timestamp>.<raw body>", so a receiver that checks this one with
 *   hash_equals() and refuses a timestamp more than five minutes from its
 *   own clock cannot be sent a captured alert again later.
 *
 * A redirect is an error: point the url at where the receiver really is.
 *
 *     new Webhook(url: 'https://hooks.example.com/cronwatch', secret: getenv('CRONWATCH_WEBHOOK_SECRET'))
 */
final class Webhook implements AlertChannel
{
    /**
     * The payload's version, sent as its first field. It goes up only if a
     * major release changes the payload in a way that is not additive.
     */
    public const SCHEMA = 1;

    private readonly Http $http;
    private readonly ?\Closure $now;

    /**
     * @param array<string, string> $headers extra request headers, for an Authorization header say; values are trimmed.
     *        A name is matched without regard to case, so a Content-Type given here replaces the default one rather
     *        than going beside it; the signature headers are always the channel's own
     * @param (callable(): (int|float))|null $now the clock the timestamp is read from, in milliseconds; default the system's
     */
    public function __construct(
        #[\SensitiveParameter] private readonly string $url,
        #[\SensitiveParameter] private readonly array $headers = [],
        #[\SensitiveParameter] private readonly ?string $secret = null,
        ?Http $http = null,
        ?callable $now = null,
    ) {
        if ($url === '') {
            throw new \InvalidArgumentException('Webhook needs a url');
        }
        $this->http = $http ?? Transport::default();
        $this->now = $now === null ? null : \Closure::fromCallable($now);
    }

    public function name(): string
    {
        return 'webhook';
    }

    public function send(Alert $alert, ChannelContext $context): void
    {
        // Sent as fetch sends a string: UTF-8, U+FFFD for bytes that are not,
        // so the signature is over exactly the bytes sent.
        $body = Js::wellFormed(Js::stringify(['schema' => self::SCHEMA, ...$alert->toJson()]));
        if ($this->secret === '') {
            $context->onError(new \RuntimeException('webhook: secret is an empty string, so this request is sent unsigned; set the secret, or leave it out to send unsigned on purpose'));
        }
        $headers = ['content-type' => 'application/json', 'user-agent' => 'cronwatch'];
        // A pasted Authorization value often carries a stray space or newline, which fetch would refuse.
        foreach ($this->headers as $name => $value) {
            self::setHeader($headers, (string) $name, is_string($value) ? Js::trim($value) : $value);
        }
        if (Shared::present($this->secret)) {
            $nowMs = $this->now !== null ? ($this->now)() : microtime(true) * 1000;
            $timestamp = Js::number(Js::floor($nowMs / 1000));
            self::setHeader($headers, 'x-cronwatch-signature', 'sha256=' . self::signature((string) $this->secret, $body));
            self::setHeader($headers, 'x-cronwatch-timestamp', $timestamp);
            self::setHeader($headers, 'x-cronwatch-timestamp-signature', 'sha256=' . self::signature((string) $this->secret, "{$timestamp}.{$body}"));
        }
        // A redirect is refused, not followed: the headers (and the signature) would go with it.
        $response = $this->http->post(Shared::postable($this->url), $body, $headers);
        // Only the origin: a webhook URL's path or query often is the credential.
        if (!$response->ok()) {
            throw new \RuntimeException('Webhook ' . Shared::origin($this->url) . " answered {$response->status}");
        }
    }

    /**
     * Sets a header, first taking out any of the same name in another case.
     *
     * @param array<string, mixed> $headers
     */
    private static function setHeader(array &$headers, string $name, mixed $value): void
    {
        $lower = strtolower($name);
        foreach (array_keys($headers) as $key) {
            if (strtolower((string) $key) === $lower) {
                unset($headers[$key]);
            }
        }
        $headers[$name] = $value;
    }

    /**
     * The signature a receiver checks: the HMAC-SHA256 of the raw body with
     * the secret, as lowercase hex, without the "sha256=" the header puts
     * before it. Compare it with hash_equals(). The timestamped one is the
     * signature of "<timestamp>.<body>", with the X-CronWatch-Timestamp value
     * as sent; check that one, and refuse a timestamp more than five minutes
     * from your clock:
     *
     *     $body = file_get_contents('php://input');
     *     $timestamp = $_SERVER['HTTP_X_CRONWATCH_TIMESTAMP'] ?? '';
     *     $sent = $_SERVER['HTTP_X_CRONWATCH_TIMESTAMP_SIGNATURE'] ?? '';
     *     $ok = preg_match('/^[0-9]+$/D', $timestamp) === 1 && abs(time() - (int) $timestamp) <= 300
     *         && hash_equals('sha256=' . Webhook::signature($secret, "{$timestamp}.{$body}"), $sent);
     */
    public static function signature(#[\SensitiveParameter] string $secret, #[\SensitiveParameter] string $body): string
    {
        return hash_hmac('sha256', Js::wellFormed($body), Js::wellFormed($secret));
    }

    /**
     * HMAC-SHA256 of the body as lowercase hex.
     *
     * @deprecated Public by accident; removed in 1.0: use signature(), which is the same.
     */
    public static function hmacSha256Hex(#[\SensitiveParameter] string $secret, #[\SensitiveParameter] string $body): string
    {
        return self::signature($secret, $body);
    }
}
