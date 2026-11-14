package dev.cronwatch.spring;

import dev.cronwatch.Cronwatch;
import dev.cronwatch.Job;
import dev.cronwatch.ObservedRun;
import dev.cronwatch.RunOptions;
import io.micrometer.observation.Observation;
import io.micrometer.observation.ObservationHandler;
import java.lang.reflect.InvocationTargetException;
import java.lang.reflect.UndeclaredThrowableException;
import java.util.UUID;
import org.jspecify.annotations.Nullable;
import org.springframework.scheduling.support.ScheduledTaskObservationContext;

/**
 * Each {@code @Scheduled} invocation as a run, from the {@code Observation} Spring makes of it: the
 * run is opened on {@code onStart}, in the thread that runs the method, so {@link
 * Cronwatch#current()} and {@code job.log} work inside it, and closed on {@code onStop}, failed
 * with the error the observation carries. A method under ShedLock's {@code @SchedulerLock} whose
 * lock another instance held, and which so returned without an error, is given back rather than
 * judged; a lock some other code asked for inside a method that is not itself locked changes
 * nothing, since that method ran. Never throws into Spring's task.
 */
final class ScheduledRuns implements ObservationHandler<ScheduledTaskObservationContext> {
  /**
   * The trigger of the runs it records: the integration's name, as its tag. Runs recorded before
   * 0.11 carry {@code scheduled}; nothing reads the trigger back.
   */
  static final String TRIGGER = CronwatchScheduling.TAG;

  private final CronwatchScheduling scheduling;
  private final boolean shedLock;
  private volatile boolean closed;

  ScheduledRuns(CronwatchScheduling scheduling, boolean shedLock) {
    this.scheduling = scheduling;
    this.shedLock = shedLock;
  }

  /**
   * Stops opening runs, for a context that is closing: a run open now is still closed when its
   * method returns. Micrometer has no way to take a handler off a registry, so this is how it is
   * detached.
   */
  void close() {
    closed = true;
  }

  @Override
  public boolean supportsContext(Observation.Context context) {
    return context instanceof ScheduledTaskObservationContext;
  }

  @Override
  public void onStart(ScheduledTaskObservationContext context) {
    ObservedRun run = null;
    boolean mayTakeBack = false;
    try {
      if (!closed) {
        CronwatchScheduling.Target target =
            scheduling.target(context.getTargetClass(), context.getMethod());
        if (target != null && !target.reactive()) {
          mayTakeBack = shedLock && target.locked();
          run = open(target, mayTakeBack);
        }
      }
    } catch (RuntimeException e) {
      scheduling.client().reportError(e, "scheduled");
    }
    context.put(RunFrames.Frame.class, RunFrames.push(run, run != null && mayTakeBack));
  }

  private @Nullable ObservedRun open(CronwatchScheduling.Target target, boolean mayTakeBack) {
    Job job = scheduling.job(target);
    if (job == null) {
      return null;
    }
    RunOptions options =
        // The id's prefix is a key, not the trigger, and stays scheduled: through 1.x.
        RunOptions.trigger(TRIGGER)
            .withId("scheduled:" + scheduling.appSlug() + ":" + UUID.randomUUID());
    if (mayTakeBack) {
      options = options.mayTakeBack();
    }
    return job.open(options);
  }

  @Override
  public void onStop(ScheduledTaskObservationContext context) {
    RunFrames.Frame frame = context.get(RunFrames.Frame.class);
    if (frame == null) {
      return;
    }
    context.remove(RunFrames.Frame.class);
    RunFrames.pop(frame);
    ObservedRun run = frame.run;
    if (run == null) {
      return;
    }
    try {
      Throwable error = unwrap(context.getError());
      if (frame.mayTakeBack && error == null && Boolean.FALSE.equals(frame.lockTaken)) {
        // Another instance held the method's own lock: it did not run here.
        run.takeBack();
      } else {
        // A method that is not itself locked ran, whatever a lock asked for inside it answered,
        // and one that threw is a failure to record.
        run.close(error);
      }
    } catch (RuntimeException e) {
      scheduling.client().reportError(e, "scheduled");
    }
  }

  /** The method's own throwable, from inside the wrappers reflection puts around a checked one. */
  static @Nullable Throwable unwrap(@Nullable Throwable error) {
    Throwable t = error;
    for (int depth = 0; depth < 8 && t != null; depth++) {
      Throwable inner =
          t instanceof UndeclaredThrowableException u
              ? u.getUndeclaredThrowable()
              : t instanceof InvocationTargetException i ? i.getTargetException() : null;
      if (inner == null) {
        return t;
      }
      t = inner;
    }
    return t;
  }

  @Override
  public String toString() {
    return "ScheduledRuns[" + (closed ? "closed" : "open") + "]";
  }
}
