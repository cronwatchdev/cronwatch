import assert from "node:assert/strict";
import net, { createServer, type Socket } from "node:net";
import { test } from "node:test";
import pg from "pg";
import { postgres } from "../src/stores/postgres.js";

/** What the store gives its own pool to wait for a connection. */
const CONNECT_TIMEOUT_MS = 5_000;

/**
 * Enough of a Postgres server for one idle connection: it accepts the
 * startup message, answers every query as an empty one, and can drop its
 * connections, as a database restarting or a proxy closing them does.
 */
async function fakePostgres(options: { stall?: boolean } = {}) {
  const sockets = new Set<Socket>();
  const message = (type: string, body: Buffer = Buffer.alloc(0)) => {
    const head = Buffer.alloc(5);
    head.write(type, 0, "latin1");
    head.writeInt32BE(body.length + 4, 1);
    return Buffer.concat([head, body]);
  };
  const ready = message("Z", Buffer.from("I"));
  const server = createServer((socket) => {
    sockets.add(socket);
    socket.on("close", () => sockets.delete(socket));
    socket.on("error", () => {});
    let started = false;
    socket.on("data", () => {
      // A server that takes the connection and never answers the startup.
      if (options.stall) return;
      if (!started) {
        started = true;
        // AuthenticationOk, then ReadyForQuery.
        socket.write(Buffer.concat([message("R", Buffer.alloc(4)), ready]));
        return;
      }
      // EmptyQueryResponse, then ReadyForQuery.
      socket.write(Buffer.concat([message("I"), ready]));
    });
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const { port } = server.address() as { port: number };
  return {
    url: `postgres://user@127.0.0.1:${port}/db`,
    drop: () => { for (const socket of sockets) socket.destroy(); },
    close: () => new Promise<void>((resolve) => { for (const socket of sockets) socket.destroy(); server.close(() => resolve()); }),
  };
}

const until = async (done: () => boolean) => {
  for (let i = 0; i < 200 && !done(); i++) await new Promise((r) => setTimeout(r, 10));
};

test("postgres: an idle connection lost by the store's own pool is reported, not thrown as an uncaught 'error' event", async () => {
  const server = await fakePostgres();
  const errors: unknown[] = [];
  const store = postgres({ connectionString: server.url, onError: (error) => errors.push(error) });
  try {
    assert.deepEqual(await store.listJobs(), []);
    // The connection is idle in the pool now. Without a listener, pg's
    // 'error' event here is an uncaught exception that ends the process.
    server.drop();
    await until(() => errors.length > 0);
    assert.equal(errors.length, 1);
    assert.ok(errors[0] instanceof Error);
    // The pool carries on with a new connection.
    assert.deepEqual(await store.listJobs(), []);
  } finally {
    await store.close!();
    await server.close();
  }
});

test("postgres: a lost idle connection is ignored by default, and an onError that throws does not escape", async () => {
  const server = await fakePostgres();
  let called = 0;
  const quiet = postgres({ connectionString: server.url });
  const loud = postgres({ connectionString: server.url, onError: () => { called += 1; throw new Error("onError threw"); } });
  try {
    await quiet.listJobs();
    await loud.listJobs();
    server.drop();
    await until(() => called > 0);
    assert.equal(called, 1);
    assert.deepEqual(await quiet.listJobs(), []);
    assert.deepEqual(await loud.listJobs(), []);
  } finally {
    await quiet.close!();
    await loud.close!();
    await server.close();
  }
});

/** Whether `promise` has settled yet, and how, once pending I/O has had its turn. */
async function outcome(promise: Promise<unknown>): Promise<"pending" | "resolved" | Error> {
  let settled: "pending" | "resolved" | Error = "pending";
  promise.then(() => { settled = "resolved"; }, (e: Error) => { settled = e; });
  for (let i = 0; i < 50; i++) await new Promise((r) => setImmediate(r));
  return settled;
}

test("postgres: the store's own pool gives up on a connection after five seconds, and keeps its connections alive", async (t) => {
  const server = await fakePostgres({ stall: true });
  const keepAlive = t.mock.method(net.Socket.prototype, "setKeepAlive");
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const store = postgres({ connectionString: server.url });
  try {
    // The server accepts the connection and never answers: pg alone would wait for ever.
    const reading = store.listJobs();
    assert.equal(await outcome(reading), "pending");
    t.mock.timers.tick(CONNECT_TIMEOUT_MS - 1);
    assert.equal(await outcome(reading), "pending");
    t.mock.timers.tick(1);
    const failed = await outcome(reading);
    assert.ok(failed instanceof Error, "the read failed, where it would have hung");
    assert.match(failed.message, /timeout/i);
    assert.ok(keepAlive.mock.calls.some((call) => call.arguments[0] === true), "TCP keepalive is on");
  } finally {
    t.mock.timers.reset();
    await store.close!();
    await server.close();
  }
});

test("postgres: poolOptions override the store's defaults and pass through, and a pool passed in is left alone", async (t) => {
  const server = await fakePostgres({ stall: true });
  const keepAlive = t.mock.method(net.Socket.prototype, "setKeepAlive");
  t.mock.timers.enable({ apis: ["setTimeout"] });
  // The caller's own values win, and anything else goes to the pool as given.
  const quick = postgres({ poolOptions: { connectionString: server.url, connectionTimeoutMillis: 250, keepAlive: false, max: 1 } });
  // A pool the caller made: no timeout is added to it, so it waits as pg does.
  const pool = new pg.Pool({ connectionString: server.url });
  pool.on("error", () => {});
  const own = postgres({ pool });
  try {
    const reading = quick.listJobs();
    const waiting = own.listJobs();
    t.mock.timers.tick(249);
    assert.equal(await outcome(reading), "pending");
    t.mock.timers.tick(1);
    assert.ok((await outcome(reading)) instanceof Error, "the caller's 250 ms, not the store's five seconds");
    t.mock.timers.tick(60_000);
    assert.equal(await outcome(waiting), "pending", "the caller's pool has no timeout unless the caller gives it one");
    assert.ok(!keepAlive.mock.calls.some((call) => call.arguments[0] === true), "keepAlive: false is kept, and the caller's pool is not given one");
    waiting.catch(() => {});
  } finally {
    t.mock.timers.reset();
    await quick.close!();
    await own.close!();
    await server.close();
    await pool.end().catch(() => {});
  }
});
