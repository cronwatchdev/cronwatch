import type { AlertChannel } from "../types.js";
import { drain, postable } from "./shared.js";

export interface WebhookOptions {
  url: string;
  /**
   * Extra request headers, for an Authorization header say. Values are
   * trimmed. A name is matched without regard to case, so a Content-Type
   * given here replaces the default one rather than going beside it; the
   * signature headers are always the channel's own.
   */
  headers?: Record<string, string>;
  /**
   * When set, each request is signed with this secret (HMAC-SHA256, as
   * lowercase hex), so the receiver can verify it:
   *
   * - `X-CronWatch-Signature: sha256=<hex>` signs the raw body alone, as it
   *   always has.
   * - `X-CronWatch-Timestamp: <unix seconds>` is when it was sent, and
   *   `X-CronWatch-Timestamp-Signature: sha256=<hex>` signs
   *   `<timestamp>.<raw body>`, so a receiver that checks this one and
   *   refuses a timestamp more than five minutes from its own clock cannot
   *   be sent a captured alert again later.
   *
   * An empty string (an unset variable read as "", say) signs nothing, and
   * each send reports that to onError.
   */
  secret?: string;
  /** The clock the timestamp is read from, in milliseconds. Default Date.now. */
  now?: () => number;
}

const TIMEOUT_MS = 10_000;

/**
 * The payload's version, sent as its first field. It goes up only if a
 * major release changes the payload in a way that is not additive; see
 * https://cronwatch.dev/schemas/webhook/1.json.
 */
export const WEBHOOK_SCHEMA = 1;

function origin(url: string): string {
  try {
    return new URL(url).origin;
  } catch {
    return "(invalid URL)";
  }
}

/**
 * POSTs the alert as JSON to any URL. The body is the Alert object after a
 * version: { schema: 1, type, job, title, message, run, details, triage, at,
 * definition }. A redirect is an error: point the url at where the receiver
 * really is.
 */
export function webhook(options: WebhookOptions): AlertChannel {
  if (!options.url) throw new Error("webhook() needs a url");
  return {
    name: "webhook",
    async send(alert, context) {
      const body = JSON.stringify({ schema: WEBHOOK_SCHEMA, ...alert });
      if (options.secret === "") {
        context?.onError?.(new Error("webhook: secret is an empty string, so this request is sent unsigned; set the secret, or leave it out to send unsigned on purpose"));
      }
      const headers: Record<string, string> = { "content-type": "application/json", "user-agent": "cronwatch" };
      // A pasted Authorization value often carries a stray space or newline, which fetch would refuse.
      for (const [name, value] of Object.entries(options.headers ?? {})) setHeader(headers, name, typeof value === "string" ? value.trim() : value);
      if (options.secret) {
        const timestamp = String(Math.floor((options.now ?? Date.now)() / 1000));
        setHeader(headers, "x-cronwatch-signature", `sha256=${await signature(options.secret, body)}`);
        setHeader(headers, "x-cronwatch-timestamp", timestamp);
        setHeader(headers, "x-cronwatch-timestamp-signature", `sha256=${await signature(options.secret, `${timestamp}.${body}`)}`);
      }
      // A redirect is refused, not followed: the headers (and the signature) would go with it.
      const response = await fetch(postable(options.url), { method: "POST", headers, body, redirect: "error", signal: AbortSignal.timeout(TIMEOUT_MS) });
      // Only the origin: a webhook URL's path or query often is the credential.
      if (!response.ok) throw new Error(`Webhook ${origin(options.url)} answered ${response.status}`);
      await drain(response);
    },
  };
}

/** Sets a header, first taking out any of the same name in another case. */
function setHeader(headers: Record<string, string>, name: string, value: string): void {
  const lower = name.toLowerCase();
  for (const key of Object.keys(headers)) if (key.toLowerCase() === lower) delete headers[key];
  headers[name] = value;
}

/**
 * The webhook's signature of a body: the HMAC-SHA256 of the body with the
 * secret, as lowercase hex. A request carries it as
 * `X-CronWatch-Signature: sha256=<signature>`, so a receiver checks one with
 * the raw body it got (before any JSON parsing) and a constant-time compare.
 * The timestamped one is the signature of `<timestamp>.<body>`, with the
 * X-CronWatch-Timestamp value as sent.
 * Web Crypto, so it runs on Workers and Deno too. Every port has it, named
 * the same.
 */
export async function signature(secret: string, body: string): Promise<string> {
  const encoder = new TextEncoder();
  const key = await crypto.subtle.importKey("raw", encoder.encode(secret), { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
  const signature = new Uint8Array(await crypto.subtle.sign("HMAC", key, encoder.encode(body)));
  return Array.from(signature, (b) => b.toString(16).padStart(2, "0")).join("");
}
