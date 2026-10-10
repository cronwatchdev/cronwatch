export function json(body: unknown, status = 200, headers: Record<string, string> = {}): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json; charset=utf-8", "cache-control": "no-store", ...headers },
  });
}

/**
 * Compares what a request `sent` with the `secret` without stopping at the
 * first differing character, and in time that depends only on the length of
 * what was sent, so neither the secret's characters nor its length can be
 * timed. Every UTF-16 code unit of `sent` is compared, against the secret's
 * unit at the same place (repeating it past its end), and a length that
 * differs counts as a difference.
 */
export function constantTimeEqual(sent: string, secret: string): boolean {
  if (secret.length === 0) return sent.length === 0;
  let diff = sent.length ^ secret.length;
  for (let i = 0; i < sent.length; i++) diff |= sent.charCodeAt(i) ^ secret.charCodeAt(i % secret.length);
  return diff === 0;
}
