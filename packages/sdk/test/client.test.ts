import assert from "node:assert/strict";
import { test } from "node:test";
import { cronwatch, memory } from "../src/index.js";
import { capture, clock, MIN, HOUR, settle, T0 } from "./helpers.js";

function make(options: Parameters<typeof cronwatch>[0] = {}) {
  const c = clock();
  const alerts = capture();
  const cw = cronwatch({ now: c.now, alerts: [alerts], cronSecret: null, ...options });
  return { cw, c, alerts };
}

test("run records output, metrics, and duration, and returns the result", async () => {
  const { cw, c } = make();
  const job = cw.job("report", { schedule: "0 2 * * *" });
  const result = await job.run(async (j) => {
    j.log("hello", { n: 1 });
    j.metric("rows", 42);
    c.advance(1500);
    return "done";
  });
  assert.equal(result, "done");
  const [run] = await cw.runs("report");
  assert.equal(run!.status, "ok");
  assert.equal(run!.durationMs, 1500);
  assert.equal(run!.output, 'hello {"n":1}');
  assert.deepEqual(run!.metrics, { rows: 42 });
  const summary = await cw.jobSummary("report");
  assert.equal(summary!.health, "healthy");
  assert.equal(summary!.nextExpectedAt, Date.UTC(2026, 0, 6, 2, 0));
});

test("a throwing job is recorded as failed, alerts, and rethrows", async () => {
  const { cw, alerts } = make();
  const job = cw.job("nightly");
  await assert.rejects(job.run(async () => { throw new Error("db down"); }), /db down/);
  const [run] = await cw.runs("nightly");
  assert.equal(run!.status, "failed");
  assert.match(run!.error!, /Error: db down/);
  assert.deepEqual(alerts.types(), ["failed"]);
  assert.match(alerts.alerts[0]!.message, /db down/);
  assert.equal((await cw.jobSummary("nightly"))!.health, "failing");
});

test("expect turns a quiet success into a failure", async () => {
  const { cw, c, alerts } = make();
  const job = cw.job("export", { expect: "wrote" });
  await job.run(async (j) => { j.log("wrote 12 files"); });
  assert.deepEqual(alerts.types(), []);
  c.advance(HOUR);
  await job.run(async (j) => { j.log("nothing to do"); });
  const [run] = await cw.runs("export");
  assert.equal(run!.status, "failed");
  assert.match(run!.error!, /did not contain "wrote"/);
  assert.deepEqual(alerts.types(), ["failed"]);
  // A returned string counts as output too.
  await job.run(async () => "wrote 3 files");
  assert.deepEqual(alerts.types(), ["failed", "recovered"]);
});

test("cw.run defines on first use and validates names and schedules", async () => {
  const { cw } = make();
  await cw.run("adhoc", { schedule: "every 5m" }, async () => 1);
  assert.equal((await cw.jobs()).length, 1);
  assert.throws(() => cw.job("bad name!"), /job name/);
  assert.throws(() => cw.job("x", { schedule: "nope" }), /not a cron expression/);
  assert.throws(() => cw.job("x", { grace: "soon" }), /grace/);
});

test("handler checks the bearer secret and reports the run", async () => {
  const { cw } = make({ cronSecret: "s3cret" });
  const handler = cw.job("hourly", { schedule: "@hourly" }).handler(async (j, req) => {
    j.log(new URL(req.url).pathname);
    if (req.headers.get("x-fail")) throw new Error("nope");
    return { fine: true };
  });
  const noAuth = await handler(new Request("http://x/api/cron/hourly"));
  assert.equal(noAuth.status, 401);
  const ok = await handler(new Request("http://x/api/cron/hourly", { headers: { authorization: "Bearer s3cret" } }));
  assert.equal(ok.status, 200);
  const body = await ok.json();
  assert.equal(body.ok, true);
  assert.equal(body.job, "hourly");
  const failed = await handler(new Request("http://x/api/cron/hourly", { headers: { authorization: "Bearer s3cret", "x-fail": "1" } }));
  assert.equal(failed.status, 500);
  assert.match((await failed.json()).error, /nope/);
  const runs = await cw.runs("hourly");
  assert.equal(runs.length, 2);
  assert.equal(runs[1]!.output, "/api/cron/hourly");
});

test("a handler returning a Response passes it through, and 4xx/5xx count as failure", async () => {
  const { cw, alerts } = make();
  const handler = cw.job("h").handler(async () => new Response("bad", { status: 503 }));
  const res = await handler(new Request("http://x/"));
  assert.equal(res.status, 503);
  assert.equal(await res.text(), "bad");
  assert.equal((await cw.runs("h"))[0]!.error, "HTTP 503");
  assert.deepEqual(alerts.types(), ["failed"]);
});

test("check finds a missed run, once, and a later run recovers", async () => {
  const { cw, c, alerts } = make();
  const job = cw.job("sync", { schedule: "every 1h", grace: "10m" });
  await cw.check(); // registers at T0
  c.advance(30 * MIN);
  assert.deepEqual((await cw.check()).alerts, []);
  c.set(T0 + 70 * MIN + 1);
  const r = await cw.check();
  assert.deepEqual(r.alerts.map((a) => a.type), ["missed"]);
  assert.equal(r.jobs[0]!.health, "late");
  assert.deepEqual((await cw.check()).alerts, [], "no repeat");
  await job.run(async () => {});
  assert.deepEqual(alerts.types(), ["missed", "recovered"]);
  assert.equal((await cw.jobSummary("sync"))!.health, "healthy");
});

test("a job declared again without its schedule closes missed with a recovery, once", async () => {
  const { cw, c, alerts } = make();
  cw.job("sync", { schedule: "every 1h", grace: "10m" });
  await cw.check();
  c.set(T0 + 70 * MIN + 1);
  assert.deepEqual((await cw.check()).alerts.map((a) => a.type), ["missed"]);
  const job = cw.job("sync");
  c.advance(MIN);
  const r = await cw.check();
  assert.deepEqual(r.alerts.map((a) => a.type), ["recovered"]);
  const alert = r.alerts[0]!;
  assert.equal(alert.title, "sync is no longer scheduled");
  assert.equal(alert.message, "Missed since 2026-01-05 10:40:00 UTC (1m ago). It has no schedule now, so nothing is due; the missed alert is closed.");
  assert.deepEqual(alert.details, { after: ["missed"], reason: "unscheduled", since: T0 + 70 * MIN + 1 });
  assert.equal(r.jobs[0]!.health, "never_ran");
  assert.deepEqual((await cw.check()).alerts, [], "no repeat");
  await job.run(async () => {});
  assert.deepEqual(alerts.types(), ["missed", "recovered"], "the next run owes nothing");
});

test("a schedule removed while silenced closes missed with its recovery, which the silence does not hold back", async () => {
  const { cw, c, alerts } = make();
  cw.job("sync", { schedule: "every 1h", grace: "10m" });
  await cw.check();
  c.set(T0 + 70 * MIN + 1);
  await cw.check();
  await cw.silence("sync", "1h");
  cw.job("sync");
  c.advance(MIN);
  assert.deepEqual((await cw.check()).alerts.map((a) => a.type), ["recovered"]);
  assert.deepEqual((await cw.jobSummary("sync"))!.open, []);
  c.advance(2 * HOUR);
  assert.deepEqual((await cw.check()).alerts, []);
  assert.deepEqual(alerts.types(), ["missed", "recovered"]);
});

test("check marks a run that never finished as stuck", async () => {
  const { cw, c, alerts } = make();
  const job = cw.job("long", { timeout: "5m" });
  const pending = job.run(() => new Promise<void>(() => {}));
  void pending;
  await new Promise((r) => setImmediate(r));
  assert.equal((await cw.runs("long"))[0]!.status, "running");
  c.advance(4 * MIN);
  assert.deepEqual((await cw.check()).alerts, []);
  c.advance(2 * MIN);
  const r = await cw.check();
  assert.deepEqual(r.alerts.map((a) => a.type), ["stuck"]);
  assert.equal((await cw.runs("long"))[0]!.status, "timeout");
  assert.equal(r.jobs[0]!.health, "stuck");
  assert.match(alerts.alerts[0]!.message, /never reported finishing/);
});

test("slow and over-budget alerts come from the job's own baseline", async () => {
  const { cw, c, alerts } = make();
  const job = cw.job("agent", { budget: { cost: 1 } });
  for (let i = 0; i < 5; i++) {
    await job.run(async (j) => { c.advance(1000); j.metrics({ tokens: 1000, cost: 0.5 }); });
    c.advance(HOUR);
  }
  assert.deepEqual(alerts.types(), []);
  await job.run(async (j) => { c.advance(15_000); j.metrics({ tokens: 1000, cost: 0.5 }); });
  assert.deepEqual(alerts.types(), ["slow"]);
  c.advance(HOUR);
  await job.run(async (j) => { c.advance(1000); j.metrics({ tokens: 5000, cost: 1.2 }); });
  assert.deepEqual(alerts.types(), ["slow", "over_budget"]);
  const last = alerts.alerts[1]!;
  assert.match(last.message, /cost: 1\.2, limit 1 \(budget\)/);
  assert.match(last.message, /tokens: 5,000, limit 3,000 \(three times the usual 1,000\)/);
  c.advance(HOUR);
  await job.run(async (j) => { c.advance(1000); j.metrics({ tokens: 1000, cost: 0.5 }); });
  assert.deepEqual(alerts.types(), ["slow", "over_budget", "recovered"]);
});

test("an under-floor alert names the metric and what it was judged against", async () => {
  const { cw, c, alerts } = make();
  const job = cw.job("import", { floor: { files: 1 } });
  for (let i = 0; i < 5; i++) {
    await job.run(async (j) => { c.advance(1000); j.metrics({ rows: 4812 + i, files: 2 }); });
    c.advance(HOUR);
  }
  await job.run(async (j) => { c.advance(1000); j.metrics({ rows: 0, files: 0 }); });
  assert.deepEqual(alerts.types(), ["under_floor"]);
  const alert = alerts.alerts[0]!;
  assert.equal(alert.title, "import fell short");
  assert.match(alert.message, /rows: 0 \(the last 5 runs all reported more than 0, the lowest 4,812\)/);
  assert.match(alert.message, /files: 0, below the floor of 1\./);
  c.advance(HOUR);
  await job.run(async (j) => { c.advance(1000); j.metrics({ rows: 0, files: 0 }); });
  assert.deepEqual(alerts.types(), ["under_floor"]);
  c.advance(HOUR);
  await job.run(async (j) => { c.advance(1000); j.metrics({ rows: 10, files: 1 }); });
  assert.deepEqual(alerts.types(), ["under_floor", "recovered"]);
});

test("a floor must be a finite number, and no higher than its ceiling", () => {
  const { cw } = make();
  assert.throws(() => cw.job("a", { floor: { rows: Number.NaN } }), /floor\.rows must be a finite number/);
  assert.throws(() => cw.job("b", { floor: { cost: 3 }, budget: { cost: 2 } }), /floor\.cost \(3\) is above budget\.cost \(2\)/);
  cw.job("c", { floor: { delta: -5 }, budget: { delta: 5 } });
});

test("silence swallows alerts and nothing opens underneath; unsilence alerts again", async () => {
  const { cw, c, alerts } = make();
  const job = cw.job("flaky");
  await cw.silence("flaky", "1h");
  await assert.rejects(job.run(async () => { throw new Error("x"); }));
  assert.deepEqual(alerts.types(), []);
  assert.equal((await cw.jobSummary("flaky"))!.health, "silenced");
  await cw.unsilence("flaky");
  await assert.rejects(job.run(async () => { throw new Error("y"); }));
  assert.deepEqual(alerts.types(), ["failed"]);
  c.advance(1);
});

test("a recovery for an alert sent before the silence is sent during it; new problems are not", async () => {
  const { cw, alerts } = make();
  const job = cw.job("flaky");
  await assert.rejects(job.run(async () => { throw new Error("x"); }));
  await cw.silence("flaky", "1h");
  await job.run(() => "ok");
  assert.deepEqual(alerts.types(), ["failed", "recovered"]);
  await assert.rejects(job.run(async () => { throw new Error("y"); }));
  assert.deepEqual(alerts.types(), ["failed", "recovered"]);
});

test("a silence ends on a whole millisecond, never past 2^53 - 1", async () => {
  const { cw, c } = make();
  cw.job("long");
  assert.equal((await cw.silence("long", "99999999999999999999w")).silencedUntil, Number.MAX_SAFE_INTEGER);
  assert.equal((await cw.silence("long", 1e300)).silencedUntil, Number.MAX_SAFE_INTEGER);
  assert.equal((await cw.silence("long", 1.5)).silencedUntil, c.now() + 1);
  const routes = cw.routes({ token: null, basePath: "/cronwatch" });
  const res = await routes.POST(new Request("http://app.test/cronwatch/api/jobs/long/silence", {
    method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify({ for: "99999999999999999999w" }),
  }));
  assert.equal(res.status, 200);
  assert.equal((await res.json()).job.silencedUntil, Number.MAX_SAFE_INTEGER);
});

test("triage output is attached to failure alerts and never blocks them", async () => {
  const { cw, alerts } = make({ triage: async ({ alert }) => `Probably ${alert.job}'s database.` });
  await assert.rejects(cw.run("t", async () => { throw new Error("x"); }));
  assert.equal(alerts.alerts[0]!.triage, "Probably t's database.");
  const errors: string[] = [];
  const { cw: cw2, alerts: alerts2 } = make({ triage: async () => { throw new Error("api down"); }, onError: (e, where) => errors.push(where) });
  await assert.rejects(cw2.run("t", async () => { throw new Error("x"); }));
  assert.deepEqual(alerts2.types(), ["failed"]);
  assert.equal(alerts2.alerts[0]!.triage, null, "tried, and gave nothing");
  assert.deepEqual(errors, ["triage for t"]);
});

test("forget removes the job and its runs", async () => {
  const { cw } = make();
  await cw.run("gone", async () => {});
  assert.equal((await cw.jobs()).length, 1);
  await cw.forget("gone");
  assert.equal((await cw.jobs()).length, 0);
  assert.equal(await cw.jobSummary("gone"), null);
});

test("after a forget, cw.run() and cw.resumeRun() by name declare the job again as it was written, not bare", async () => {
  const { cw, c, alerts } = make();
  cw.job("nightly", { schedule: "0 2 * * *", description: "Nightly export" });
  await cw.run("nightly", async () => {});
  const handle = await cw.job("nightly", { schedule: "0 2 * * *", description: "Nightly export" }).start();
  await cw.forget("nightly");
  assert.equal(await cw.jobSummary("nightly"), null);
  await cw.run("nightly", async () => {});
  const summary = (await cw.jobSummary("nightly"))!;
  assert.equal(summary.definition.schedule, "0 2 * * *");
  assert.equal(summary.definition.description, "Nightly export");
  await cw.forget("nightly");
  const resumed = await cw.resumeRun("nightly", handle.id);
  assert.equal(resumed.id, handle.id);
  c.advance(HOUR);
  await cw.check();
  assert.equal((await cw.jobSummary("nightly"))!.definition.schedule, "0 2 * * *");
  assert.ok(!alerts.types().includes("recovered"), "no 'no longer scheduled' recovery");
});

test("a job that hangs on every run alerts stuck once, and recovers on its first success", async () => {
  const { cw, c, alerts } = make();
  const job = cw.job("hangs", { schedule: "*/5 * * * *", timezone: "UTC", timeout: "10m" });
  // Twelve runs, five minutes apart, none of which ever finishes.
  const handles = [];
  for (let i = 0; i < 12; i++) {
    handles.push(await job.start());
    c.advance(5 * MIN);
    await cw.check();
  }
  assert.deepEqual(alerts.types(), ["stuck"]);
  assert.equal((await cw.jobSummary("hangs"))!.health, "stuck");
  // A run that fails alerts as failed by its own rule; stuck stays open beside it.
  await job.run(async () => { throw new Error("boom"); }).catch(() => {});
  assert.deepEqual(alerts.types(), ["stuck", "failed"]);
  assert.deepEqual((await cw.jobSummary("hangs"))!.open.sort(), ["failed", "stuck"]);
  // A hung run that reports failing late is not counted again and alerts nothing.
  await handles[0]!.fail(new Error("late"));
  assert.deepEqual(alerts.types(), ["stuck", "failed"]);
  // The first success closes both with one recovery.
  await job.run(async () => {});
  assert.deepEqual(alerts.types(), ["stuck", "failed", "recovered"]);
  assert.deepEqual([...(alerts.alerts[2]!.details as { after: string[] }).after].sort(), ["failed", "stuck"]);
  assert.deepEqual((await cw.jobSummary("hangs"))!.open, []);
});

test("a stuck run that finishes ok late closes stuck with a recovery, though other runs started meanwhile", async () => {
  const { cw, c, alerts } = make();
  const job = cw.job("slow-queue", { timeout: "10m" });
  const first = await job.start();
  c.advance(11 * MIN);
  await cw.check();
  assert.deepEqual(alerts.types(), ["stuck"]);
  const second = await job.start();
  assert.deepEqual((await cw.jobSummary("slow-queue"))!.open, ["stuck"], "a start leaves stuck open");
  await first.finish();
  assert.deepEqual(alerts.types(), ["stuck", "recovered"]);
  await second.finish();
  assert.deepEqual(alerts.types(), ["stuck", "recovered"]);
});

test("a store that hangs while recording a run's start does not hold the job back, and leaves no running row", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const base = memory();
  let release!: () => void;
  const gate = new Promise<void>((resolve) => { release = resolve; });
  let inserts = 0;
  const store = new Proxy(base, {
    get(target, prop, receiver) {
      const value = Reflect.get(target, prop, receiver);
      if (prop !== "insertRun") return value;
      // The first insert (the run's start) hangs until released.
      return async (run: Parameters<typeof base.insertRun>[0]) => { if (++inserts === 1) await gate; return base.insertRun(run); };
    },
  });
  const errors: string[] = [];
  const { cw, alerts } = make({ store, onError: (e, where) => errors.push(`${where}: ${(e as Error).message}`) });
  const job = cw.job("nightly");
  let ran = false;
  const running = job.run(async (j) => { ran = true; j.log("did the work"); return "done"; });
  await settle();
  assert.equal(ran, false, "still waiting on the store");
  t.mock.timers.tick(4_999);
  await settle();
  assert.equal(ran, false);
  t.mock.timers.tick(1);
  await settle();
  assert.equal(ran, true, "the job ran once the deadline passed");
  assert.deepEqual(errors, ["recording nightly: the store took longer than 5s to record the start of nightly; running it unrecorded"]);
  // The job is done and its finish is waiting on the same store. The late
  // start lands first, and the finish is written over it.
  release();
  assert.equal(await running, "done");
  await settle();
  const runs = await cw.runs("nightly");
  assert.equal(runs.length, 1);
  assert.equal(runs[0]!.status, "ok");
  assert.equal(runs[0]!.output, "did the work");
  assert.deepEqual(await base.runningRuns(), []);
  assert.equal(errors.length, 1);
  assert.deepEqual(alerts.types(), []);
});

test("a start write that lands after the finish failed is finished then, not left running", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const base = memory();
  let release!: () => void;
  const gate = new Promise<void>((resolve) => { release = resolve; });
  let inserts = 0;
  const store = new Proxy(base, {
    get(target, prop, receiver) {
      const value = Reflect.get(target, prop, receiver);
      if (prop !== "insertRun") return value;
      return async (run: Parameters<typeof base.insertRun>[0]) => {
        inserts += 1;
        if (inserts === 1) { await gate; return base.insertRun(run); }
        // The finish finds the store down, not hanging.
        throw new Error("store down");
      };
    },
  });
  const errors: string[] = [];
  const { cw, alerts } = make({ store, onError: (e, where) => errors.push(`${where}: ${(e as Error).message}`) });
  const running = cw.job("nightly").run(async () => { throw new Error("boom"); });
  await settle();
  t.mock.timers.tick(5_000);
  await assert.rejects(running, /boom/, "the job's own error is what run() throws");
  assert.deepEqual(errors.map((e) => e.replace(/:.*/, "")), ["recording nightly", "recording nightly"]);
  assert.deepEqual(await cw.runs("nightly"), []);
  // The start write lands at last, after the finish gave up: the run is finished and judged now.
  release();
  await settle();
  const runs = await cw.runs("nightly");
  assert.equal(runs.length, 1);
  assert.equal(runs[0]!.status, "failed");
  assert.match(runs[0]!.error!, /boom/);
  assert.deepEqual(await base.runningRuns(), []);
  assert.deepEqual(alerts.types(), ["failed"]);
});

test("a start write that lands while the finish is failing is finished by that finish, not left running", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const base = memory();
  let releaseStart!: () => void;
  let failFinish!: () => void;
  const startGate = new Promise<void>((resolve) => { releaseStart = resolve; });
  const finishGate = new Promise<void>((resolve) => { failFinish = resolve; });
  let inserts = 0;
  let getRunDown = false;
  const store = new Proxy(base, {
    get(target, prop, receiver) {
      const value = Reflect.get(target, prop, receiver);
      if (prop === "insertRun") {
        return async (run: Parameters<typeof base.insertRun>[0]) => {
          inserts += 1;
          if (inserts === 1) { await startGate; return base.insertRun(run); }
          // The finish, written as a run of its own, hangs and then fails.
          await finishGate;
          throw new Error("store down");
        };
      }
      // The read after that failure (was the run recorded meanwhile?) fails too, once.
      if (prop === "getRun") return (id: string) => (getRunDown ? (getRunDown = false, Promise.reject(new Error("store down"))) : base.getRun(id));
      return value;
    },
  });
  const errors: string[] = [];
  const { cw, alerts } = make({ store, onError: (e, where) => errors.push(`${where}: ${(e as Error).message}`) });
  const running = cw.job("nightly").run(async () => { throw new Error("boom"); });
  await settle();
  t.mock.timers.tick(5_000);
  await settle();
  assert.equal(inserts, 2, "the job has run and its finish is being written");
  // The start lands now, while the finish is still under way.
  releaseStart();
  await settle();
  assert.equal((await base.getRun((await base.runningRuns())[0]!.id))!.status, "running");
  // Then the finish fails, and cannot see that the start has landed.
  getRunDown = true;
  failFinish();
  await assert.rejects(running, /boom/);
  await settle();
  const runs = await cw.runs("nightly");
  assert.equal(runs.length, 1);
  assert.equal(runs[0]!.status, "failed");
  assert.deepEqual(await base.runningRuns(), [], "no row left running by a live process");
  assert.deepEqual(alerts.types(), ["failed"]);
  assert.deepEqual(errors.map((e) => e.replace(/^recording nightly: /, "")), ["the store took longer than 5s to record the start of nightly; running it unrecorded", "store down"]);
});

test("a handle kept across a forget declares the job again on its next run, and writes it once", async () => {
  let upserts = 0;
  const base = memory();
  const store = new Proxy(base, {
    get(target, prop, receiver) {
      const value = Reflect.get(target, prop, receiver);
      if (prop !== "upsertJob") return value;
      return (...args: Parameters<typeof base.upsertJob>) => { upserts += 1; return base.upsertJob(...args); };
    },
  });
  const { cw } = make({ store });
  const job = cw.job("nightly", { schedule: "0 2 * * *", description: "Nightly export" });
  await job.run(async () => {});
  assert.equal(upserts, 1);
  await cw.forget("nightly");
  assert.deepEqual(cw.definedJobs(), []);

  await job.run(async () => {});
  assert.equal(upserts, 2);
  assert.deepEqual(cw.definedJobs().map((d) => d.name), ["nightly"], "declared again, so checks and sources see it");
  await job.run(async () => {});
  await job.run(async () => {});
  assert.equal(upserts, 2, "written once, not on every run");
  assert.equal((await cw.jobSummary("nightly"))!.definition.description, "Nightly export");

  // Forgotten by another process since: this one's check writes it again.
  await base.deleteJob("nightly");
  await cw.check();
  assert.ok(await base.getJob("nightly"));
});

test("a forget that lands while a check is writing declarations is not undone by that check", async () => {
  for (const read of ["check", "jobs", "jobSummary"] as const) {
    const store = memory();
    const { cw } = make({ store });
    cw.job("keep", { schedule: "0 2 * * *" });
    cw.job("nightly", { schedule: "0 2 * * *" });
    // Neither is written yet, so the read's own loop is what would write them.
    const reading = read === "check" ? cw.check() : read === "jobs" ? cw.jobs() : cw.jobSummary("nightly");
    await cw.forget("nightly");
    await reading;
    assert.deepEqual(cw.definedJobs().map((d) => d.name), ["keep"], `${read}: still forgotten here`);
    assert.equal(await store.getJob("nightly"), null, `${read}: not written again`);
    assert.equal(await store.getState("nightly"), null, `${read}: no state left behind`);
    // Its next run still brings it back.
    await cw.run("nightly", async () => {});
    assert.ok(await store.getJob("nightly"));
  }
});

test("an onError that throws does not stop a check, the jobs after it, or a dashboard read", async () => {
  const base = memory();
  const broken = new Set<string>();
  const store = new Proxy(base, {
    get(target, prop, receiver) {
      const value = Reflect.get(target, prop, receiver);
      if (prop === "listRuns") return (job: string, limit: number) => (broken.has(job) ? Promise.reject(new Error("store down")) : base.listRuns(job, limit));
      if (prop === "prune") return () => Promise.reject(new Error("prune down"));
      return value;
    },
  });
  let calls = 0;
  const { cw, c, alerts } = make({
    store,
    sources: [{ name: "bad", sync: async () => { throw new Error("source down"); } }],
    onError: () => { calls += 1; throw new Error("onError threw"); },
  });
  cw.job("a-broken", { schedule: "every 5m" });
  cw.job("b-late", { schedule: "every 5m" });
  await cw.jobs(); // written to the store now, so both are due an hour on
  broken.add("a-broken");
  c.advance(HOUR);
  const result = await cw.check();
  assert.ok(calls >= 3, "the source, the broken job, and the prune were each reported");
  assert.deepEqual(result.jobs.map((j) => j.name), ["a-broken", "b-late"]);
  assert.deepEqual(alerts.alerts.map((a) => `${a.job} ${a.type}`), ["b-late missed"], "the job after the broken one was still checked");
  // A dashboard read of the broken job answers, showing it as failing.
  assert.equal((await cw.jobSummary("a-broken"))!.health, "failing");
});

test("a failing alert channel does not break the run", async () => {
  const errors: string[] = [];
  const { cw } = make({ alerts: [{ name: "broken", send: async () => { throw new Error("no network"); } }], onError: (_e, where) => errors.push(where) });
  await assert.rejects(cw.run("x", async () => { throw new Error("job"); }), /job/);
  assert.deepEqual(errors, ["alert channel broken"]);
});
