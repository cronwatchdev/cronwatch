<?php

declare(strict_types=1);

namespace Cronwatch\Alerts;

/**
 * What the channels and Claude triage POST through. The SDK calls fetch with
 * redirect: "error" and AbortSignal.timeout(10_000); an Http does the same:
 * a redirect is an answer like any other outside 2xx (never followed, so
 * credential headers never go where it points), and the whole request has
 * one deadline. Every channel takes `http:`, so a test (or WordPress, whose
 * HTTP API a plugin should use) can stand in for the network.
 */
interface Http
{
    /** Milliseconds a channel's request may take, as the SDK's AbortSignal.timeout(10_000). */
    public const TIMEOUT_MS = 10_000;

    /**
     * The most of an answer's body an Http keeps (1 MiB): a channel reads 200
     * characters of a refusal and triage a few kilobytes, and a compressed
     * answer from a hostile or broken endpoint could otherwise decode to more
     * than PHP's memory limit, a fatal error no catch sees.
     */
    public const MAX_BODY = 1_048_576;

    /**
     * The most of an error answer's body (one outside 2xx) an Http keeps
     * (64 KiB), as the SDK's errorText() reads: far more than any secret
     * plus the 200 characters a channel quotes, so a provider (or something
     * posing as one) cannot make a send hold a body of any size.
     */
    public const ERROR_READ_MAX = 65_536;

    /**
     * POSTs `body` with `headers` (lowercase names, values sent trimmed).
     * Returns the answer, whatever its status; throws RequestTimeout when no
     * answer came within `timeoutMs`, and anything else for a request that
     * could not be made (no such host, refused, TLS). An answer whose body
     * is still arriving at the deadline is returned with an empty body, as
     * the SDK's channels treat a body they could not read. At most MAX_BODY
     * bytes of a body are kept, and ERROR_READ_MAX of one outside 2xx.
     *
     * @param array<string, string> $headers
     */
    public function post(string $url, string $body, array $headers, int $timeoutMs = self::TIMEOUT_MS): HttpResponse;
}
