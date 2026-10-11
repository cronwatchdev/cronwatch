import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { test } from "node:test";
import { cronwatch } from "../src/index.js";
import { constantTimeEqual } from "../src/http.js";
import { capture, clock, HOUR } from "./helpers.js";

function app(onError?: (error: unknown, where: string) => void) {
  const c = clock();
  const cw = cronwatch({ now: c.now, alerts: [capture()], cronSecret: null, onError });
  const routes = cw.routes({ token: "tok", basePath: "/cronwatch" });
  const send = (method: string, path: string, headers: Record<string, string> = {}, body?: string) =>
    routes.handler(new Request(`http://app.test${path}`, { method, headers, body }));
  const cookie = { cookie: `cronwatch_token=${createHash("sha256").update("cronwatch-cookie:tok").digest("hex")}` };
  const bearer = { authorization: "Bearer tok" };
  return { cw, c, routes, send, cookie, bearer };
}

test("cross-site writes are refused whatever the credentials", async () => {
  const { cw, send, cookie, bearer } = app();
  await cw.run("x", async () => {});
  const form = { "content-type": "application/x-www-form-urlencoded" };
  const foreign: Record<string, string>[] = [
    { origin: "https://evil.example" },
    { origin: "null" },
    { "sec-fetch-site": "cross-site" },
    { "sec-fetch-site": "same-site" },
    { origin: "http://app.test", "sec-fetch-site": "cross-site" },
  ];
  for (const headers of foreign) {
    assert.equal((await send("POST", "/cronwatch/jobs/x/silence", { ...cookie, ...form, ...headers }, "for=1h")).status, 403, JSON.stringify(headers));
    assert.equal((await send("POST", "/cronwatch/api/check", { ...bearer, ...headers })).status, 403, JSON.stringify(headers));
    assert.equal((await send("DELETE", "/cronwatch/api/jobs/x", { ...cookie, ...headers })).status, 403, JSON.stringify(headers));
  }
  assert.notEqual(await cw.jobSummary("x"), null);
  assert.equal((await cw.jobSummary("x"))!.silencedUntil, null);
});

test("same-origin forms and header-less API clients still write", async () => {
  const { cw, send, cookie, bearer } = app();
  await cw.run("x", async () => {});
  const sameOrigin = { origin: "http://app.test", "sec-fetch-site": "same-origin", referer: "http://app.test/cronwatch/jobs/x" };
  const check = await send("POST", "/cronwatch/check", { ...cookie, ...sameOrigin });
  assert.equal(check.status, 303, "the dashboard's Run check now button");
  const silence = await send("POST", "/cronwatch/jobs/x/silence", { ...cookie, ...sameOrigin, "content-type": "application/x-www-form-urlencoded" }, "for=4h");
  assert.equal(silence.status, 303);
  assert.equal(silence.headers.get("location"), "http://app.test/cronwatch/jobs/x");
  assert.equal((await send("POST", "/cronwatch/api/jobs/x/unsilence", bearer)).status, 200);
  assert.equal((await send("POST", "/cronwatch/api/check", { ...bearer, "sec-fetch-site": "none" })).status, 200);
});

test("GET /api/check runs only for a bearer; cookies must POST", async () => {
  const { send, cookie, bearer } = app();
  const viaCookie = await send("GET", "/cronwatch/api/check", cookie);
  assert.equal(viaCookie.status, 405);
  assert.equal(viaCookie.headers.get("allow"), "POST");
  assert.equal((await send("POST", "/cronwatch/api/check", cookie)).status, 200);
  assert.equal((await send("GET", "/cronwatch/api/check", bearer)).status, 200);
});

test("?token= is only accepted on an HTML GET", async () => {
  const { cw, send } = app();
  await cw.run("x", async () => {});
  assert.equal((await send("GET", "/cronwatch/api/jobs?token=tok")).status, 401);
  assert.equal((await send("GET", "/cronwatch/api/jobs/x?token=tok")).status, 401);
  assert.equal((await send("POST", "/cronwatch/api/check?token=tok")).status, 401);
  assert.equal((await send("POST", "/cronwatch/check?token=tok")).status, 401);
  assert.equal((await send("POST", "/cronwatch/jobs/x/forget?token=tok")).status, 401);
  assert.notEqual(await cw.jobSummary("x"), null);
  assert.equal((await send("GET", "/cronwatch/jobs/x?token=tok")).status, 303);
});

test("malformed cookies and paths are answered, not thrown", async () => {
  const { send, bearer } = app();
  assert.equal((await send("GET", "/cronwatch/", { cookie: "cronwatch_token=%E0%A4%A" })).status, 401);
  assert.equal((await send("GET", "/cronwatch/api/jobs", { cookie: "cronwatch_token=%" })).status, 401);
  assert.equal((await send("GET", "/cronwatch/jobs/%E0%A4%A", bearer)).status, 400);
  const api = await send("GET", "/cronwatch/api/jobs/%zz", bearer);
  assert.equal(api.status, 400);
  assert.equal((await api.json()).ok, false);
  assert.equal((await send("POST", "/cronwatch/api/jobs/%zz/silence", bearer)).status, 400);
});

test("?runs= is clamped to a whole number in range", async () => {
  const { cw, send, bearer } = app();
  for (let i = 0; i < 3; i++) await cw.run("r", async () => {});
  const count = async (runs: string) => (await (await send("GET", `/cronwatch/api/jobs/r?runs=${runs}`, bearer)).json()).runs.length;
  assert.equal(await count("0"), 1);
  assert.equal(await count("-5"), 1);
  assert.equal(await count("2.7"), 2);
  assert.equal(await count("abc"), 3);
  assert.equal(await count(""), 3);
  assert.equal(await count("1e9"), 3);
  assert.equal(await count("Infinity"), 3);
});

test("an unexpected error is a generic 500, reported through onError", async () => {
  const reported: [unknown, string][] = [];
  const { cw, send, bearer } = app((error, where) => reported.push([error, where]));
  cw.jobs = async () => { throw new Error("secret connection string"); };
  cw.jobsWithRuns = async () => { throw new Error("secret connection string"); };
  const api = await send("GET", "/cronwatch/api/jobs", bearer);
  assert.equal(api.status, 500);
  const body = await api.text();
  assert.doesNotMatch(body, /secret/);
  assert.deepEqual(JSON.parse(body), { ok: false, error: "Internal error" });
  const page = await send("GET", "/cronwatch/", bearer);
  assert.equal(page.status, 500);
  assert.match(page.headers.get("content-type")!, /text\/html/);
  assert.doesNotMatch(await page.text(), /secret/);
  assert.equal(reported.length, 2);
  assert.equal(reported[0]![1], "routes");
  assert.match((reported[0]![0] as Error).message, /secret connection string/);
});

test("a throwing onError still yields a 500", async () => {
  const { cw, send, bearer } = app(() => { throw new Error("logger down"); });
  cw.jobs = async () => { throw new Error("boom"); };
  assert.equal((await send("GET", "/cronwatch/api/jobs", bearer)).status, 500);
});

test("silence durations: strings are validated, numbers are milliseconds", async () => {
  const { cw, c, send, bearer } = app();
  await cw.run("s", async () => {});
  const json = { ...bearer, "content-type": "application/json" };
  const silence = (body: unknown) => send("POST", "/cronwatch/api/jobs/s/silence", json, JSON.stringify(body));

  for (const bad of ["forever", "2 hours", "", "-5", "1h then some"]) {
    const res = await silence({ for: bad });
    assert.equal(res.status, 400, bad);
    const body = await res.json();
    assert.equal(body.ok, false);
    assert.match(body.error, /silence duration/, bad);
  }
  assert.equal((await cw.jobSummary("s"))!.silencedUntil, null, "a bad duration silences nothing");

  const until = async (body: unknown) => (await (await silence(body)).json()).job.silencedUntil - c.now();
  assert.equal(await until({ for: 7_200_000 }), 7_200_000);
  assert.equal(await until({ for: "60000" }), 60_000);
  assert.equal(await until({ for: "90m" }), 90 * 60_000);
  assert.equal(await until({}), HOUR);
  const viaQuery = await send("POST", "/cronwatch/api/jobs/s/silence?for=forever", bearer);
  assert.equal(viaQuery.status, 400);
});

test("the silence form shows an error for a bad duration and 404s a missing job", async () => {
  const { cw, send, cookie } = app();
  await cw.run("s", async () => {});
  const form = { ...cookie, "content-type": "application/x-www-form-urlencoded" };
  const bad = await send("POST", "/cronwatch/jobs/s/silence", form, "for=forever");
  assert.equal(bad.status, 400);
  assert.match(bad.headers.get("content-type")!, /text\/html/);
  assert.match(await bad.text(), /silence duration &quot;forever&quot;/);
  assert.equal((await cw.jobSummary("s"))!.silencedUntil, null);
  assert.equal((await send("POST", "/cronwatch/jobs/ghost/silence", form, "for=1h")).status, 404);
  assert.equal((await send("POST", "/cronwatch/jobs/ghost/unsilence", form)).status, 404);
  assert.equal((await send("POST", "/cronwatch/jobs/s/explode", form)).status, 404);
});

test("pages carry a strict CSP and security headers, and need no script of their own", async () => {
  const { cw, send, bearer } = app();
  await cw.run("h", async () => {});
  for (const path of ["/cronwatch/", "/cronwatch/jobs/h", "/cronwatch/nope"]) {
    const res = await send("GET", path, bearer);
    const csp = res.headers.get("content-security-policy")!;
    assert.match(csp, /default-src 'none'/);
    assert.match(csp, /frame-ancestors 'none'/);
    assert.match(csp, /form-action 'self'/);
    // Scripts only from the dashboard itself (app.js, which registers the service worker).
    assert.match(csp, /script-src 'self';/);
    assert.doesNotMatch(csp, /unsafe-eval|script-src[^;]*unsafe-inline/);
    assert.equal(res.headers.get("x-frame-options"), "DENY");
    assert.equal(res.headers.get("x-content-type-options"), "nosniff");
    assert.equal(res.headers.get("referrer-policy"), "same-origin");
    assert.equal(res.headers.get("cache-control"), "no-store");
    const html = await res.text();
    assert.deepEqual(html.match(/<script[^>]*>[^<]*<\/script>/gi), [`<script src="/cronwatch/app.js" defer></script>`], "one script, external, empty");
    assert.equal(html.match(/<script/gi)!.length, 1);
    assert.doesNotMatch(html, /\son[a-z]+=/i, "no inline event handlers");
  }
  const page = await (await send("GET", "/cronwatch/jobs/h", bearer)).text();
  assert.match(page, /<details class="confirm"><summary>Delete history<\/summary><form/, "forget confirms without script");
  const api = await send("GET", "/cronwatch/api/jobs", bearer);
  assert.equal(api.headers.get("x-content-type-options"), "nosniff");
  assert.equal(api.headers.get("cache-control"), "no-store");
});

test("markup in definitions, output, and metrics stays escaped on every page", async () => {
  const { cw, send, bearer } = app();
  const job = cw.job("m", { schedule: "0 2 * * *", description: "<img src=x>", tags: ["<t>"], expect: "<e>" });
  await job.run(async (j) => { j.log("<o>"); j.metric("<k>", 1); });
  for (const path of ["/cronwatch/", "/cronwatch/jobs/m", "/cronwatch/jobs/%3Cx%3E"]) {
    const html = await (await send("GET", path, bearer)).text();
    assert.doesNotMatch(html, /<img|<t>|<e>|<o>|<k>|<x>/, path);
  }
});

/** No token in development: the routes as a dev server would serve them, with the sign-in line swallowed. */
function devRoutes() {
  const before = { NODE_ENV: process.env.NODE_ENV, CRONWATCH_TOKEN: process.env.CRONWATCH_TOKEN };
  process.env.NODE_ENV = "development";
  delete process.env.CRONWATCH_TOKEN;
  try {
    const cw = cronwatch({ alerts: [capture()], cronSecret: null });
    return cw.routes({ basePath: "/cronwatch" });
  } finally {
    for (const [k, v] of Object.entries(before)) {
      if (v === undefined) delete process.env[k]; else process.env[k] = v;
    }
  }
}

test("without a token in development, nothing a request says about itself lets it in", async () => {
  const routes = devRoutes();
  const info = console.info;
  console.info = () => {};
  try {
    const local = "http://localhost:3000/cronwatch/api/jobs";
    // What Next.js hands a route handler for a browser on this machine, and
    // what the old loopback check let through: all of it can be forged, since
    // Next.js keeps a client's X-Forwarded-For and a tunnel rewrites Host.
    const looksLocal: Record<string, string>[] = [
      {},
      { host: "localhost:3000", "x-forwarded-host": "localhost:3000", "x-forwarded-for": "::ffff:127.0.0.1", "x-forwarded-port": "3000", "x-forwarded-proto": "http" },
      { host: "127.0.0.1:3000", "x-forwarded-for": "::1" },
      { host: "[::1]:3000", forwarded: 'for="[::1]:51234";host=localhost;proto=http' },
      { host: "localhost", "x-real-ip": "127.0.0.1" },
    ];
    for (const headers of looksLocal) {
      const res = await routes.GET(new Request(local, { headers }));
      assert.equal(res.status, 401, JSON.stringify(headers));
      assert.equal((await res.json()).ok, false);
    }
    const write = await routes.POST(new Request("http://localhost:3000/cronwatch/api/check", { method: "POST", headers: { host: "localhost:3000" } }));
    assert.equal(write.status, 401);
  } finally {
    console.info = info;
  }
});


/** Runs fn with these variables set (undefined unsets one), then puts them back. */
async function withEnv(values: Record<string, string | undefined>, fn: () => Promise<void>): Promise<void> {
  const before = Object.fromEntries(Object.keys(values).map((k) => [k, process.env[k]]));
  for (const [k, v] of Object.entries(values)) {
    if (v === undefined) delete process.env[k]; else process.env[k] = v;
  }
  try {
    await fn();
  } finally {
    for (const [k, v] of Object.entries(before)) {
      if (v === undefined) delete process.env[k]; else process.env[k] = v;
    }
  }
}

const PRODUCTION = { CRONWATCH_ENV: undefined, APP_ENV: undefined, NODE_ENV: "production" };

test("a CRONWATCH_TOKEN or token of only whitespace counts as unset, so the routes stay locked", async () => {
  for (const blank of ["", " ", "  ", "\t", " \n  ﻿ "]) {
    await withEnv({ ...PRODUCTION, CRONWATCH_TOKEN: blank }, async () => {
      for (const routes of [cronwatch({ alerts: [capture()], cronSecret: null }).routes(), cronwatch({ alerts: [capture()], cronSecret: null }).routes({ token: blank })]) {
        const label = JSON.stringify(blank);
        assert.equal((await routes.GET(new Request("http://app.test/cronwatch/api/jobs"))).status, 503, label);
        const signIn = await routes.GET(new Request(`http://app.test/cronwatch/?token=${encodeURIComponent(blank)}`));
        assert.equal(signIn.status, 503, label);
        assert.equal(signIn.headers.get("set-cookie"), null, label);
        assert.equal((await routes.GET(new Request("http://app.test/cronwatch/api/jobs", { headers: { authorization: "Bearer  " } }))).status, 503, label);
      }
    });
  }
  // A blank token given in code falls back to the variable, as an empty one always has.
  await withEnv({ ...PRODUCTION, CRONWATCH_TOKEN: "from-env" }, async () => {
    const routes = cronwatch({ alerts: [capture()], cronSecret: null }).routes({ token: "  " });
    assert.equal((await routes.GET(new Request("http://app.test/cronwatch/api/jobs", { headers: { authorization: "Bearer from-env" } }))).status, 200);
  });
  // Anything else is trimmed, so a value read from a file with a trailing
  // newline still matches the header a client sends, which HTTP trims.
  for (const padded of [" padded ", "padded\n"]) {
    await withEnv({ ...PRODUCTION, CRONWATCH_TOKEN: padded }, async () => {
      for (const routes of [cronwatch({ alerts: [capture()], cronSecret: null }).routes(), cronwatch({ alerts: [capture()], cronSecret: null }).routes({ token: padded })]) {
        assert.equal((await routes.GET(new Request("http://app.test/cronwatch/api/jobs", { headers: { authorization: "Bearer padded" } }))).status, 200);
        assert.equal((await routes.GET(new Request("http://app.test/cronwatch/?token=padded"))).status, 303);
      }
    });
  }
});

test("a test or testing environment is not development: no token of its own, and a handler with no secret refuses", async () => {
  for (const values of [{ NODE_ENV: "test" }, { APP_ENV: "testing" }, { CRONWATCH_ENV: " Test " }, { APP_ENV: "test", NODE_ENV: "development" }]) {
    await withEnv({ ...PRODUCTION, NODE_ENV: undefined, ...values, CRONWATCH_TOKEN: undefined, CRON_SECRET: undefined }, async () => {
      const cw = cronwatch({ alerts: [capture()] });
      const label = JSON.stringify(values);
      assert.equal((await cw.routes().GET(new Request("http://localhost:3000/cronwatch/api/jobs"))).status, 503, label);
      const handler = cw.job("nightly").handler(() => "ran");
      assert.equal((await handler(new Request("http://localhost:3000/api/cron/nightly"))).status, 503, label);
    });
  }
});

test("a token given in code that is not a string or null throws, so it never becomes a password", () => {
  const cw = cronwatch({ alerts: [capture()], cronSecret: null });
  for (const token of [false, true, 0, 5, {}, ["tok"]]) {
    assert.throws(() => cw.routes({ token: token as unknown as string }), /^TypeError: routes: token must be a string, or null to opt out, not (boolean|number|object|an array)$/, String(token));
  }
});

test("an Authorization header that is not a bearer (a proxy's Basic auth) leaves the cookie and ?token= to sign in", async () => {
  const { send, cookie } = app();
  const basic = { authorization: "Basic dXNlcjpwYXNz" };
  assert.equal((await send("GET", "/cronwatch/api/jobs", { ...basic, ...cookie })).status, 200);
  assert.equal((await send("GET", "/cronwatch/", { ...basic, ...cookie })).status, 200);
  assert.equal((await send("GET", "/cronwatch/api/jobs", basic)).status, 401);
  const link = await send("GET", "/cronwatch/jobs/x?token=tok", basic);
  assert.equal(link.status, 303);
  assert.ok(link.headers.get("set-cookie"));
  // Not a bearer, so GET /api/check does not run on its strength.
  assert.equal((await send("GET", "/cronwatch/api/check", { ...basic, ...cookie })).status, 405);
  // A bearer is matched whatever the scheme's case, and with any whitespace after it.
  for (const authorization of ["Bearer tok", "bearer tok", "BEARER\ttok", "Bearer   tok"]) {
    assert.equal((await send("GET", "/cronwatch/api/jobs", { authorization })).status, 200, authorization);
  }
  // A wrong bearer still wins over a good cookie; "Bearer" with nothing after it, or run on, is no bearer.
  assert.equal((await send("GET", "/cronwatch/api/jobs", { authorization: "Bearer wrong", ...cookie })).status, 401);
  assert.equal((await send("GET", "/cronwatch/api/jobs", { authorization: "Bearer", ...cookie })).status, 200);
  assert.equal((await send("GET", "/cronwatch/api/jobs", { authorization: "Bearertok", ...cookie })).status, 200);
});

test("a request body past 1 MiB is answered 413, or read as no token on the sign-in form, and nothing is silenced", async () => {
  const { cw, send, bearer } = app();
  await cw.run("x", async () => {});
  const json = { "content-type": "application/json" };
  const form = { "content-type": "application/x-www-form-urlencoded" };
  const pad = "x".repeat(1 << 20);
  const api = await send("POST", "/cronwatch/api/jobs/x/silence", { ...bearer, ...json }, JSON.stringify({ for: "2h", pad }));
  assert.equal(api.status, 413);
  assert.deepEqual(await api.json(), { ok: false, error: "Request body too large" });
  const page = await send("POST", "/cronwatch/jobs/x/silence", { ...bearer, ...form }, `for=2h&pad=${pad}`);
  assert.equal(page.status, 413);
  assert.match(await page.text(), /The request was too large\./);
  // Without a Content-Length, read one chunk past the cap at most.
  const chunks = [new TextEncoder().encode(`{"for":"2h","pad":"`), ...Array.from({ length: 10 }, () => new TextEncoder().encode(pad)), new TextEncoder().encode(`"}`)];
  let pulled = 0;
  const stream = new ReadableStream<Uint8Array>({ pull(controller) { const next = chunks[pulled++]; if (next) controller.enqueue(next); else controller.close(); } });
  const streamed = await cw.routes({ token: "tok", basePath: "/cronwatch" }).handler(new Request("http://app.test/cronwatch/api/jobs/x/silence", { method: "POST", headers: { ...bearer, ...json }, body: stream, duplex: "half" } as RequestInit));
  assert.equal(streamed.status, 413);
  assert.ok(pulled <= 4, `stopped reading past the cap (pulled ${pulled} of ${chunks.length})`);
  assert.equal((await cw.jobSummary("x"))!.silencedUntil, null);
  // The sign-in form: a body too large is no token at all.
  const signIn = await send("POST", "/cronwatch/signin", form, `token=tok&pad=${pad}`);
  assert.equal(signIn.status, 401);
  assert.equal(signIn.headers.get("set-cookie"), null);
  // A body just under the cap is read.
  const fits = await send("POST", "/cronwatch/api/jobs/x/silence", { ...bearer, ...json }, JSON.stringify({ for: "2h", pad: "x".repeat((1 << 20) - 32) }));
  assert.equal(fits.status, 200);
});

test("the secret compare matches only the secret itself, whatever is sent", () => {
  for (const [sent, secret, equal] of [
    ["tok", "tok", true], ["tok", "tok2", false], ["tok2", "tok", false], ["toktok", "tok", false], ["", "tok", false],
    ["", "", true], ["x", "", false], ["\u{1F600}", "\u{1F600}", true], ["\u{1F600}", "\u{1F601}", false],
  ] as [string, string, boolean][]) {
    assert.equal(constantTimeEqual(sent, secret), equal, `${JSON.stringify(sent)} vs ${JSON.stringify(secret)}`);
  }
});

test("the ?token= sign-in never redirects off the site, however many slashes lead the path", async () => {
  const cw = cronwatch({ alerts: [capture()], cronSecret: null });
  const routes = cw.routes({ token: "tok", basePath: "/" });
  for (const path of ["//evil.example/x", "///evil.example", "/\\/evil.example"]) {
    const res = await routes.handler(new Request(`http://app.test${path}?token=tok`));
    assert.equal(res.status, 303, path);
    const location = res.headers.get("location")!;
    assert.ok(location.startsWith("/") && !location.startsWith("//"), `${path} -> ${location}`);
    assert.ok(!new URL(location, "http://app.test").host.includes("evil"), `${path} -> ${location}`);
  }
});

test("only the base and the paths under it are served; a look-alike prefix or a dot segment out of it is not", async () => {
  const { cw, send, bearer } = app();
  await cw.run("x", async () => {});
  for (const path of ["/cronwatchapi/jobs", "/cronwatch/../api/jobs", "/api/jobs", "/cronwatch-other/", "/", "/x/cronwatch/api/jobs"]) {
    const res = await send("GET", path, bearer);
    assert.equal(res.status, 404, path);
    assert.match(res.headers.get("content-type")!, /text\/html/, path);
    assert.doesNotMatch(await res.text(), /"jobs"/, path);
  }
  assert.equal((await send("GET", "/cronwatch/api/jobs", bearer)).status, 200);
  assert.equal((await send("GET", "/cronwatch/./api/jobs", bearer)).status, 200, "a dot segment that stays under the base");
  assert.equal((await send("GET", "/cronwatch/jobs/../api/jobs", bearer)).status, 200);
  assert.equal((await send("GET", "/cronwatch", bearer)).status, 200, "the base itself is the dashboard");
  assert.equal((await send("GET", "/cronwatch/", bearer)).status, 200);
  // Mounted at the root, every path is the routes'.
  const root = cw.routes({ token: "tok", basePath: "/" });
  assert.equal((await root.handler(new Request("http://app.test/api/jobs", { headers: bearer }))).status, 200);
});
