/**
 * Environment variables, read only where there is a `process` to read them
 * from. Cloudflare Workers without nodejs_compat have none: there every value
 * comes from the options, which is where a Worker passes its env bindings.
 */
export function readEnv(name: string): string | undefined {
  return typeof process === "undefined" || !process.env ? undefined : process.env[name];
}

/**
 * A secret from the environment (CRONWATCH_TOKEN, CRON_SECRET), trimmed (as
 * String.prototype.trim trims): a value read from a file often ends in a
 * newline that no header can carry, since HTTP strips the whitespace around
 * a header's value. Undefined when the variable is unset, empty, or only
 * whitespace, so a blank value counts as not set and the routes and handlers
 * fail closed.
 */
export function readSecretEnv(name: string): string | undefined {
  const value = readEnv(name)?.trim();
  return value === undefined || value === "" ? undefined : value;
}

/**
 * A token or secret passed in code: a string, null (the opt-out), or
 * undefined (not given). A string is trimmed, as readSecretEnv() trims, and
 * one that is empty or only whitespace counts as not given. Anything else
 * (false, a number, an object) throws a TypeError naming the option, so it
 * never becomes a password.
 */
export function secretOption(value: unknown, what: string): string | null | undefined {
  if (value === undefined || value === null) return value;
  if (typeof value !== "string") {
    throw new TypeError(`${what} must be a string, or null to opt out, not ${Array.isArray(value) ? "an array" : typeof value}`);
  }
  const trimmed = value.trim();
  return trimmed === "" ? undefined : trimmed;
}

/**
 * The environment's name from the values of the variables that name it, in
 * order, as every CronWatch library reads it: the first that holds more than
 * whitespace, trimmed and lowercased, with "prod" read as "production" and
 * "dev" and "local" as "development"; "" when none is set. "test" and
 * "testing" are not development: a test run or CI needs CRON_SECRET or a
 * token, as production does.
 */
export function environmentOf(values: readonly (string | undefined)[]): string {
  for (const value of values) {
    const name = typeof value === "string" ? value.trim().toLowerCase() : "";
    if (name === "") continue;
    if (name === "prod") return "production";
    if (name === "dev" || name === "local") return "development";
    return name;
  }
  return "";
}

/**
 * The environment's name (environmentOf) from CRONWATCH_ENV, APP_ENV, and
 * NODE_ENV. The variables are written out in full so that bundlers which
 * inline process.env.NODE_ENV still can.
 */
export function environment(): string {
  if (typeof process === "undefined" || !process.env) return "";
  return environmentOf([process.env.CRONWATCH_ENV, process.env.APP_ENV, process.env.NODE_ENV]);
}

/** Development makes a dashboard token of its own and lets a handler with no secret run. */
export function isDevelopment(): boolean {
  return environment() === "development";
}

/** Production warns that the default in-memory store forgets on restart. */
export function isProduction(): boolean {
  return environment() === "production";
}
