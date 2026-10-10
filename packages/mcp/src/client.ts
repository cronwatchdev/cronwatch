/** A small HTTP client for the JSON API that @cronwatch/sdk mounts. */
export interface ApiClientOptions {
  /** Where the routes are mounted, e.g. https://app.example.com/cronwatch */
  baseUrl: string;
  token: string | null;
  fetch?: typeof fetch;
  /** How long a request may take before it is given up on, in milliseconds. Default 30 seconds; a check gets CHECK_TIMEOUT_MS. */
  timeoutMs?: number;
}

/** How long a request waits for the dashboard by default. */
const TIMEOUT_MS = 30_000;
/** How long a check waits: it sends alerts, each with up to 25 seconds of triage and 15 of channels. */
const CHECK_TIMEOUT_MS = 120_000;

export class ApiError extends Error {
  constructor(message: string, readonly status: number) {
    super(message);
    this.name = "ApiError";
  }
}

export class ApiClient {
  private readonly base: string;
  private readonly token: string | null;
  private readonly fetchFn: typeof fetch;
  private readonly timeoutMs: number | undefined;

  constructor(options: ApiClientOptions) {
    this.base = options.baseUrl.replace(/\/+$/, "");
    this.token = options.token;
    this.fetchFn = options.fetch ?? fetch;
    this.timeoutMs = options.timeoutMs;
  }

  async call<T = unknown>(method: string, path: string, body?: unknown): Promise<T> {
    const headers: Record<string, string> = { accept: "application/json" };
    if (this.token) headers.authorization = `Bearer ${this.token}`;
    if (body !== undefined) headers["content-type"] = "application/json";
    // A dashboard that takes the connection and never answers would hang the tool call.
    const timeout = this.timeoutMs ?? (path === "/check" ? CHECK_TIMEOUT_MS : TIMEOUT_MS);
    let response: Response;
    try {
      response = await this.fetchFn(`${this.base}/api${path}`, {
        method,
        headers,
        body: body === undefined ? undefined : JSON.stringify(body),
        signal: AbortSignal.timeout(timeout),
      });
    } catch (e) {
      if ((e as Error)?.name === "TimeoutError") throw new ApiError(`${method} ${path} got no answer within ${timeout / 1000} seconds`, 0);
      throw e;
    }
    const text = await response.text();
    let data: unknown = null;
    try {
      data = text ? JSON.parse(text) : null;
    } catch {
      throw new ApiError(`${method} ${path} answered ${response.status} with a non-JSON body: ${text.slice(0, 200)}`, response.status);
    }
    if (!response.ok) {
      const message = typeof data === "object" && data && "error" in data ? String((data as { error: unknown }).error) : response.statusText;
      throw new ApiError(`${method} ${path} failed (${response.status}): ${message}`, response.status);
    }
    return data as T;
  }
}
