import assert from "node:assert/strict";
import { test } from "node:test";
import { cronwatch, memory } from "../src/index.js";
import type { Alert, AlertChannel, Store } from "../src/index.js";
import { heldRun, SEND_LEASE_MS } from "../src/evaluate.js";
import { capture, clock, MIN, settle, T0 } from "./helpers.js";

/**
 * A store for a process that is about to die: once `kill()` is called,
 * nothing it asks of the store ever completes, as when the process is gone.
 */
function mortal(store: Store) {
  let dead = false;
  const wrapped = new Proxy(store, {
    get(target, prop, receiver) {
      const value = Reflect.get(target, prop, receiver);
      if (typeof value !== "function") return value;
      return (...args: unknown[]) => (dead ? new Promise(() => {}) : value.apply(target, args));
    },
  });
  return { store: wrapped, kill: () => { dead = true; } };
}

/** A channel whose sends wait for `release()`, and which says when one has started. */
function held() {
  let release!: () => void;
  let entered!: () => void;
  const gate = new Promise<void>((r) => { release = r; });
  const sending = new Promise<void>((r) => { entered = r; });
  const sent: Alert[] = [];
  const channel: AlertChannel = { name: "held", send: async (alert) => { entered(); await gate; sent.push(alert); } };
  return { channel, sending, release, sent };
}

test("the write that opens a condition holds its alert, so a process that dies before sending it does not lose it", async () => {
  const c = clock();
  const shared = memory();
  const { store, kill } = mortal(shared);
  let triaging!: () => void;
  const triaged = new Promise<void>((r) => { triaging = r; });
  // The process dies while its triage call is out: no channel was ever called.
  const dying = cronwatch({ store, now: c.now, alerts: [capture()], cronSecret: null, triage: async () => { kill(); triaging(); return new Promise(() => {}); } });
  void dying.run("nightly", async () => { throw new Error("disk full"); }).catch(() => {});
  await triaged;
  const state = (await shared.getState("nightly"))!;
  assert.equal(state.open.failed, T0);
  assert.deepEqual(state.sending!.map((entry) => [entry.alert.type, entry.alert.at, entry.until]), [["failed", T0, T0 + SEND_LEASE_MS]]);
  assert.equal(state.sending![0]!.alert.triage, undefined, "triage is made at send time, never stored here");
  assert.deepEqual(state.undelivered, []);

  // Another process's checks leave it alone while its sender's lease runs.
  const sent = capture();
  const server = cronwatch({ store: shared, now: c.now, alerts: [sent], cronSecret: null, triage: async () => "The disk is full." });
  c.advance(MIN);
  await server.check();
  assert.deepEqual(sent.types(), []);

  // Once it has run out, the next check sends it, triaged, once.
  c.set(T0 + SEND_LEASE_MS + 1);
  const result = await server.check();
  assert.deepEqual(result.alerts.map((a) => a.type), ["failed"]);
  assert.deepEqual(sent.alerts.map((a) => [a.type, a.at, a.triage]), [["failed", T0, "The disk is full."]]);
  const after = (await shared.getState("nightly"))!;
  assert.equal(after.sending, undefined, "the key goes once nothing is being sent");
  assert.deepEqual(after.undelivered, []);
  await server.check();
  await assert.rejects(server.run("nightly", async () => { throw new Error("again"); }));
  assert.deepEqual(sent.types(), ["failed"], "the condition still alerts once");
});

test("an alert a channel took just before its process died is sent again after the lease: at least once", async () => {
  const c = clock();
  const shared = memory();
  const { store, kill } = mortal(shared);
  const first = capture();
  // Accepted, then the process is gone before it records that.
  const dying = cronwatch({ store, now: c.now, alerts: [{ name: "first", send: async (alert) => { await first.send(alert); kill(); } }], cronSecret: null });
  void dying.run("nightly", async () => { throw new Error("x"); }).catch(() => {});
  await settle();
  assert.deepEqual(first.types(), ["failed"]);
  const sent = capture();
  const server = cronwatch({ store: shared, now: c.now, alerts: [sent], cronSecret: null });
  c.set(T0 + SEND_LEASE_MS + 1);
  await server.check();
  assert.deepEqual(sent.types(), ["failed"], "sent a second time: the one duplicate a crash can cause");
});

test("while an alert is being sent, no check anywhere sends it too", async () => {
  const c = clock();
  const shared = memory();
  const { channel, sending, release, sent } = held();
  const worker = cronwatch({ store: shared, now: c.now, alerts: [channel], cronSecret: null });
  const other = capture();
  const server = cronwatch({ store: shared, now: c.now, alerts: [other], cronSecret: null });
  const run = worker.run("nightly", async () => { throw new Error("x"); }).catch(() => {});
  await sending;
  c.advance(MIN);
  await server.check();
  // The sending process's own check, too.
  const ownCheck = worker.check();
  await settle();
  release();
  await Promise.all([run, ownCheck]);
  assert.deepEqual(sent.map((a) => a.type), ["failed"]);
  assert.deepEqual(other.types(), []);
  const state = (await shared.getState("nightly"))!;
  assert.equal(state.sending, undefined);
  assert.deepEqual(state.undelivered, []);
  assert.equal(state.lastAlertAt, T0, "the time the run was judged, as before");
  c.set(T0 + SEND_LEASE_MS + MIN);
  await server.check();
  await worker.check();
  assert.deepEqual(other.types(), []);
  assert.deepEqual(sent.map((a) => a.type), ["failed"]);
});

test("an alert no channel took moves from the outbox to the retry queue, with its triage", async () => {
  const c = clock();
  const shared = memory();
  const down: AlertChannel = { name: "down", send: async () => { throw new Error("down"); } };
  const cw = cronwatch({ store: shared, now: c.now, alerts: [down], cronSecret: null, onError: () => {}, triage: async () => "Look at the disk." });
  await assert.rejects(cw.run("nightly", async () => { throw new Error("x"); }));
  const state = (await shared.getState("nightly"))!;
  assert.equal(state.sending, undefined);
  assert.deepEqual(state.undelivered!.map((a) => [a.type, a.triage]), [["failed", "Look at the disk."]]);
});

test("a process that queues its alerts for a check elsewhere writes them with the state that opens the condition", async () => {
  const c = clock();
  const shared = memory();
  let writes = 0;
  const counting: Store = { ...shared, compareAndSetState: async (state, version) => { writes++; return shared.compareAndSetState!(state, version); } };
  const recorder = cronwatch({ store: counting, now: c.now, deliver: "check", cronSecret: null });
  await assert.rejects(recorder.run("backup", async () => { throw new Error("disk full"); }));
  const state = (await shared.getState("backup"))!;
  assert.deepEqual(state.undelivered!.map((a) => a.type), ["failed"]);
  assert.equal(state.sending, undefined);
  assert.equal(state.pendingRuns, undefined);
  assert.equal(writes, 2, "the start holds the run; then one write: the failure, its alert, and the run's release together");
});

test("two processes checking at once retry a queued alert once: the one whose claim lands sends it", async () => {
  const c = clock();
  const shared = memory();
  let down = true;
  const sent: Alert[] = [];
  const channel: AlertChannel = { name: "flaky", send: async (alert) => { if (down) throw new Error("down"); sent.push(alert); } };
  const a = cronwatch({ store: shared, now: c.now, alerts: [channel], cronSecret: null, onError: () => {} });
  const b = cronwatch({ store: shared, now: c.now, alerts: [channel], cronSecret: null, onError: () => {} });
  await assert.rejects(a.run("nightly", async () => { throw new Error("disk full"); }));
  assert.deepEqual((await shared.getState("nightly"))!.undelivered!.map((x) => x.type), ["failed"]);
  down = false;
  c.advance(MIN);
  await Promise.all([a.check(), b.check()]);
  assert.deepEqual(sent.map((x) => x.type), ["failed"]);
  const state = (await shared.getState("nightly"))!;
  assert.deepEqual(state.undelivered, []);
  assert.equal(state.sending, undefined);
});

test("while a job is silenced a retry claims only recoveries; the rest wait in the queue", async () => {
  const c = clock();
  const shared = memory();
  let down = true;
  const sent: Alert[] = [];
  const channel: AlertChannel = { name: "flaky", send: async (alert) => { if (down) throw new Error("down"); sent.push(alert); } };
  const cw = cronwatch({ store: shared, now: c.now, alerts: [channel], cronSecret: null, onError: () => {} });
  await assert.rejects(cw.run("nightly", async () => { throw new Error("disk full"); }));
  await cw.silence("nightly", "1h");
  down = false;
  c.advance(MIN);
  await cw.check();
  assert.equal(sent.length, 0);
  assert.deepEqual((await shared.getState("nightly"))!.undelivered!.map((x) => x.type), ["failed"]);
  await cw.unsilence("nightly");
  await cw.check();
  assert.deepEqual(sent.map((x) => x.type), ["failed"]);
});

test("a finished run whose evaluation was never written is evaluated by a later check, once", async () => {
  const c = clock();
  const shared = memory();
  // The finish's state write keeps losing, as if the process stopped between the run's write and its evaluation.
  let refuse = false;
  const contested: Store = { ...shared, compareAndSetState: async (state, version) => (refuse ? false : shared.compareAndSetState!(state, version)) };
  const sent = capture();
  const cw = cronwatch({ store: contested, now: c.now, alerts: [sent], cronSecret: null, onError: () => {} });
  const handle = await cw.job("nightly").start();
  refuse = true;
  await handle.finish({ error: new Error("disk full") });
  refuse = false;
  assert.equal((await shared.getRun(handle.id))!.status, "failed");
  assert.deepEqual(sent.types(), []);
  assert.deepEqual((await shared.getState("nightly"))!.pendingRuns, [handle.id]);
  // Left alone at first, in case its finish is still being written elsewhere.
  c.advance(MIN);
  await cw.check();
  assert.deepEqual(sent.types(), []);
  c.advance(SEND_LEASE_MS);
  await cw.check();
  assert.deepEqual(sent.types(), ["failed"]);
  assert.equal((await shared.getState("nightly"))!.pendingRuns, undefined);
  await cw.check();
  assert.deepEqual(sent.types(), ["failed"]);
});

test("a pending run the store no longer has is let go, and a run stored unjudged on purpose is not evaluated", async () => {
  const c = clock();
  const shared = memory();
  const sent = capture();
  const cw = cronwatch({ store: shared, now: c.now, alerts: [sent], cronSecret: null });
  cw.job("nightly");
  await shared.setState({ job: "nightly", open: {}, consecutiveFailures: 0, silencedUntil: null, lastAlertAt: null, pendingRuns: ["gone"] });
  await cw.check();
  assert.equal((await shared.getState("nightly"))!.pendingRuns, undefined);
  const run = { id: "imported", job: "nightly", status: "running" as const, startedAt: c.now(), finishedAt: null, durationMs: null, error: null, output: null, metrics: {}, trigger: "x" };
  await cw.recordRun(run);
  assert.deepEqual((await shared.getState("nightly"))!.pendingRuns, ["imported"]);
  await cw.recordRun({ ...run, status: "failed", finishedAt: c.now(), durationMs: 0, error: "x" }, { evaluate: false });
  assert.equal((await shared.getState("nightly"))!.pendingRuns, undefined);
  c.advance(SEND_LEASE_MS + MIN);
  await cw.check();
  assert.deepEqual(sent.types(), []);
});

test("two clients recover one pending failure once, including after a CAS retry", async () => {
  const c = clock();
  const store = memory();
  const sent = capture();
  const options = { store, now: c.now, alerts: [sent], cronSecret: null };
  const a = cronwatch(options), b = cronwatch(options);
  await store.upsertJob({ name: "pending", failuresBeforeAlert: 2 }, c.now());
  await store.insertRun({ id: "lost", job: "pending", status: "failed", startedAt: c.now(), finishedAt: c.now(), durationMs: 0, error: "failed", output: null, metrics: {}, trigger: "start" });
  await store.setState({ job: "pending", open: {}, consecutiveFailures: 0, silencedUntil: null, lastAlertAt: null, pendingRuns: ["lost"] });
  c.advance(SEND_LEASE_MS + MIN);
  await Promise.all([a.check(), b.check()]);
  const state = (await store.getState("pending"))!;
  assert.equal(state.consecutiveFailures, 1);
  assert.equal(state.pendingRuns, undefined);
  assert.deepEqual(state.open, {});
  assert.deepEqual(sent.types(), []);
});

/** A channel that refuses while `down.value` is true, and records what it took. */
function switchable(name: string) {
  const down = { value: false };
  const sent: Alert[] = [];
  const channel: AlertChannel = { name, send: async (alert) => { if (down.value) throw new Error(`${name} down`); sent.push(alert); } };
  return { channel, down, sent };
}

test("a channel that refuses an alert is the only one it is retried to, until it takes it", async () => {
  const c = clock();
  const shared = memory();
  const slack = switchable("slack");
  const email = switchable("email");
  const errors: string[] = [];
  const cw = cronwatch({ store: shared, now: c.now, alerts: [slack.channel, email.channel], cronSecret: null, onError: (_e, where) => errors.push(where) });
  slack.down.value = true;
  await assert.rejects(cw.run("nightly", async () => { throw new Error("x"); }));
  assert.deepEqual(errors, ["alert channel slack"]);
  assert.equal(email.sent.length, 1);
  let state = (await shared.getState("nightly"))!;
  assert.deepEqual(state.undelivered!.map((a) => a.owed), [["slack"]]);
  assert.equal(state.lastAlertAt, T0, "email took it");
  // Still down: the retry goes to Slack alone, and email never sees it twice.
  c.advance(MIN);
  await cw.check();
  assert.equal(email.sent.length, 1);
  assert.deepEqual((await shared.getState("nightly"))!.undelivered!.map((a) => a.owed), [["slack"]]);
  // Back up: Slack takes it, and the entry is done.
  slack.down.value = false;
  c.advance(MIN);
  const checked = await cw.check();
  assert.deepEqual(checked.alerts.map((a) => a.type), ["failed"]);
  assert.equal(slack.sent.length, 1);
  assert.equal(email.sent.length, 1);
  assert.equal("owed" in slack.sent[0]!, false, "a channel never sees owed");
  state = (await shared.getState("nightly"))!;
  assert.deepEqual(state.undelivered, []);
  assert.equal(state.lastAlertAt, T0 + 2 * MIN);
});

test("owed names channels by name and position, and a channel no longer configured is dropped from it", async () => {
  const c = clock();
  const shared = memory();
  const first = switchable("webhook");
  const second = switchable("webhook");
  const email = switchable("email");
  second.down.value = true;
  email.down.value = true;
  const cw = cronwatch({ store: shared, now: c.now, alerts: [first.channel, second.channel, email.channel], cronSecret: null, onError: () => {} });
  await assert.rejects(cw.run("nightly", async () => { throw new Error("x"); }));
  assert.deepEqual((await shared.getState("nightly"))!.undelivered!.map((a) => a.owed), [["webhook#2", "email"]]);
  // A later process has only the first webhook and email: webhook#2 is gone.
  email.down.value = false;
  const later = cronwatch({ store: shared, now: c.now, alerts: [first.channel, email.channel], cronSecret: null, onError: () => {} });
  c.advance(MIN);
  await later.check();
  assert.equal(first.sent.length, 1, "the first webhook took it the first time, and is not sent it again");
  assert.equal(email.sent.length, 1);
  assert.deepEqual((await shared.getState("nightly"))!.undelivered, []);
});

test("an alert queued without owed, by an older release, is retried to every channel configured now", async () => {
  const c = clock();
  const shared = memory();
  const down: AlertChannel = { name: "down", send: async () => { throw new Error("down"); } };
  const old = cronwatch({ store: shared, now: c.now, alerts: [down], cronSecret: null, onError: () => {} });
  await assert.rejects(old.run("nightly", async () => { throw new Error("x"); }));
  const state = (await shared.getState("nightly"))!;
  const legacy = state.undelivered!.map(({ owed: _owed, ...alert }) => alert);
  await shared.setState({ ...state, undelivered: legacy as typeof state.undelivered });
  const a = switchable("a");
  const b = switchable("b");
  const now = cronwatch({ store: shared, now: c.now, alerts: [a.channel, b.channel], cronSecret: null, onError: () => {} });
  c.advance(MIN);
  await now.check();
  assert.equal(a.sent.length, 1);
  assert.equal(b.sent.length, 1);
  assert.deepEqual((await shared.getState("nightly"))!.undelivered, []);
});

test("the first send has the whole run; the copy held in state, and so a retry, has it trimmed", async () => {
  const c = clock();
  const shared = memory();
  const sent: Alert[] = [];
  let down = true;
  const spy: AlertChannel = { name: "spy", send: async (alert) => { sent.push(alert); if (down) throw new Error("spy down"); } };
  const cw = cronwatch({ store: shared, now: c.now, alerts: [spy], cronSecret: null, onError: () => {} });
  await assert.rejects(cw.run("big", async (job) => { job.log("o".repeat(10_000)); throw new Error("e".repeat(10_000)); }));
  assert.ok(sent[0]!.run!.output!.length >= 10_000, "first send: the whole output");
  assert.ok(sent[0]!.run!.error!.length >= 10_000, "first send: the whole error");
  const held = (await shared.getState("big"))!.undelivered![0]!;
  assert.deepEqual(held.run, heldRun(sent[0]!.run!));
  assert.match(held.run!.error!, /^Error: e{2041}\n\[\d+ more characters trimmed\]$/);
  assert.match(held.run!.output!, /^\[\d+ earlier characters trimmed\]\n/);
  down = false;
  c.advance(MIN);
  await cw.check();
  assert.deepEqual(sent[1]!.run, held.run, "the retry sends the held copy");
});

test("a recovery check that beats the finishing process evaluates a tracked run only once", async () => {
  const c = clock();
  const shared = memory();
  const checker = cronwatch({ store: shared, now: c.now, cronSecret: null });
  const store: Store = { ...shared, updateRunIf: async (run, statuses) => {
    const written = await shared.updateRunIf!(run, statuses);
    if (written) await checker.check();
    return written;
  } };
  const runner = cronwatch({ store, now: c.now, cronSecret: null });
  const job = runner.job("race", { failuresBeforeAlert: 2 });
  await assert.rejects(job.run(async () => { c.advance(SEND_LEASE_MS + MIN); throw new Error("failed"); }));
  assert.equal((await shared.getState("race"))!.consecutiveFailures, 1);
  assert.deepEqual((await shared.getState("race"))!.open, {});
});

test("a timeout that consumes the pending marker while finishing still recovers on late success", async () => {
  const c = clock();
  const shared = memory();
  const sent = capture();
  const checker = cronwatch({ store: shared, now: c.now, cronSecret: null, alerts: [sent] });
  let checked = false;
  const store: Store = { ...shared, updateRunIf: async (run, statuses) => {
    if (!checked && run.status === "ok") { checked = true; await checker.check(); }
    return shared.updateRunIf!(run, statuses);
  } };
  const runner = cronwatch({ store, now: c.now, cronSecret: null, alerts: [sent] });
  await runner.job("late", { timeout: "1m" }).run(() => { c.advance(2 * MIN); });
  assert.deepEqual(sent.types(), ["stuck", "recovered"]);
  assert.deepEqual((await shared.getState("late"))!.open, {});
});

test("a recovered pending run finished through recordRun is evaluated once", async () => {
  const c = clock();
  const shared = memory();
  const checker = cronwatch({ store: shared, now: c.now, cronSecret: null });
  const store: Store = { ...shared, updateRunIf: async (run, statuses) => {
    const written = await shared.updateRunIf!(run, statuses);
    if (written) await checker.check();
    return written;
  } };
  const runner = cronwatch({ store, now: c.now, cronSecret: null });
  const handle = await runner.job("external", { failuresBeforeAlert: 2 }).start();
  const run = (await shared.getRun(handle.id))!;
  c.advance(SEND_LEASE_MS + MIN);
  await runner.recordRun({ ...run, status: "failed", finishedAt: c.now() - SEND_LEASE_MS - 1, error: "failed" });
  assert.equal((await shared.getState("external"))!.consecutiveFailures, 1);
  assert.deepEqual((await shared.getState("external"))!.open, {});
});


test("an evaluated import retries after its pending marker write fails, without evaluating historical imports", async () => {
  const c = clock();
  const store = memory();
  const sent = capture();
  const cas = store.compareAndSetState!.bind(store);
  let fail = true;
  store.compareAndSetState = async (...args) => {
    if (fail) { fail = false; throw new Error("state unavailable"); }
    return cas(...args);
  };
  const cw = cronwatch({ store, now: c.now, alerts: [sent] });
  cw.job("imported");
  const run = { id: "failed-import", job: "imported", status: "failed" as const, startedAt: c.now(), finishedAt: c.now(), durationMs: 0, error: "failed", output: null, metrics: {}, trigger: "source" };
  await assert.rejects(cw.recordRun(run), /state unavailable/);
  assert.equal((await store.getRun(run.id))!.status, "running");
  await cw.recordRun(run);
  await cw.recordRun(run);
  c.advance(SEND_LEASE_MS + MIN);
  await cw.check();
  assert.deepEqual(sent.types(), ["failed"]);
  assert.equal((await store.getState(run.job))!.consecutiveFailures, 1);
  assert.equal((await store.getState(run.job))!.pendingRuns, undefined);

  cw.job("history");
  await cw.recordRun({ ...run, id: "history", job: "history" }, { evaluate: false });
  await cw.recordRun({ ...run, id: "history", job: "history" });
  c.advance(SEND_LEASE_MS + MIN);
  await cw.check();
  assert.deepEqual(sent.types(), ["failed"]);
});

test("concurrent finished imports install evaluation intent and evaluate once", async () => {
  const c = clock();
  const store = memory();
  const sent = capture();
  const clients = Array.from({ length: 4 }, () => cronwatch({ store, now: c.now, alerts: [sent], onError: () => {} }));
  for (const cw of clients) cw.job("imported");
  const run = { id: "concurrent-import", job: "imported", status: "failed" as const, startedAt: c.now(), finishedAt: c.now(), durationMs: 0, error: "failed", output: null, metrics: {}, trigger: "source" };
  await Promise.all(clients.map((cw) => cw.recordRun(run)));
  c.advance(SEND_LEASE_MS + MIN);
  await Promise.all(clients.map((cw) => cw.check()));
  assert.deepEqual(sent.types(), ["failed"]);
  assert.equal((await store.getState(run.job))!.consecutiveFailures, 1);
  assert.equal((await store.getState(run.job))!.pendingRuns, undefined);
});

test("a concurrent check does not time out a completed import awaiting its marker", async () => {
  const store = memory();
  const c = clock();
  const sent = capture();
  let release!: () => void;
  let staged!: () => void;
  const gate = new Promise<void>((resolve) => { release = resolve; });
  const ready = new Promise<void>((resolve) => { staged = resolve; });
  const insert = store.insertRun.bind(store);
  store.insertRun = async (run) => { await insert(run); staged(); await gate; };
  const importer = cronwatch({ store, now: c.now, alerts: [sent] });
  const checker = cronwatch({ store, now: c.now, alerts: [sent] });
  importer.job("old", { timeout: "1m" });
  const importing = importer.recordRun({ id: "old-success", job: "old", status: "ok", startedAt: c.now() - 60 * MIN, finishedAt: c.now() - 59 * MIN, durationMs: MIN, error: null, output: null, metrics: {}, trigger: "source" });
  await ready;
  try {
    await checker.check();
    assert.deepEqual(sent.types(), []);
  } finally {
    release();
    await importing;
  }
  assert.equal((await store.getRun("old-success"))!.status, "ok");
  assert.deepEqual(sent.types(), []);
});
