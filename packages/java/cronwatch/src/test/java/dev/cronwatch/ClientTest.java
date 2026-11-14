package dev.cronwatch;

import static dev.cronwatch.Support.HOUR;
import static dev.cronwatch.Support.MIN;
import static dev.cronwatch.Support.T0;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertSame;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import dev.cronwatch.Support.Made;
import dev.cronwatch.internal.js.Js;
import java.util.List;
import java.util.concurrent.CountDownLatch;
import org.junit.jupiter.api.Test;

/**
 * The SDK's {@code client.test.ts}, ported: runs, failures, expect, the checks for missed and stuck
 * runs, baselines and budgets, silence, triage, forget, and a failing channel. The handler cases
 * are in {@code web.HandlerTest}.
 */
class ClientTest {
  @Test
  void runRecordsOutputMetricsAndDurationAndReturnsTheResult() {
    Made m = Support.make();
    Job job = m.cw().job("report", JobOptions.builder().schedule("0 2 * * *"));
    String result =
        job.call(
            j -> {
              j.log("hello {\"n\":1}");
              j.metric("rows", 42);
              m.clock().advance(1500);
              return "done";
            });
    assertEquals("done", result);
    Run run = m.cw().runs("report", 50).get(0);
    assertEquals(RunStatus.OK, run.status());
    assertEquals(1500L, run.durationMs());
    assertEquals("hello {\"n\":1}", run.output());
    assertEquals("{\"rows\":42}", run.metrics().toJson());
    JobSummary summary = m.cw().jobSummary("report");
    assertEquals(JobHealth.HEALTHY, summary.health());
    assertEquals(Js.dateUtc(2026, 0, 6, 2, 0, 0, 0), summary.nextExpectedAt());
  }

  @Test
  void aThrowingJobIsRecordedAsFailedAlertsAndRethrows() {
    Made m = Support.make();
    Job job = m.cw().job("nightly");
    IllegalStateException e =
        assertThrows(
            IllegalStateException.class,
            () ->
                job.run(
                    j -> {
                      throw new IllegalStateException("db down");
                    }));
    assertEquals("db down", e.getMessage());
    Run run = m.cw().runs("nightly", 50).get(0);
    assertEquals(RunStatus.FAILED, run.status());
    assertTrue(run.error().startsWith("IllegalStateException: db down"), run.error());
    assertEquals(List.of("failed"), m.alerts().types());
    assertTrue(m.alerts().alerts.get(0).message().contains("db down"));
    assertEquals(JobHealth.FAILING, m.cw().jobSummary("nightly").health());
  }

  @Test
  void expectTurnsAQuietSuccessIntoAFailure() {
    Made m = Support.make();
    Job job = m.cw().job("export", JobOptions.builder().expect("wrote"));
    job.run(j -> j.log("wrote 12 files"));
    assertEquals(List.of(), m.alerts().types());
    m.clock().advance(HOUR);
    job.run(j -> j.log("nothing to do"));
    Run run = m.cw().runs("export", 50).get(0);
    assertEquals(RunStatus.FAILED, run.status());
    assertTrue(run.error().contains("did not contain \"wrote\""), run.error());
    assertEquals(List.of("failed"), m.alerts().types());
    // A returned string counts as output too.
    job.call(j -> "wrote 3 files");
    assertEquals(List.of("failed", "recovered"), m.alerts().types());
  }

  @Test
  void runDefinesOnFirstUseAndValidatesNamesAndSchedules() {
    Made m = Support.make();
    int one = m.cw().call("adhoc", JobOptions.builder().schedule("every 5m"), j -> 1);
    assertEquals(1, one);
    assertEquals(1, m.cw().jobs().size());
    assertTrue(
        assertThrows(CronwatchException.class, () -> m.cw().job("bad name!"))
            .getMessage()
            .contains("job name"));
    CronwatchException cron =
        assertThrows(
            CronwatchException.class, () -> m.cw().job("x", JobOptions.builder().schedule("nope")));
    assertTrue(cron.getMessage().contains("not a cron expression"), cron.getMessage());
    assertEquals(CronwatchException.Kind.INVALID, cron.kind());
    assertTrue(
        assertThrows(
                CronwatchException.class, () -> m.cw().job("x", JobOptions.builder().grace("soon")))
            .getMessage()
            .contains("grace"));
  }

  @Test
  void anHttpAnswerOf400OrMoreFailsTheRun() throws Exception {
    Made m = Support.make();
    Job job = m.cw().job("h");
    Object bad = Support.httpResponse(503);
    assertSame(bad, job.call(j -> bad), "the answer is passed through");
    assertEquals("HTTP 503 Service Unavailable", m.cw().runs("h", 1).get(0).error());
    assertEquals(List.of("failed"), m.alerts().types());
    Object fine = Support.httpResponse(204);
    job.call(j -> fine);
    assertEquals(RunStatus.OK, m.cw().runs("h", 1).get(0).status());
  }

  @Test
  void checkFindsAMissedRunOnceAndALaterRunRecovers() {
    Made m = Support.make();
    Job job = m.cw().job("sync", JobOptions.builder().schedule("every 1h").grace("10m"));
    m.cw().check(); // registers at T0
    m.clock().advance(30 * MIN);
    assertEquals(List.of(), m.cw().check().alerts());
    m.clock().set(T0 + 70 * MIN + 1);
    CheckResult r = m.cw().check();
    assertEquals(T0 + 70 * MIN + 1, r.checkedAt(), "a check of its own, not the last one's answer");
    assertEquals(List.of(AlertType.MISSED), r.alerts().stream().map(Alert::type).toList());
    assertEquals(JobHealth.LATE, r.jobs().get(0).health());
    assertEquals(List.of(), m.cw().check().alerts(), "no repeat");
    job.run(j -> {});
    assertEquals(List.of("missed", "recovered"), m.alerts().types());
    assertEquals(JobHealth.HEALTHY, m.cw().jobSummary("sync").health());
  }

  @Test
  void aJobDeclaredAgainWithoutItsScheduleClosesMissedWithARecoveryOnce() {
    Made m = Support.make();
    m.cw().job("sync", JobOptions.builder().schedule("every 1h").grace("10m"));
    m.cw().check();
    m.clock().set(T0 + 70 * MIN + 1);
    assertEquals(1, m.cw().check().alerts().size());
    Job job = m.cw().job("sync");
    m.clock().advance(MIN);
    CheckResult r = m.cw().check();
    assertEquals(1, r.alerts().size());
    Alert alert = r.alerts().get(0);
    assertEquals(AlertType.RECOVERED, alert.type());
    assertEquals("sync is no longer scheduled", alert.title());
    assertEquals(
        "Missed since 2026-01-05 10:40:00 UTC (1m ago). It has no schedule now, so nothing is"
            + " due; the missed alert is closed.",
        alert.message());
    assertEquals(
        "{\"after\":[\"missed\"],\"reason\":\"unscheduled\",\"since\":" + (T0 + 70 * MIN + 1) + "}",
        alert.details().toValue().toJson());
    assertEquals(JobHealth.NEVER_RAN, r.jobs().get(0).health());
    assertEquals(List.of(), m.cw().check().alerts(), "no repeat");
    job.run(j -> {});
    assertEquals(List.of("missed", "recovered"), m.alerts().types(), "the next run owes nothing");
  }

  @Test
  void aScheduleRemovedWhileSilencedClosesMissedWithItsRecoveryWhichTheSilenceDoesNotHoldBack() {
    Made m = Support.make();
    m.cw().job("sync", JobOptions.builder().schedule("every 1h").grace("10m"));
    m.cw().check();
    m.clock().set(T0 + 70 * MIN + 1);
    m.cw().check();
    m.cw().silence("sync", "1h");
    m.cw().job("sync");
    m.clock().advance(MIN);
    assertEquals(
        List.of(AlertType.RECOVERED), m.cw().check().alerts().stream().map(Alert::type).toList());
    assertEquals(List.of(), m.cw().jobSummary("sync").open());
    m.clock().advance(2 * HOUR);
    assertEquals(List.of(), m.cw().check().alerts());
    assertEquals(List.of("missed", "recovered"), m.alerts().types());
  }

  @Test
  void aRecoveryForAnAlertSentBeforeTheSilenceIsSentDuringItNewProblemsAreNot() throws Exception {
    Made m = Support.make();
    Job job = m.cw().job("flaky");
    assertThrows(
        IllegalStateException.class,
        () ->
            job.run(
                j -> {
                  throw new IllegalStateException("x");
                }));
    m.cw().silence("flaky", "1h");
    job.run(j -> {});
    assertEquals(List.of("failed", "recovered"), m.alerts().types());
    assertThrows(
        IllegalStateException.class,
        () ->
            job.run(
                j -> {
                  throw new IllegalStateException("y");
                }));
    assertEquals(List.of("failed", "recovered"), m.alerts().types());
  }

  @Test
  void afterAForgetRunAndResumeRunByNameDeclareTheJobAgainAsItWasWrittenNotBare() throws Exception {
    Made m = Support.make();
    JobOptions written = JobOptions.builder().schedule("0 2 * * *").description("Nightly export");
    m.cw().job("nightly", written);
    m.cw().run("nightly", j -> {});
    RunHandle handle = m.cw().job("nightly", written.copy()).start();
    m.cw().forget("nightly");
    assertNull(m.cw().jobSummary("nightly"));
    m.cw().run("nightly", j -> {});
    JobSummary summary = m.cw().jobSummary("nightly");
    assertEquals("0 2 * * *", summary.definition().schedule());
    assertEquals("Nightly export", summary.definition().description());
    m.cw().forget("nightly");
    RunHandle resumed = m.cw().resumeRun("nightly", handle.id());
    assertEquals(handle.id(), resumed.id());
    m.clock().advance(HOUR);
    m.cw().check();
    assertEquals("0 2 * * *", m.cw().jobSummary("nightly").definition().schedule());
    assertTrue(!m.alerts().types().contains("recovered"), "no 'no longer scheduled' recovery");
  }

  @Test
  void checkMarksARunThatNeverFinishedAsStuck() throws Exception {
    Made m = Support.make();
    Job job = m.cw().job("long", JobOptions.builder().timeout("5m"));
    CountDownLatch release = new CountDownLatch(1);
    Thread t = Support.background(() -> job.run(j -> release.await()));
    Support.await("the run to start", () -> m.cw().runs("long", 1).size() == 1);
    assertEquals(RunStatus.RUNNING, m.cw().runs("long", 1).get(0).status());
    m.clock().advance(4 * MIN);
    assertEquals(List.of(), m.cw().check().alerts());
    m.clock().advance(2 * MIN);
    CheckResult r = m.cw().check();
    assertEquals(List.of(AlertType.STUCK), r.alerts().stream().map(Alert::type).toList());
    assertEquals(RunStatus.TIMEOUT, m.cw().runs("long", 1).get(0).status());
    assertEquals(JobHealth.STUCK, r.jobs().get(0).health());
    assertTrue(m.alerts().alerts.get(0).message().contains("never reported finishing"));
    release.countDown();
    t.join();
  }

  @Test
  void slowAndOverBudgetAlertsComeFromTheJobsOwnBaseline() {
    Made m = Support.make();
    Job job = m.cw().job("agent", JobOptions.builder().budget("cost", 1));
    for (int i = 0; i < 5; i++) {
      job.run(
          j -> {
            m.clock().advance(1000);
            j.metric("tokens", 1000);
            j.metric("cost", 0.5);
          });
      m.clock().advance(HOUR);
    }
    assertEquals(List.of(), m.alerts().types());
    job.run(
        j -> {
          m.clock().advance(15_000);
          j.metric("tokens", 1000);
          j.metric("cost", 0.5);
        });
    assertEquals(List.of("slow"), m.alerts().types());
    m.clock().advance(HOUR);
    job.run(
        j -> {
          m.clock().advance(1000);
          j.metric("tokens", 5000);
          j.metric("cost", 1.2);
        });
    assertEquals(List.of("slow", "over_budget"), m.alerts().types());
    String message = m.alerts().alerts.get(1).message();
    assertTrue(message.contains("cost: 1.2, limit 1 (budget)"), message);
    assertTrue(
        message.contains("tokens: 5,000, limit 3,000 (three times the usual 1,000)"), message);
    m.clock().advance(HOUR);
    job.run(
        j -> {
          m.clock().advance(1000);
          j.metric("tokens", 1000);
          j.metric("cost", 0.5);
        });
    assertEquals(List.of("slow", "over_budget", "recovered"), m.alerts().types());
  }

  @Test
  void anUnderFloorAlertNamesTheMetricAndWhatItWasJudgedAgainst() {
    Made m = Support.make();
    Job job = m.cw().job("import", JobOptions.builder().floor("files", 1));
    for (int i = 0; i < 5; i++) {
      int rows = 4812 + i;
      job.run(
          j -> {
            m.clock().advance(1000);
            j.metric("rows", rows);
            j.metric("files", 2);
          });
      m.clock().advance(HOUR);
    }
    job.run(
        j -> {
          m.clock().advance(1000);
          j.metric("rows", 0);
          j.metric("files", 0);
        });
    assertEquals(List.of("under_floor"), m.alerts().types());
    Alert alert = m.alerts().alerts.get(0);
    assertEquals("import fell short", alert.title());
    assertTrue(
        alert
            .message()
            .contains("rows: 0 (the last 5 runs all reported more than 0, the lowest 4,812)"),
        alert.message());
    assertTrue(alert.message().contains("files: 0, below the floor of 1."), alert.message());
    m.clock().advance(HOUR);
    job.run(
        j -> {
          m.clock().advance(1000);
          j.metric("rows", 0);
          j.metric("files", 0);
        });
    assertEquals(List.of("under_floor"), m.alerts().types());
    m.clock().advance(HOUR);
    job.run(
        j -> {
          m.clock().advance(1000);
          j.metric("rows", 10);
          j.metric("files", 1);
        });
    assertEquals(List.of("under_floor", "recovered"), m.alerts().types());
  }

  @Test
  void aFloorMustBeAFiniteNumberAndNoHigherThanItsCeiling() {
    Made m = Support.make();
    assertTrue(
        assertThrows(
                CronwatchException.class,
                () -> m.cw().job("a", JobOptions.builder().floor("rows", Double.NaN)))
            .getMessage()
            .contains("floor.rows must be a finite number"));
    assertTrue(
        assertThrows(
                CronwatchException.class,
                () -> m.cw().job("b", JobOptions.builder().floor("cost", 3).budget("cost", 2)))
            .getMessage()
            .contains("floor.cost (3) is above budget.cost (2), so every run would alert"));
    assertTrue(
        assertThrows(
                CronwatchException.class,
                () -> m.cw().job("c", JobOptions.builder().field("floor", "rows")))
            .getMessage()
            .contains("floor must be an object of { metric: floor }"));
    m.cw().job("d", JobOptions.builder().floor("delta", -5).budget("delta", 5));
  }

  @Test
  void silenceSwallowsAlertsAndNothingOpensUnderneath() {
    Made m = Support.make();
    Job job = m.cw().job("flaky");
    m.cw().silence("flaky", "1h");
    assertThrows(
        IllegalStateException.class,
        () ->
            job.run(
                j -> {
                  throw new IllegalStateException("x");
                }));
    assertEquals(List.of(), m.alerts().types());
    assertEquals(JobHealth.SILENCED, m.cw().jobSummary("flaky").health());
    m.cw().unsilence("flaky");
    assertThrows(
        IllegalStateException.class,
        () ->
            job.run(
                j -> {
                  throw new IllegalStateException("y");
                }));
    assertEquals(List.of("failed"), m.alerts().types());
  }

  @Test
  void triageIsAttachedToFailureAlertsAndNeverBlocksThem() {
    Made m = Support.make(b -> b.triage(ctx -> "Probably " + ctx.alert().job() + "'s database."));
    assertThrows(
        IllegalStateException.class,
        () ->
            m.cw()
                .run(
                    "t",
                    j -> {
                      throw new IllegalStateException("x");
                    }));
    assertEquals("Probably t's database.", m.alerts().alerts.get(0).triage());

    Made m2 =
        Support.make(
            b ->
                b.triage(
                    ctx -> {
                      throw new IllegalStateException("api down");
                    }));
    assertThrows(
        IllegalStateException.class,
        () ->
            m2.cw()
                .run(
                    "t",
                    j -> {
                      throw new IllegalStateException("x");
                    }));
    assertEquals(List.of("failed"), m2.alerts().types());
    Alert alert = m2.alerts().alerts.get(0);
    assertNull(alert.triage());
    assertTrue(alert.triageTried(), "tried, and gave nothing");
    assertEquals(List.of("triage for t"), m2.errors().wheres());
  }

  @Test
  void forgetRemovesTheJobAndItsRuns() {
    Made m = Support.make();
    m.cw().run("gone", j -> {});
    assertEquals(1, m.cw().jobs().size());
    m.cw().forget("gone");
    assertEquals(0, m.cw().jobs().size());
    assertNull(m.cw().jobSummary("gone"));
  }

  @Test
  void aFailingAlertChannelDoesNotBreakTheRun() {
    Made m =
        Support.make(
            b ->
                b.alerts(
                    List.of(
                        Channel.of(
                            "broken",
                            (a, ctx) -> {
                              throw new IllegalStateException("no network");
                            }))));
    assertThrows(
        IllegalStateException.class,
        () ->
            m.cw()
                .run(
                    "x",
                    j -> {
                      throw new IllegalStateException("job");
                    }));
    assertEquals(List.of("alert channel broken"), m.errors().wheres());
  }

  /**
   * The SDK's test of a handle kept across a forget: the name had left the declarations, so each
   * later run of the handle wrote the job's row again and the job stayed out of the declared set,
   * where no check wrote it back. Its next run declares it again, as it was, and writes it once.
   */
  @Test
  void aHandleKeptAcrossAForgetDeclaresTheJobAgainOnItsNextRunAndWritesItOnce() throws Exception {
    dev.cronwatch.store.MemoryStore store = new dev.cronwatch.store.MemoryStore();
    Made m = Support.make(b -> b.store(store));
    Job job =
        m.cw()
            .job(
                "nightly",
                JobOptions.builder().schedule("0 2 * * *").description("Nightly export"));
    job.run(j -> {});
    assertEquals(T0, store.getJob("nightly").updatedAt());
    m.cw().forget("nightly");
    assertEquals(List.of(), m.cw().definedJobs());

    m.clock().advance(MIN);
    job.run(j -> {});
    assertEquals(T0 + MIN, store.getJob("nightly").updatedAt(), "written by the run");
    assertEquals(
        List.of("nightly"),
        m.cw().definedJobs().stream().map(Definition::name).toList(),
        "declared again, so checks and sources see it");
    m.clock().advance(MIN);
    job.run(j -> {});
    m.clock().advance(MIN);
    job.run(j -> {});
    assertEquals(T0 + MIN, store.getJob("nightly").updatedAt(), "written once, not on every run");
    assertEquals("Nightly export", m.cw().jobSummary("nightly").definition().description());

    // Forgotten by another process since: this one's check writes it again.
    store.deleteJob("nightly");
    m.cw().check();
    assertTrue(store.getJob("nightly") != null);
    assertEquals(List.of(), m.errors().wheres());
  }

  /**
   * The SDK's test of a forget that lands while a check or a dashboard read is writing
   * declarations: the read had the name from before the forget and wrote it back, declaring the job
   * again. Only a run brings a forgotten job back.
   */
  @Test
  void aForgetThatLandsWhileACheckIsWritingDeclarationsIsNotUndoneByThatCheck() throws Exception {
    for (String read : List.of("check", "jobs")) {
      Support.Wrapped store = new Support.Wrapped();
      Made m = Support.make(b -> b.store(store));
      m.cw().job("keep", JobOptions.builder().schedule("0 2 * * *"));
      m.cw().job("nightly", JobOptions.builder().schedule("0 2 * * *"));
      // Neither is written yet, so the read's own loop is what would write them: it is held at
      // the first, with the second already on its list.
      CountDownLatch held = new CountDownLatch(1);
      store.upsertGate.set(held);
      Thread reading =
          Thread.ofVirtual()
              .start(
                  () -> {
                    if (read.equals("check")) {
                      m.cw().check();
                    } else {
                      m.cw().jobs();
                    }
                  });
      assertTrue(store.upsertEntered.await(10, java.util.concurrent.TimeUnit.SECONDS), read);
      m.cw().forget("nightly");
      held.countDown();
      reading.join();
      assertEquals(
          List.of("keep"),
          m.cw().definedJobs().stream().map(Definition::name).toList(),
          read + ": still forgotten here");
      assertNull(store.getJob("nightly"), read + ": not written again");
      assertNull(store.getState("nightly"), read + ": no state left behind");
      // Its next run still brings it back.
      m.cw().run("nightly", j -> {});
      assertTrue(store.getJob("nightly") != null, read);
      assertEquals(List.of(), m.errors().wheres(), read);
    }

    // A read of the one job (the dashboard's job page) that had its declaration from before the
    // forget: its write, asked for with the name read back as a run's is, leaves it forgotten.
    dev.cronwatch.store.MemoryStore store = new dev.cronwatch.store.MemoryStore();
    Made m = Support.make(b -> b.store(store));
    m.cw().job("keep", JobOptions.builder().schedule("0 2 * * *"));
    Job nightly = m.cw().job("nightly", JobOptions.builder().schedule("0 2 * * *"));
    m.cw().forget("nightly");
    m.cw().core.sync(nightly.def(), true, false);
    assertNull(m.cw().jobSummary("nightly"));
    assertEquals(List.of("keep"), m.cw().definedJobs().stream().map(Definition::name).toList());
    assertNull(store.getJob("nightly"), "jobSummary: not written again");
    assertNull(store.getState("nightly"));
    m.cw().run("nightly", j -> {});
    assertTrue(store.getJob("nightly") != null);
    assertEquals("0 2 * * *", m.cw().jobSummary("nightly").definition().get("schedule"));
  }

  /**
   * The SDK's test of a job that hangs on every run: each new start closed stuck, so every timeout
   * opened it, and alerted, again. Stuck stays open until a run finishes ok.
   */
  @Test
  void aJobThatHangsOnEveryRunAlertsStuckOnceAndRecoversOnItsFirstSuccess() {
    Made m = Support.make();
    Job job =
        m.cw()
            .job(
                "hangs",
                JobOptions.builder().schedule("*/5 * * * *").timezone("UTC").timeout("10m"));
    // Twelve runs, five minutes apart, none of which ever finishes.
    List<RunHandle> handles = new java.util.ArrayList<>();
    for (int i = 0; i < 12; i++) {
      handles.add(job.start());
      m.clock().advance(5 * MIN);
      m.cw().check();
    }
    assertEquals(List.of("stuck"), m.alerts().types());
    assertEquals(JobHealth.STUCK, m.cw().jobSummary("hangs").health());
    // A run that fails alerts as failed by its own rule; stuck stays open beside it.
    assertThrows(
        IllegalStateException.class,
        () ->
            job.run(
                j -> {
                  throw new IllegalStateException("boom");
                }));
    assertEquals(List.of("stuck", "failed"), m.alerts().types());
    assertEquals(
        List.of("failed", "stuck"),
        m.cw().jobSummary("hangs").open().stream().map(Condition::value).sorted().toList());
    // A hung run that reports failing late is not counted again and alerts nothing.
    handles.get(0).fail(new IllegalStateException("late"));
    assertEquals(List.of("stuck", "failed"), m.alerts().types());
    // The first success closes both with one recovery.
    job.run(j -> {});
    assertEquals(List.of("stuck", "failed", "recovered"), m.alerts().types());
    AlertDetails.Recovered recovered = (AlertDetails.Recovered) m.alerts().alerts.get(2).details();
    assertEquals(
        List.of("failed", "stuck"),
        recovered.after().stream().map(Condition::value).sorted().toList());
    assertEquals(List.of(), m.cw().jobSummary("hangs").open());
  }

  /** The SDK's test beside it: the stuck run's own late success is what recovers. */
  @Test
  void aStuckRunThatFinishesOkLateClosesStuckWithARecoveryThoughOtherRunsStartedMeanwhile() {
    Made m = Support.make();
    Job job = m.cw().job("slow-queue", JobOptions.builder().timeout("10m"));
    RunHandle first = job.start();
    m.clock().advance(11 * MIN);
    m.cw().check();
    assertEquals(List.of("stuck"), m.alerts().types());
    RunHandle second = job.start();
    assertEquals(
        List.of(Condition.STUCK),
        m.cw().jobSummary("slow-queue").open(),
        "a start leaves stuck open");
    first.finish();
    assertEquals(List.of("stuck", "recovered"), m.alerts().types());
    second.finish();
    assertEquals(List.of("stuck", "recovered"), m.alerts().types());
  }

  /** {@code inner}, with each {@code insertRun} first handed to {@code before} with its count. */
  private static dev.cronwatch.store.Store insertsThrough(
      dev.cronwatch.store.Store inner, java.util.function.IntConsumer before) {
    java.util.concurrent.atomic.AtomicInteger inserts =
        new java.util.concurrent.atomic.AtomicInteger();
    return (dev.cronwatch.store.Store)
        java.lang.reflect.Proxy.newProxyInstance(
            dev.cronwatch.store.Store.class.getClassLoader(),
            new Class<?>[] {dev.cronwatch.store.Store.class},
            (proxy, method, args) -> {
              if (method.getName().equals("insertRun")) {
                before.accept(inserts.incrementAndGet());
              }
              try {
                return method.invoke(inner, args);
              } catch (java.lang.reflect.InvocationTargetException e) {
                throw e.getCause();
              }
            });
  }

  private static void awaitQuietly(CountDownLatch latch) {
    try {
      latch.await();
    } catch (InterruptedException e) {
      Thread.currentThread().interrupt();
    }
  }

  /**
   * The SDK's test of a store that hangs while a run's start is written: the job waited on it for
   * as long as it hung. Past the deadline it runs unrecorded, and the write that lands late leaves
   * no running row, whether it lands before the finish or after it.
   */
  @Test
  void aStoreThatHangsRecordingARunsStartDoesNotHoldTheJobBackAndLeavesNoRunningRow()
      throws Exception {
    assertEquals(5_000, new Core.Timings().startDeadlineMs, "the SDK's START_DEADLINE_MS");
    assertEquals("5s", dev.cronwatch.internal.duration.Durations.format(5_000));
    for (String lands : List.of("after the finish", "before the finish")) {
      dev.cronwatch.store.MemoryStore base = new dev.cronwatch.store.MemoryStore();
      CountDownLatch gate = new CountDownLatch(1);
      // The first insert (the run's start) hangs until released.
      dev.cronwatch.store.Store store =
          insertsThrough(
              base,
              n -> {
                if (n == 1) {
                  awaitQuietly(gate);
                }
              });
      Made m =
          Support.make(
              b -> {
                b.store(store);
                b.timings.startDeadlineMs = 400;
              });
      Job job = m.cw().job("nightly");
      CountDownLatch ran = new CountDownLatch(1);
      CountDownLatch finish = new CountDownLatch(1);
      java.util.concurrent.atomic.AtomicReference<String> answer =
          new java.util.concurrent.atomic.AtomicReference<>();
      long began = System.nanoTime();
      Thread running =
          Support.background(
              () ->
                  answer.set(
                      job.call(
                          j -> {
                            ran.countDown();
                            j.log("did the work");
                            finish.await();
                            return "done";
                          })));
      assertTrue(ran.await(10, java.util.concurrent.TimeUnit.SECONDS), "the job ran");
      assertTrue(
          System.nanoTime() - began >= 390_000_000L, lands + ": only once the deadline passed");
      assertEquals(
          List.of(
              "recording nightly: the store took longer than 400ms to record the start of nightly;"
                  + " running it unrecorded"),
          List.of(m.errors().wheres().get(0) + ": " + m.errors().messages().get(0)));
      if (lands.equals("before the finish")) {
        // The late start lands while the job still runs, and the finish is written over it.
        gate.countDown();
        Support.await("the late row", () -> !runningOf(base).isEmpty());
        finish.countDown();
        running.join();
      } else {
        // The finish is recorded without the start, and the late start is then refused.
        finish.countDown();
        running.join();
        assertEquals(RunStatus.OK, m.cw().runs("nightly", 5).get(0).status());
        gate.countDown();
      }
      assertEquals("done", answer.get());
      m.cw().close();
      List<Run> runs = base.listRuns("nightly", 5);
      assertEquals(1, runs.size(), lands);
      assertEquals(RunStatus.OK, runs.get(0).status(), lands);
      assertEquals("did the work", runs.get(0).output(), lands);
      assertEquals(List.of(), base.runningRuns(), lands);
      assertEquals(1, m.errors().entries.size(), lands + ": " + m.errors().messages());
      assertEquals(List.of(), m.alerts().types(), lands);
    }
  }

  private static List<Run> runningOf(dev.cronwatch.store.Store store) {
    try {
      return store.runningRuns();
    } catch (Exception e) {
      throw new IllegalStateException(e);
    }
  }

  /** The SDK's test beside it: the finish failed, so the late start is finished when it lands. */
  @Test
  void aStartWriteThatLandsAfterTheFinishFailedIsFinishedThenNotLeftRunning() throws Exception {
    dev.cronwatch.store.MemoryStore base = new dev.cronwatch.store.MemoryStore();
    CountDownLatch gate = new CountDownLatch(1);
    dev.cronwatch.store.Store store =
        insertsThrough(
            base,
            n -> {
              if (n == 1) {
                awaitQuietly(gate);
                return;
              }
              // The finish finds the store down, not hanging.
              throw new IllegalStateException("store down");
            });
    Made m =
        Support.make(
            b -> {
              b.store(store);
              b.timings.startDeadlineMs = 300;
            });
    IllegalStateException own =
        assertThrows(
            IllegalStateException.class,
            () ->
                m.cw()
                    .job("nightly")
                    .run(
                        j -> {
                          throw new IllegalStateException("boom");
                        }));
    assertEquals("boom", own.getMessage(), "the job's own error is what run() throws");
    assertEquals(List.of("recording nightly", "recording nightly"), m.errors().wheres());
    assertEquals(List.of(), m.cw().runs("nightly", 5));
    // The start write lands at last, after the finish gave up: the run is finished and judged now.
    gate.countDown();
    Support.await("the late finish", () -> m.alerts().types().equals(List.of("failed")));
    List<Run> runs = m.cw().runs("nightly", 5);
    assertEquals(1, runs.size());
    assertEquals(RunStatus.FAILED, runs.get(0).status());
    assertTrue(String.valueOf(runs.get(0).error()).contains("boom"), runs.get(0).toString());
    assertEquals(List.of(), base.runningRuns());
    assertEquals(List.of("failed"), m.alerts().types());
  }

  /**
   * The SDK's test of the window between a finish that fails and its conclusion: the finish is
   * written as a run of its own because the start had not landed, the start lands while that write
   * is under way and leaves its row to the finish, and the finish then fails without seeing it (its
   * insert and its read back both fail). The finish is written over the landed start before the run
   * concludes, so no row is left running by a live process.
   */
  @Test
  void aStartThatLandsWhileTheFinishIsFailingIsFinishedBeforeTheRunConcludes() throws Exception {
    dev.cronwatch.store.MemoryStore base = new dev.cronwatch.store.MemoryStore();
    CountDownLatch startGate = new CountDownLatch(1);
    CountDownLatch finishGate = new CountDownLatch(1);
    java.util.concurrent.atomic.AtomicInteger inserts =
        new java.util.concurrent.atomic.AtomicInteger();
    java.util.concurrent.atomic.AtomicBoolean getRunDown =
        new java.util.concurrent.atomic.AtomicBoolean();
    dev.cronwatch.store.Store store =
        (dev.cronwatch.store.Store)
            java.lang.reflect.Proxy.newProxyInstance(
                dev.cronwatch.store.Store.class.getClassLoader(),
                new Class<?>[] {dev.cronwatch.store.Store.class},
                (proxy, method, args) -> {
                  if (method.getName().equals("insertRun")) {
                    if (inserts.incrementAndGet() == 1) {
                      awaitQuietly(startGate);
                    } else {
                      // The finish, written as a run of its own, hangs and then fails.
                      awaitQuietly(finishGate);
                      throw new IllegalStateException("store down");
                    }
                  }
                  // The read after that failure (was the run recorded meanwhile?) fails too, once.
                  if (method.getName().equals("getRun") && getRunDown.getAndSet(false)) {
                    throw new IllegalStateException("store down");
                  }
                  try {
                    return method.invoke(base, args);
                  } catch (java.lang.reflect.InvocationTargetException e) {
                    throw e.getCause();
                  }
                });
    Made m =
        Support.make(
            b -> {
              b.store(store);
              b.timings.startDeadlineMs = 300;
            });
    java.util.concurrent.atomic.AtomicReference<Throwable> thrown =
        new java.util.concurrent.atomic.AtomicReference<>();
    Thread running =
        Thread.ofVirtual()
            .start(
                () -> {
                  try {
                    m.cw()
                        .job("nightly")
                        .run(
                            j -> {
                              throw new IllegalStateException("boom");
                            });
                  } catch (RuntimeException e) {
                    thrown.set(e);
                  }
                });
    Support.await("the job has run and its finish is being written", () -> inserts.get() == 2);
    // The start lands now, while the finish is still under way.
    startGate.countDown();
    Support.await("the start's row", () -> !runningOf(base).isEmpty());
    // Then the finish fails, and cannot see that the start has landed.
    getRunDown.set(true);
    finishGate.countDown();
    running.join();
    assertEquals("boom", String.valueOf(thrown.get().getMessage()));
    List<Run> runs = m.cw().runs("nightly", 5);
    assertEquals(1, runs.size());
    assertEquals(RunStatus.FAILED, runs.get(0).status());
    assertEquals(List.of(), base.runningRuns(), "no row left running by a live process");
    assertEquals(List.of("failed"), m.alerts().types());
    assertEquals(List.of("recording nightly", "recording nightly"), m.errors().wheres());
    assertTrue(m.errors().messages().get(0).startsWith("the store took longer than 300ms"));
    assertTrue(
        m.errors().messages().get(1).contains("store down"), m.errors().messages().toString());
  }
}
