package dev.cronwatch.jobrunr;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertTrue;

import dev.cronwatch.Cronwatch;
import dev.cronwatch.JobContext;
import dev.cronwatch.Run;
import dev.cronwatch.RunStatus;
import dev.cronwatch.StoredJob;
import dev.cronwatch.store.MemoryStore;
import java.time.Duration;
import java.time.ZoneId;
import java.util.List;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.function.BooleanSupplier;
import org.jobrunr.configuration.JobRunr;
import org.jobrunr.jobs.annotations.Job;
import org.jobrunr.scheduling.JobScheduler;
import org.jobrunr.server.BackgroundJobServerConfiguration;
import org.jobrunr.storage.InMemoryStorageProvider;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.Test;

/**
 * A real JobRunr background job server over its in-memory storage: the recurring jobs declared with
 * their schedules, each attempt a run (a retry a new run), a job that is not recurring watched when
 * named, a recurring job deleted, and the check job.
 */
class CronwatchJobRunrTest {
  /** The jobs, as JobRunr runs them: public static methods. */
  public static final class Work {
    static final AtomicInteger FLAKY = new AtomicInteger();

    private Work() {}

    /** Logs through the current run. */
    public static void ok() {
      JobContext run = Cronwatch.current();
      if (run != null) {
        run.log("done");
      }
    }

    /** A job with a name of its own. */
    @Job(name = "report")
    public static void report() {
      ok();
    }

    /** Fails its first attempt, and JobRunr tries it once more. */
    @Job(name = "flaky", retries = 1)
    public static void flaky() {
      if (FLAKY.incrementAndGet() == 1) {
        throw new IllegalStateException("not yet");
      }
    }
  }

  @AfterEach
  void destroy() {
    JobRunr.destroy();
  }

  private static Cronwatch client(MemoryStore store, List<String> errors) {
    return Cronwatch.builder()
        .store(store)
        .alerts(List.of())
        .noShutdownHook()
        .onError((where, error) -> errors.add(where + ": " + error.getMessage()))
        .build();
  }

  private static String stored(MemoryStore store, String name) throws Exception {
    StoredJob job = store.getJob(name);
    assertNotNull(job, name + " is not stored");
    return job.definition().toJson();
  }

  private static List<Run> finished(Cronwatch cw, String name) {
    return cw.runs(name, 20).stream().filter(r -> !r.status().equals(RunStatus.RUNNING)).toList();
  }

  /**
   * Waits up to forty seconds (JobRunr polls every five at the least), and fails with {@code what}.
   */
  private static void await(String what, BooleanSupplier condition) throws InterruptedException {
    long deadline = System.nanoTime() + 40_000_000_000L;
    while (!condition.getAsBoolean()) {
      if (System.nanoTime() > deadline) {
        assertTrue(condition.getAsBoolean(), "waited forty seconds for: " + what);
        return;
      }
      Thread.sleep(50);
    }
  }

  @Test
  void recurringJobsAreDeclaredAndOnesDeletedLoseTheirSchedule() throws Exception {
    MemoryStore store = new MemoryStore();
    List<String> errors = new CopyOnWriteArrayList<>();
    InMemoryStorageProvider storage = new InMemoryStorageProvider();
    JobScheduler scheduler =
        JobRunr.configure().useStorageProvider(storage).initialize().getJobScheduler();
    scheduler.scheduleRecurrently(
        "nightly", "0 2 * * *", ZoneId.of("Europe/London"), () -> Work.ok());
    scheduler.scheduleRecurrently("often", Duration.ofHours(2), () -> Work.ok());
    CronwatchJobRunr.scheduleCheck(scheduler);
    try (Cronwatch cw = client(store, errors);
        CronwatchJobRunr w =
            CronwatchJobRunr.watch(cw, storage, JobRunrOptions.defaults().app("billing"))) {
      assertTrue(w.settle(Duration.ofSeconds(10)));
      String tags = "\"tags\":[\"jobrunr\",\"jobrunr:billing\"]";
      assertEquals(
          "{\"schedule\":\"0 2 * * *\",\"timezone\":\"Europe/London\","
              + tags
              + ",\"name\":\"nightly\"}",
          stored(store, "nightly"));
      assertEquals(
          "{\"schedule\":\"every 2h\"," + tags + ",\"name\":\"often\"}", stored(store, "often"));
      assertNull(store.getJob(CronwatchJobRunr.CHECK_ID), "the check is never a job");

      scheduler.deleteRecurringJob("often");
      w.sync();
      assertEquals(
          "{\"description\":\"A scheduled task (no longer scheduled)\","
              + tags
              + ",\"name\":\"often\"}",
          stored(store, "often"));
      // Read when watched and again by the sync, and walked beside JobRunr's fire times once.
      assertEquals(1, w.walks.get(), "an unchanged cron is walked once");
      CronwatchJobRunr.runCheck();
      assertTrue(errors.isEmpty(), errors.toString());
    }
  }

  @Test
  void eachAttemptIsARunAndARetryIsANewOne() throws Exception {
    MemoryStore store = new MemoryStore();
    List<String> errors = new CopyOnWriteArrayList<>();
    InMemoryStorageProvider storage = new InMemoryStorageProvider();
    try (Cronwatch cw = client(store, errors);
        CronwatchJobRunr w =
            CronwatchJobRunr.watch(
                cw,
                storage,
                JobRunrOptions.defaults().app("billing").watchJob("report").watchJob("flaky"))) {
      JobScheduler scheduler =
          JobRunr.configure()
              .useStorageProvider(storage)
              .withJobFilter(w)
              .useBackgroundJobServer(
                  BackgroundJobServerConfiguration.usingStandardBackgroundJobServerConfiguration()
                      .andPollIntervalInSeconds(5)
                      .andWorkerCount(2))
              .initialize()
              .getJobScheduler();
      scheduler.enqueue(() -> Work.report());
      scheduler.enqueue(() -> Work.flaky());
      scheduler.enqueue(() -> Work.ok()); // neither recurring nor named: not watched
      scheduler.scheduleRecurrently("tick", Duration.ofSeconds(5), () -> Work.ok());

      await("the named job's run", () -> finished(cw, "report").size() == 1);
      Run report = finished(cw, "report").get(0);
      assertEquals(RunStatus.OK, report.status());
      assertEquals("done", report.output());
      assertEquals("jobrunr", report.trigger());
      assertTrue(report.id().startsWith("jobrunr:billing:"), report.id());
      assertTrue(report.id().endsWith(":1"), "the first attempt");

      await("the retry's run", () -> finished(cw, "flaky").size() == 2);
      List<Run> flaky = finished(cw, "flaky");
      assertEquals(RunStatus.OK, flaky.get(0).status());
      assertEquals(RunStatus.FAILED, flaky.get(1).status());
      assertTrue(
          flaky.get(1).error().startsWith("IllegalStateException: not yet"), flaky.get(1).error());
      assertTrue(flaky.get(0).id().endsWith(":2") && flaky.get(1).id().endsWith(":1"));

      await("the recurring job's run", () -> !finished(cw, "tick").isEmpty());
      assertTrue(cw.jobs().stream().noneMatch(j -> j.name().contains("Work.ok")));
      assertTrue(errors.isEmpty(), errors.toString());
    }
  }

  /**
   * {@code job} as JobRunr hands it to a filter on its {@code attempt}th attempt, then {@code
   * more}.
   */
  private static org.jobrunr.jobs.Job attempt(
      org.jobrunr.jobs.Job job, int attempt, org.jobrunr.jobs.states.JobState... more) {
    List<org.jobrunr.jobs.states.JobState> states = new java.util.ArrayList<>();
    states.add(new org.jobrunr.jobs.states.EnqueuedState());
    for (int i = 0; i < attempt; i++) {
      states.add(new org.jobrunr.jobs.states.ProcessingState(java.util.UUID.randomUUID(), "test"));
    }
    states.addAll(List.of(more));
    org.jobrunr.jobs.Job made =
        new org.jobrunr.jobs.Job(
            job.getId(),
            0,
            job.getJobDetails(),
            states,
            new java.util.concurrent.ConcurrentHashMap<>());
    made.setJobName(job.getJobName());
    return made;
  }

  @Test
  void anAttemptJobRunrNeverReportsTheEndOfIsClosedFailedWhenTheNextOpens() throws Exception {
    // An Error from the job skips every JobRunr filter: its run stayed open in the filter, and the
    // current run stayed set, until the job was retried.
    MemoryStore store = new MemoryStore();
    List<String> errors = new CopyOnWriteArrayList<>();
    InMemoryStorageProvider storage = new InMemoryStorageProvider();
    try (Cronwatch cw = client(store, errors);
        CronwatchJobRunr w =
            CronwatchJobRunr.watch(
                cw, storage, JobRunrOptions.defaults().app("billing").watchJob("report"))) {
      JobScheduler scheduler =
          JobRunr.configure().useStorageProvider(storage).initialize().getJobScheduler();
      org.jobrunr.jobs.Job job = storage.getJobById(scheduler.enqueue(() -> Work.report()));
      w.onProcessing(attempt(job, 1));
      w.onProcessing(attempt(job, 2));
      w.onProcessingSucceeded(attempt(job, 2));
      List<Run> runs = finished(cw, "report");
      assertEquals(2, runs.size(), runs.toString());
      assertEquals(RunStatus.OK, runs.get(0).status());
      assertEquals(RunStatus.FAILED, runs.get(1).status());
      assertTrue(runs.get(1).id().endsWith(":1"), runs.get(1).id());
      assertTrue(
          runs.get(1).error().contains("without JobRunr reporting how"), runs.get(1).error());
      assertNull(Cronwatch.current(), "no run is left current on the worker's thread");
      assertTrue(errors.isEmpty(), errors.toString());
    }
  }

  /** What this thread holds of a run: the current run's id and the MDC keys, or nulls. */
  private static List<String> held() {
    JobContext current = Cronwatch.current();
    return java.util.Arrays.asList(
        current == null ? null : current.runId(),
        org.slf4j.MDC.get("cronwatch_job"),
        org.slf4j.MDC.get("cronwatch_run"));
  }

  @Test
  void aPooledWorkerKeepsNoRunOfAnAttemptJobRunrNeverReportedTheEndOf() throws Exception {
    // An Error skips every filter, so the attempt's run stays current on the worker's thread, with
    // its MDC keys, until something closes it: on that thread as it starts its next job, or on
    // another (the job's next attempt on another worker, or a state JobRunr applied), after which
    // the worker clears what that run left.
    MemoryStore store = new MemoryStore();
    List<String> errors = new CopyOnWriteArrayList<>();
    InMemoryStorageProvider storage = new InMemoryStorageProvider();
    java.util.concurrent.ExecutorService worker =
        java.util.concurrent.Executors.newSingleThreadExecutor();
    List<String> none = java.util.Arrays.asList(null, null, null);
    try (Cronwatch cw = client(store, errors);
        CronwatchJobRunr w =
            CronwatchJobRunr.watch(
                cw, storage, JobRunrOptions.defaults().app("billing").watchJob("report"))) {
      JobScheduler scheduler =
          JobRunr.configure().useStorageProvider(storage).initialize().getJobScheduler();
      org.jobrunr.jobs.Job job = storage.getJobById(scheduler.enqueue(() -> Work.report()));
      org.jobrunr.jobs.Job other = storage.getJobById(scheduler.enqueue(() -> Work.ok()));

      // Closed on the worker's thread as it starts a job that is not watched.
      List<String> opened =
          worker
              .submit(
                  () -> {
                    w.onProcessing(attempt(job, 1));
                    return held();
                  })
              .get();
      assertEquals("report", opened.get(1));
      assertEquals(opened.get(0), opened.get(2));
      assertEquals(
          none,
          worker
              .submit(
                  () -> {
                    w.onProcessing(attempt(other, 1));
                    return held();
                  })
              .get());
      List<Run> runs = finished(cw, "report");
      assertEquals(1, runs.size(), runs.toString());
      assertTrue(runs.get(0).error().contains("without JobRunr reporting how"));

      // Closed from another thread: the worker clears what it left as it starts its next job.
      opened =
          worker
              .submit(
                  () -> {
                    w.onProcessing(attempt(job, 2));
                    return held();
                  })
              .get();
      assertEquals("report", opened.get(1));
      org.jobrunr.jobs.states.ProcessingState processing =
          new org.jobrunr.jobs.states.ProcessingState(java.util.UUID.randomUUID(), "test");
      org.jobrunr.jobs.states.FailedState orphaned =
          new org.jobrunr.jobs.states.FailedState(
              "Orphaned job", new IllegalThreadStateException("the server stopped"));
      w.onStateApplied(attempt(job, 2, orphaned), processing, orphaned);
      assertEquals(2, finished(cw, "report").size());
      assertEquals(opened, worker.submit(CronwatchJobRunrTest::held).get(), "left behind");
      assertEquals(
          none,
          worker
              .submit(
                  () -> {
                    w.onProcessing(attempt(other, 2));
                    return held();
                  })
              .get());

      // And a watched job's run, opened over such a leftover, ends with nothing to put back.
      worker.submit(() -> w.onProcessing(attempt(job, 3))).get();
      w.onStateApplied(attempt(job, 3, orphaned), processing, orphaned);
      assertEquals(
          none,
          worker
              .submit(
                  () -> {
                    w.onProcessing(attempt(job, 4));
                    w.onProcessingSucceeded(attempt(job, 4));
                    return held();
                  })
              .get());
      assertEquals(4, finished(cw, "report").size());
      assertTrue(errors.isEmpty(), errors.toString());
    } finally {
      worker.shutdownNow();
    }
  }

  @Test
  void anAttemptDeletedWhileItRanIsGivenBackNotFailed() throws Exception {
    MemoryStore store = new MemoryStore();
    List<String> errors = new CopyOnWriteArrayList<>();
    InMemoryStorageProvider storage = new InMemoryStorageProvider();
    try (Cronwatch cw = client(store, errors);
        CronwatchJobRunr w =
            CronwatchJobRunr.watch(
                cw, storage, JobRunrOptions.defaults().app("billing").watchJob("report"))) {
      JobScheduler scheduler =
          JobRunr.configure().useStorageProvider(storage).initialize().getJobScheduler();
      org.jobrunr.jobs.Job job = storage.getJobById(scheduler.enqueue(() -> Work.report()));
      w.onProcessing(attempt(job, 1));
      w.onProcessingFailed(
          attempt(job, 1, new org.jobrunr.jobs.states.DeletedState("deleted from the dashboard")),
          new RuntimeException(new InterruptedException("sleep interrupted")));
      assertEquals(List.of(), cw.runs("report", 20), "no run: the attempt was given back");
      assertNull(Cronwatch.current());
      assertTrue(errors.isEmpty(), errors.toString());
    }
  }

  @Test
  void anAttemptJobRunrMovesOnFromIsClosedWhenItsStateIsApplied() throws Exception {
    // An orphan the server's own JobRunr fails, and an attempt scheduled again while its run was
    // open: JobRunr reports neither end to onProcessingSucceeded or onProcessingFailed.
    MemoryStore store = new MemoryStore();
    List<String> errors = new CopyOnWriteArrayList<>();
    InMemoryStorageProvider storage = new InMemoryStorageProvider();
    try (Cronwatch cw = client(store, errors);
        CronwatchJobRunr w =
            CronwatchJobRunr.watch(
                cw, storage, JobRunrOptions.defaults().app("billing").watchJob("report"))) {
      JobScheduler scheduler =
          JobRunr.configure().useStorageProvider(storage).initialize().getJobScheduler();
      org.jobrunr.jobs.Job job = storage.getJobById(scheduler.enqueue(() -> Work.report()));
      org.jobrunr.jobs.states.ProcessingState processing =
          new org.jobrunr.jobs.states.ProcessingState(java.util.UUID.randomUUID(), "test");
      w.onProcessing(attempt(job, 1));
      org.jobrunr.jobs.states.FailedState orphaned =
          new org.jobrunr.jobs.states.FailedState(
              "Orphaned job", new IllegalThreadStateException("the server stopped"));
      w.onStateApplied(attempt(job, 1, orphaned), processing, orphaned);
      w.onProcessing(attempt(job, 2));
      org.jobrunr.jobs.states.ScheduledState again =
          new org.jobrunr.jobs.states.ScheduledState(java.time.Instant.now(), "again");
      w.onStateApplied(attempt(job, 2, again), processing, again);
      List<Run> runs = finished(cw, "report");
      assertEquals(2, runs.size(), runs.toString());
      assertTrue(runs.get(1).id().endsWith(":1"), runs.get(1).id());
      assertTrue(
          runs.get(1).error().startsWith("IllegalThreadStateException: the server stopped"),
          runs.get(1).error());
      assertTrue(
          runs.get(0).error().contains("without JobRunr reporting how"), runs.get(0).error());
      // Already closed: a later report of either end changes nothing.
      w.onProcessingSucceeded(attempt(job, 2));
      assertEquals(2, finished(cw, "report").size());
      assertNull(Cronwatch.current());
      assertTrue(errors.isEmpty(), errors.toString());
    }
  }

  @Test
  void theCheckClosesARunItMarkedTimedOutOnceJobRunrHasMovedOn() throws Exception {
    MemoryStore store = new MemoryStore();
    List<String> errors = new CopyOnWriteArrayList<>();
    InMemoryStorageProvider storage = new InMemoryStorageProvider();
    java.util.concurrent.atomic.AtomicLong clock =
        new java.util.concurrent.atomic.AtomicLong(1_767_605_400_000L);
    try (Cronwatch cw =
            Cronwatch.builder()
                .store(store)
                .alerts(List.of())
                .clock(clock::get)
                .noShutdownHook()
                .onError((where, error) -> errors.add(where + ": " + error.getMessage()))
                .build();
        CronwatchJobRunr w =
            CronwatchJobRunr.watch(
                cw, storage, JobRunrOptions.defaults().app("billing").watchJob("report"))) {
      JobScheduler scheduler =
          JobRunr.configure().useStorageProvider(storage).initialize().getJobScheduler();
      org.jobrunr.jobs.Job job = storage.getJobById(scheduler.enqueue(() -> Work.report()));
      w.onProcessing(attempt(job, 1));
      w.checkNow();
      assertEquals(RunStatus.RUNNING, cw.runs("report", 5).get(0).status(), "within its timeout");
      clock.addAndGet(Duration.ofHours(3).toMillis());
      w.checkNow();
      Run run = cw.runs("report", 5).get(0);
      assertTrue(
          run.error() != null && run.error().contains("without JobRunr reporting how"),
          run.toString());
      assertTrue(errors.isEmpty(), errors.toString());
    }
  }

  @Test
  void theCheckRunsEveryLiveWatchAndFailsWithNone() throws Exception {
    InMemoryStorageProvider storage = new InMemoryStorageProvider();
    JobScheduler scheduler =
        JobRunr.configure().useStorageProvider(storage).initialize().getJobScheduler();
    scheduler.scheduleRecurrently("often", Duration.ofHours(2), () -> Work.ok());
    MemoryStore first = new MemoryStore();
    MemoryStore second = new MemoryStore();
    List<String> errors = new CopyOnWriteArrayList<>();
    try (Cronwatch one = client(first, errors);
        Cronwatch two = client(second, errors)) {
      CronwatchJobRunr a = CronwatchJobRunr.watch(one, storage);
      CronwatchJobRunr b = CronwatchJobRunr.watch(two, storage);
      try {
        assertTrue(a.settle(Duration.ofSeconds(10)) && b.settle(Duration.ofSeconds(10)));
        scheduler.deleteRecurringJob("often");
        // A second watch no longer replaces the first: the check syncs both.
        CronwatchJobRunr.runCheck();
        assertTrue(stored(first, "often").contains("no longer scheduled"), stored(first, "often"));
        assertTrue(
            stored(second, "often").contains("no longer scheduled"), stored(second, "often"));
        a.close();
        CronwatchJobRunr.runCheck();
      } finally {
        a.close();
        b.close();
      }
      IllegalStateException none =
          org.junit.jupiter.api.Assertions.assertThrows(
              IllegalStateException.class, CronwatchJobRunr::runCheck);
      assertEquals(CronwatchJobRunr.NO_WATCH, none.getMessage());
      assertTrue(errors.isEmpty(), errors.toString());
    }
  }

  @Test
  void anExceptionIsTheJobsOwn() {
    IllegalStateException own = new IllegalStateException("x");
    assertEquals(
        own, CronwatchJobRunr.unwrap(new java.lang.reflect.InvocationTargetException(own)));
    assertEquals(own, CronwatchJobRunr.unwrap(own));
  }
}
