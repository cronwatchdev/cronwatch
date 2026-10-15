package dev.cronwatch;

import static dev.cronwatch.Support.MIN;
import static dev.cronwatch.Support.T0;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import dev.cronwatch.Support.Capture;
import dev.cronwatch.Support.Clock;
import dev.cronwatch.Support.Errors;
import dev.cronwatch.internal.evaluate.Evaluate;
import dev.cronwatch.json.JsObject;
import dev.cronwatch.json.Json;
import dev.cronwatch.store.MemoryStore;
import dev.cronwatch.store.Store;
import java.io.IOException;
import java.lang.reflect.InvocationTargetException;
import java.lang.reflect.Proxy;
import java.util.ArrayList;
import java.util.List;
import java.util.Objects;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.atomic.AtomicBoolean;
import java.util.concurrent.atomic.AtomicInteger;
import org.junit.jupiter.api.Test;

/**
 * The SDK's {@code outbox.test.ts}, ported: an alert is written with the state that opens its
 * condition, so a process that dies before sending it does not lose it, and a live sender's alert
 * is sent by no one else.
 */
class OutboxTest {
  /**
   * A store for a process about to die: once {@link #kill} is called, nothing it asks of the store
   * ever completes, as when the process is gone.
   */
  static final class Mortal {
    final Store store;
    private volatile boolean dead;

    Mortal(Store inner) {
      store =
          (Store)
              Proxy.newProxyInstance(
                  Store.class.getClassLoader(),
                  new Class<?>[] {Store.class},
                  (proxy, method, args) -> {
                    if (dead && method.getDeclaringClass() != Object.class) {
                      new CountDownLatch(1).await();
                    }
                    try {
                      return method.invoke(inner, args);
                    } catch (InvocationTargetException e) {
                      throw e.getCause();
                    }
                  });
    }

    void kill() {
      dead = true;
    }
  }

  /** The alerts' types and times and triage, for comparing. */
  private static List<List<Object>> seen(List<Alert> alerts) {
    List<List<Object>> out = new ArrayList<>();
    for (Alert a : alerts) {
      out.add(List.of(a.type().value(), a.at(), String.valueOf(a.triage())));
    }
    return out;
  }

  private static void fail() {
    throw new IllegalStateException("disk full");
  }

  // The cross-port check: a queued alert lost the fields a newer writer added (and an alert of a
  // type this release does not know had its details rewritten as a failure's) on the next state
  // write, and a retry sent it without them. The SDK carries queued alerts as plain objects.
  @Test
  void aFinishedImportRetriesAfterItsPendingMarkerWriteFails() throws Exception {
    MemoryStore shared = new MemoryStore();
    AtomicBoolean refuse = new AtomicBoolean(true);
    Store contested =
        (Store)
            Proxy.newProxyInstance(
                Store.class.getClassLoader(),
                new Class<?>[] {Store.class},
                (proxy, method, args) -> {
                  if (refuse.get() && method.getName().equals("compareAndSetState")) return false;
                  try {
                    return method.invoke(shared, args);
                  } catch (InvocationTargetException e) {
                    throw e.getCause();
                  }
                });
    Capture capture = new Capture();
    try (Cronwatch cw =
        Support.builder(new Clock(), capture, new Errors()).store(contested).build()) {
      cw.job("imported");
      Run run =
          Run.of(
              "external",
              "imported",
              RunStatus.FAILED,
              T0,
              T0,
              0L,
              "disk full",
              null,
              Metrics.empty(),
              "x");
      assertThrows(CronwatchException.class, () -> cw.recordRun(run));
      assertEquals(RunStatus.RUNNING, Objects.requireNonNull(shared.getRun(run.id())).status());
      refuse.set(false);
      cw.recordRun(run);
      cw.recordRun(run);
      assertEquals(List.of("failed"), capture.types());
      assertEquals(RunStatus.FAILED, Objects.requireNonNull(shared.getRun(run.id())).status());
      assertNull(shared.getState(run.job()).pendingRuns());
    }
  }

  @Test
  void queuedAlertsKeepWhatANewerWriterAddedThroughWritesAndRetries() throws Exception {
    AtomicBoolean down = new AtomicBoolean(true);
    List<String> bodies = new CopyOnWriteArrayList<>();
    Channel flaky =
        Channel.of(
            "flaky",
            (a, ctx) -> {
              bodies.add(a.toJson());
              if (down.get()) {
                throw new IOException("down");
              }
            });
    Support.Made m = Support.make(b -> b.alerts(List.of(flaky)));
    assertThrows(IllegalStateException.class, () -> m.cw().run("r", j -> fail()));
    Store store = m.cw().store();
    JobState written = store.getState("r");
    assertNotNull(written);
    JsObject state = Json.parseObject(written.toJson());
    JsObject failed =
        (JsObject) ((List<?>) Objects.requireNonNull(state.get("undelivered"))).get(0);
    failed.set("futureAlertField", "kept");
    ((JsObject) Objects.requireNonNull(failed.get("details"))).set("futureDetail", 1);
    JsObject future =
        failed
            .copy()
            .set("type", "future_condition")
            .set("details", new JsObject().set("x", 1).set("a_b", List.of(2)));
    ((JsObject) Objects.requireNonNull(state.get("open"))).set("future_condition", T0);
    JsObject held = future.copy().set("at", T0 - 1);
    state.set("undelivered", List.of(failed, future));
    state.set(
        "sending",
        List.of(
            new JsObject()
                .set("until", T0 + 3_600_000L)
                .set("alert", held)
                .set("futureEntryKey", true)));

    // Read and written back as they were.
    String json = state.toJson();
    String again = JobState.fromJson(json).toJson();
    for (String kept :
        List.of(
            "\"futureAlertField\":\"kept\"",
            "\"futureDetail\":1",
            "\"details\":{\"x\":1,\"a_b\":[2]}",
            "\"futureEntryKey\":true")) {
      assertTrue(again.contains(kept), kept + " in " + again);
    }

    // Retried with them, and still queued with them after the retry fails.
    store.setState(JobState.fromJson(json));
    m.clock().advance(MIN);
    m.cw().check();
    assertEquals(3, bodies.size(), bodies.toString());
    assertTrue(bodies.get(1).contains("\"futureAlertField\":\"kept\""), bodies.get(1));
    assertTrue(bodies.get(1).contains("\"futureDetail\":1"), bodies.get(1));
    assertTrue(bodies.get(2).contains("\"details\":{\"x\":1,\"a_b\":[2]}"), bodies.get(2));
    JobState after = store.getState("r");
    assertNotNull(after);
    String stored = after.toJson();
    for (String kept :
        List.of(
            "\"futureAlertField\":\"kept\"",
            "\"futureDetail\":1",
            "\"details\":{\"x\":1,\"a_b\":[2]}",
            "\"futureEntryKey\":true")) {
      assertTrue(stored.contains(kept), kept + " in " + stored);
    }
  }

  @Test
  void theWriteThatOpensAConditionHoldsItsAlertSoAProcessThatDiesBeforeSendingDoesNotLoseIt()
      throws Exception {
    Clock clock = new Clock();
    MemoryStore shared = new MemoryStore();
    Mortal mortal = new Mortal(shared);
    CountDownLatch triaging = new CountDownLatch(1);
    // The process dies while its triage call is out: no channel was ever called.
    Cronwatch.Builder b =
        Support.builder(clock, new Capture(), new Errors())
            .store(mortal.store)
            .triage(
                context -> {
                  mortal.kill();
                  triaging.countDown();
                  new CountDownLatch(1).await();
                  return null;
                });
    b.timings.triageMs = 3_600_000;
    Cronwatch dying = b.build();
    Support.background(() -> dying.run("nightly", j -> fail()));
    triaging.await();
    JobState state = shared.getState("nightly");
    assertNotNull(state);
    assertEquals(T0, state.openAt(Condition.FAILED));
    List<SendingAlert> sending = state.sending();
    assertNotNull(sending);
    assertEquals(1, sending.size());
    Alert held = sending.get(0).alert();
    assertNotNull(held);
    assertEquals(
        List.of("failed", T0, T0 + Evaluate.SEND_LEASE_MS),
        List.of(held.type().value(), held.at(), sending.get(0).until()));
    assertNull(held.triage(), "triage is made at send time, never stored here");
    assertEquals(false, held.toValue().has("triage"));
    assertEquals(List.of(), state.undelivered());

    // Another process's checks leave it alone while its sender's lease runs.
    Capture sent = new Capture();
    try (Cronwatch server =
        Support.builder(clock, sent, new Errors())
            .store(shared)
            .triage(context -> "The disk is full.")
            .build()) {
      clock.advance(MIN);
      server.check();
      assertEquals(List.of(), sent.types());

      // Once it has run out, the next check sends it, triaged, once.
      clock.set(T0 + Evaluate.SEND_LEASE_MS + 1);
      CheckResult result = server.check();
      assertEquals(List.of("failed"), seen(result.alerts()).stream().map(a -> a.get(0)).toList());
      assertEquals(List.of(List.of("failed", T0, "The disk is full.")), seen(sent.alerts));
      JobState after = shared.getState("nightly");
      assertNotNull(after);
      assertNull(after.sending(), "the key goes once nothing is being sent");
      assertEquals(false, after.toValue().has("sending"));
      assertEquals(List.of(), after.undelivered());
      server.check();
      assertThrows(IllegalStateException.class, () -> server.run("nightly", j -> fail()));
      assertEquals(List.of("failed"), sent.types(), "the condition still alerts once");
    }
  }

  @Test
  void anAlertAChannelTookJustBeforeItsProcessDiedIsSentAgainAfterTheLease() throws Exception {
    Clock clock = new Clock();
    MemoryStore shared = new MemoryStore();
    Mortal mortal = new Mortal(shared);
    Capture first = new Capture();
    CountDownLatch taken = new CountDownLatch(1);
    // Accepted, then the process is gone before it records that.
    Channel accepting =
        new Channel() {
          @Override
          public String name() {
            return "first";
          }

          @Override
          public void send(Alert alert, ChannelContext context) {
            first.send(alert, context);
            mortal.kill();
            taken.countDown();
          }
        };
    Cronwatch dying =
        Support.builder(clock, new Capture(), new Errors())
            .alerts(List.of(accepting))
            .store(mortal.store)
            .build();
    Support.background(() -> dying.run("nightly", j -> fail()));
    taken.await();
    assertEquals(List.of("failed"), first.types());
    Capture sent = new Capture();
    try (Cronwatch server = Support.builder(clock, sent, new Errors()).store(shared).build()) {
      clock.set(T0 + Evaluate.SEND_LEASE_MS + 1);
      server.check();
      assertEquals(
          List.of("failed"),
          sent.types(),
          "sent a second time: the one duplicate a crash can cause");
    }
  }

  @Test
  void whileAnAlertIsBeingSentNoCheckAnywhereSendsItToo() throws Exception {
    Clock clock = new Clock();
    MemoryStore shared = new MemoryStore();
    CountDownLatch gate = new CountDownLatch(1);
    CountDownLatch sending = new CountDownLatch(1);
    List<Alert> sent = new CopyOnWriteArrayList<>();
    Channel held =
        new Channel() {
          @Override
          public String name() {
            return "held";
          }

          @Override
          public void send(Alert alert, ChannelContext context) throws InterruptedException {
            sending.countDown();
            gate.await();
            sent.add(alert);
          }
        };
    Capture other = new Capture();
    try (Cronwatch worker =
            Support.builder(clock, new Capture(), new Errors())
                .alerts(List.of(held))
                .store(shared)
                .build();
        Cronwatch server = Support.builder(clock, other, new Errors()).store(shared).build()) {
      Thread run = Support.background(() -> worker.run("nightly", j -> fail()));
      sending.await();
      clock.advance(MIN);
      server.check();
      // The sending process's own check, too.
      worker.check();
      gate.countDown();
      run.join();
      assertEquals(List.of("failed"), sent.stream().map(a -> a.type().value()).toList());
      assertEquals(List.of(), other.types());
      JobState state = shared.getState("nightly");
      assertNotNull(state);
      assertNull(state.sending());
      assertEquals(List.of(), state.undelivered());
      assertEquals(T0, state.lastAlertAt(), "the time the run was judged, as before");
      clock.set(T0 + Evaluate.SEND_LEASE_MS + MIN);
      server.check();
      worker.check();
      assertEquals(List.of(), other.types());
      assertEquals(List.of("failed"), sent.stream().map(a -> a.type().value()).toList());
    }
  }

  @Test
  void anAlertNoChannelTookMovesFromTheOutboxToTheRetryQueueWithItsTriage() throws Exception {
    Clock clock = new Clock();
    MemoryStore shared = new MemoryStore();
    Channel down =
        new Channel() {
          @Override
          public String name() {
            return "down";
          }

          @Override
          public void send(Alert alert, ChannelContext context) {
            throw new IllegalStateException("down");
          }
        };
    try (Cronwatch cw =
        Support.builder(clock, new Capture(), new Errors())
            .alerts(List.of(down))
            .store(shared)
            .triage(context -> "Look at the disk.")
            .build()) {
      assertThrows(IllegalStateException.class, () -> cw.run("nightly", j -> fail()));
      JobState state = shared.getState("nightly");
      assertNotNull(state);
      assertNull(state.sending());
      assertEquals(List.of(List.of("failed", T0, "Look at the disk.")), seen(state.undelivered()));
    }
  }

  @Test
  void aProcessThatQueuesForACheckElsewhereWritesItsAlertsWithTheStateThatOpensTheCondition()
      throws Exception {
    Clock clock = new Clock();
    MemoryStore shared = new MemoryStore();
    AtomicInteger writes = new AtomicInteger();
    Store counting =
        (Store)
            Proxy.newProxyInstance(
                Store.class.getClassLoader(),
                new Class<?>[] {Store.class},
                (proxy, method, args) -> {
                  if (method.getName().equals("compareAndSetState")) {
                    writes.incrementAndGet();
                  }
                  try {
                    return method.invoke(shared, args);
                  } catch (InvocationTargetException e) {
                    throw e.getCause();
                  }
                });
    try (Cronwatch recorder =
        Support.builder(clock, new Capture(), new Errors())
            .store(counting)
            .deliver(Deliver.AT_CHECK)
            .build()) {
      assertThrows(IllegalStateException.class, () -> recorder.run("backup", j -> fail()));
      JobState state = shared.getState("backup");
      assertNotNull(state);
      assertEquals(
          List.of("failed"), state.undelivered().stream().map(a -> a.type().value()).toList());
      assertNull(state.sending());
      // The start's write holds the run as pending; then one write has the failure and its alert.
      assertEquals(2, writes.get(), "the start's, then the failure and its alert together");
      assertNull(state.pendingRuns(), "the run is no longer pending once judged");
    }
  }

  /** A channel that refuses every alert while {@code down}, and keeps the ones it accepts. */
  private static Channel flaky(AtomicBoolean down, List<Alert> sent) {
    return new Channel() {
      @Override
      public String name() {
        return "flaky";
      }

      @Override
      public void send(Alert alert, ChannelContext context) {
        if (down.get()) {
          throw new IllegalStateException("down");
        }
        sent.add(alert);
      }
    };
  }

  private static Cronwatch client(Store store, Clock clock, Channel channel) {
    return Cronwatch.builder()
        .store(store)
        .clock(clock::now)
        .alerts(List.of(channel))
        .noCronSecret()
        .onError((where, e) -> {})
        .noShutdownHook()
        .build();
  }

  private static List<String> types(List<Alert> alerts) {
    return alerts.stream().map(a -> a.type().value()).toList();
  }

  @Test
  void twoProcessesCheckingAtOnceRetryAQueuedAlertOnceTheOneWhoseClaimLandsSendsIt()
      throws Exception {
    Clock clock = new Clock();
    MemoryStore shared = new MemoryStore();
    AtomicBoolean down = new AtomicBoolean(true);
    List<Alert> sent = new CopyOnWriteArrayList<>();
    try (Cronwatch a = client(shared, clock, flaky(down, sent));
        Cronwatch b = client(shared, clock, flaky(down, sent))) {
      assertThrows(IllegalStateException.class, () -> a.run("nightly", j -> fail()));
      assertEquals(List.of("failed"), types(shared.getState("nightly").undelivered()));
      down.set(false);
      clock.advance(MIN);
      Thread one = Support.background(a::check);
      Thread two = Support.background(b::check);
      one.join();
      two.join();
      assertEquals(List.of("failed"), types(sent));
      JobState state = shared.getState("nightly");
      assertEquals(List.of(), state.undelivered());
      assertNull(state.sending());
    }
  }

  @Test
  void whileAJobIsSilencedARetryClaimsOnlyRecoveriesTheRestWaitInTheQueue() throws Exception {
    Clock clock = new Clock();
    MemoryStore shared = new MemoryStore();
    AtomicBoolean down = new AtomicBoolean(true);
    List<Alert> sent = new CopyOnWriteArrayList<>();
    try (Cronwatch cw = client(shared, clock, flaky(down, sent))) {
      assertThrows(IllegalStateException.class, () -> cw.run("nightly", j -> fail()));
      cw.silence("nightly", "1h");
      down.set(false);
      clock.advance(MIN);
      cw.check();
      assertEquals(List.of(), sent);
      assertEquals(List.of("failed"), types(shared.getState("nightly").undelivered()));
      cw.unsilence("nightly");
      cw.check();
      assertEquals(List.of("failed"), types(sent));
    }
  }

  @Test
  void aFinishedRunWhoseEvaluationWasNeverWrittenIsEvaluatedByALaterCheckOnce() throws Exception {
    Clock clock = new Clock();
    MemoryStore shared = new MemoryStore();
    // The finish's state write keeps losing, as if the process stopped between the run's write and
    // its evaluation.
    AtomicBoolean refuse = new AtomicBoolean();
    Store contested =
        (Store)
            Proxy.newProxyInstance(
                Store.class.getClassLoader(),
                new Class<?>[] {Store.class},
                (proxy, method, args) -> {
                  if (refuse.get() && method.getName().equals("compareAndSetState")) {
                    return false;
                  }
                  try {
                    return method.invoke(shared, args);
                  } catch (InvocationTargetException e) {
                    throw e.getCause();
                  }
                });
    Capture capture = new Capture();
    try (Cronwatch cw = Support.builder(clock, capture, new Errors()).store(contested).build()) {
      RunHandle handle = cw.job("nightly").start();
      refuse.set(true);
      handle.fail(new IllegalStateException("disk full"));
      refuse.set(false);
      assertEquals(RunStatus.FAILED, Objects.requireNonNull(shared.getRun(handle.id())).status());
      assertEquals(List.of(), capture.types());
      assertEquals(List.of(handle.id()), shared.getState("nightly").pendingRuns());
      // Left alone at first, in case its finish is still being written elsewhere.
      clock.advance(MIN);
      cw.check();
      assertEquals(List.of(), capture.types());
      clock.advance(Evaluate.SEND_LEASE_MS);
      cw.check();
      assertEquals(List.of("failed"), capture.types());
      assertNull(shared.getState("nightly").pendingRuns());
      cw.check();
      assertEquals(List.of("failed"), capture.types());
    }
  }

  @Test
  void aPendingRunTheStoreNoLongerHasIsLetGoAndARunStoredUnjudgedOnPurposeIsNotEvaluated()
      throws Exception {
    Clock clock = new Clock();
    MemoryStore shared = new MemoryStore();
    Capture capture = new Capture();
    try (Cronwatch cw = Support.builder(clock, capture, new Errors()).store(shared).build()) {
      cw.job("nightly");
      shared.setState(
          JobState.fromJson(
              "{\"job\":\"nightly\",\"open\":{},\"consecutiveFailures\":0,"
                  + "\"silencedUntil\":null,\"lastAlertAt\":null,\"pendingRuns\":[\"gone\"]}"));
      cw.check();
      assertNull(shared.getState("nightly").pendingRuns());
      Run run = Run.running("imported", "nightly", clock.now(), "x");
      cw.recordRun(run);
      assertEquals(List.of("imported"), shared.getState("nightly").pendingRuns());
      cw.recordRun(
          Run.of(
              "imported",
              "nightly",
              RunStatus.FAILED,
              run.startedAt(),
              clock.now(),
              0L,
              "x",
              null,
              Metrics.empty(),
              "x"),
          false);
      assertNull(shared.getState("nightly").pendingRuns());
      clock.advance(Evaluate.SEND_LEASE_MS + MIN);
      cw.check();
      assertEquals(List.of(), capture.types());
    }
  }
}
