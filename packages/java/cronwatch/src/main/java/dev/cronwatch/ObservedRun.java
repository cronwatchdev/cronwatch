package dev.cronwatch;

import org.jspecify.annotations.Nullable;

/**
 * A run whose function is seen from outside, as a scheduler's listener sees one: opened when the
 * scheduler says the function starts ({@link Job#open(RunOptions)}) and closed when it says the
 * function has ended. The two halves of {@link Job#run}, so the running row, {@link
 * Cronwatch#current()}, the MDC keys, the timeout, and the shutdown hook's record are the same as a
 * run's. Open it in the thread the function runs in; close it there too, so the current run and the
 * MDC keys are put back as they were (closed from another thread, it is recorded all the same).
 *
 * <p>Safe to use from any thread; the first of {@link #close} and {@link #takeBack} ends it, and
 * later calls do nothing.
 */
public final class ObservedRun {
  private final Runs runs;
  private final Runs.Opened opened;

  ObservedRun(Runs runs, Runs.Opened opened) {
    this.runs = runs;
    this.opened = opened;
  }

  /** The run's context, what a job's function is given. */
  public JobContext context() {
    return opened.context;
  }

  /** The run's id. */
  public String id() {
    return opened.run.id();
  }

  /** The job's name. */
  public String job() {
    return opened.def.name();
  }

  /** Whether the run is still open. */
  public boolean isOpen() {
    return !opened.isClosed();
  }

  /**
   * Clears the calling thread's current run ({@link Cronwatch#current()}), and the MDC keys that
   * name it, when that run's function has ended. A run closed from another thread than the one that
   * opened it leaves both in the opening thread, which alone can put them back; a scheduler's
   * listener whose attempts may end without a word (JobRunr's after an {@code Error}) calls this as
   * each attempt starts on a pooled thread, so a job that is not watched does not log under a run
   * that has ended. Opening a run does it too. Does nothing when the current run is still going.
   */
  public static void clearEnded() {
    Runs.clearEnded();
  }

  /**
   * Ends the part of the run in which the function runs: the current run and the MDC keys are put
   * back as they were, in the thread that opened it, and the timeout no longer applies. For a
   * listener that learns how the run went only later; {@link #close} does it anyway.
   */
  public void endFunction() {
    runs.endFunction(opened);
  }

  /**
   * Closes the run, failed with {@code failure} when it is not null and succeeded otherwise, and
   * judges and records it, waiting for the store as a run does. Returns the run as recorded. A
   * failure the run's {@link RunOptions#withDiscardWhen} answers true for takes the run back
   * instead. Never throws.
   */
  public Run close(@Nullable Throwable failure) {
    return runs.close(opened, null, failure);
  }

  /**
   * Closes the run as a success with {@code value}, what the function returned: a {@code String} is
   * the output when nothing was logged, and an HTTP answer of 400 or more fails the run.
   */
  public Run closeWith(@Nullable Object value) {
    return runs.close(opened, value, null);
  }

  /**
   * Gives the run back rather than judge it, for an attempt the scheduler did not really make (a
   * lock another instance held): its running row is deleted, nothing is judged or alerted, and the
   * job's state is left as it was. A row a check already marked stuck is left as it is. When the
   * store cannot take a run back (it has no {@code deleteRunIf}, or it failed), which is reported,
   * the run is closed failed instead, saying it could not be given back: never as a success, which
   * would send a recovery for an attempt that did not finish. Says whether it was given back.
   */
  public boolean takeBack() {
    return takeBack(null);
  }

  /**
   * {@link #takeBack()} for an attempt that ended in {@code thrown} (a scheduler's word that it was
   * deleted, or never ran): when the run cannot be given back, it is closed failed with {@code
   * thrown}, or, when that is null, with an error saying it could not be given back.
   */
  public boolean takeBack(@Nullable Throwable thrown) {
    if (runs.takeBack(opened)) {
      return true;
    }
    runs.close(
        opened,
        null,
        thrown != null
            ? thrown
            : new CronwatchException(
                CronwatchException.Kind.OTHER,
                "the attempt was to be given back, but the store could not take it back"));
    return false;
  }

  @Override
  public String toString() {
    return "ObservedRun[" + opened.def.name() + ", run " + opened.run.id() + "]";
  }
}
