/** What the CLI says about CRONWATCH_URL before it starts: why it is refused, or a warning to print. */
export interface BaseUrlVerdict {
  /** Why the URL is refused; the server does not start. */
  error?: string;
  /** Said on stderr, and the server starts. */
  warning?: string;
}

const LOCAL_HOSTS = ["localhost", "127.0.0.1", "[::1]"];

/**
 * Checks where the routes are said to be mounted. Refused: what is not a URL,
 * a scheme other than http or https, a user name or password (fetch refuses
 * them, and they would be printed), a query or fragment (the API's paths are
 * added to the end), and plain http to another machine unless allowed.
 * Nothing it says repeats a user name, password, query, or fragment.
 */
export function checkBaseUrl(raw: string, allowHttp: boolean): BaseUrlVerdict {
  let url: URL;
  try {
    url = new URL(raw);
  } catch {
    // Not echoed: what does not parse may still hold a password.
    return { error: "CRONWATCH_URL is not a URL; give where cw.routes() is mounted, e.g. https://app.example.com/cronwatch" };
  }
  const shown = `${url.protocol}//${url.host}${url.pathname}`;
  if (url.protocol !== "http:" && url.protocol !== "https:") {
    return { error: `CRONWATCH_URL must be an http or https URL, not ${url.protocol}//; give where cw.routes() is mounted, e.g. https://app.example.com/cronwatch` };
  }
  if (url.username || url.password) {
    return { error: `CRONWATCH_URL (${shown}) carries a user name or password, which is not supported; the dashboard is reached with CRONWATCH_TOKEN alone.` };
  }
  if (url.search || url.hash) {
    return { error: `CRONWATCH_URL (${shown}) has a query or fragment; give only where cw.routes() is mounted, e.g. https://app.example.com/cronwatch` };
  }
  if (url.protocol === "http:" && !LOCAL_HOSTS.includes(url.hostname)) {
    if (!allowHttp) {
      return { error: `${shown} is plain http, so the token would travel unencrypted; use https. For a private network you trust, pass --allow-http or set CRONWATCH_ALLOW_HTTP=1.` };
    }
    return { warning: `${shown} is plain http, so the token travels unencrypted (allowed by --allow-http or CRONWATCH_ALLOW_HTTP).` };
  }
  return {};
}
