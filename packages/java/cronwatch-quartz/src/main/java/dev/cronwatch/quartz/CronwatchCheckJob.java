package dev.cronwatch.quartz;

import org.quartz.DisallowConcurrentExecution;
import org.quartz.Job;
import org.quartz.JobExecutionContext;
import org.quartz.JobExecutionException;
import org.quartz.SchedulerException;

/**
 * CronWatch's check as a Quartz job, scheduled with {@link CronwatchQuartz#scheduleCheck}: a sync
 * (the scheduler's jobs declared again, the declarations written, and jobs gone from it declared
 * again without their schedule, within 30 seconds) and a check, once per firing across a cluster.
 * Its runs are never a job. On a node that does not watch the scheduler it fails, so Quartz's own
 * record shows the check did not run: watch the scheduler on every node that can fire it.
 */
@DisallowConcurrentExecution
public final class CronwatchCheckJob implements Job {
  /** What a node with no live watch fails the check with. */
  static final String NO_WATCH =
      "cronwatch: this node has no live CronWatch watch of this scheduler, so the check did not"
          + " run; call CronwatchQuartz.watch(cw, scheduler) on every node that runs the check";

  /** Made by Quartz's job factory. */
  public CronwatchCheckJob() {}

  @Override
  public void execute(JobExecutionContext context) throws JobExecutionException {
    Object found;
    try {
      found = context.getScheduler().getContext().get(CronwatchQuartz.CONTEXT_KEY);
    } catch (SchedulerException e) {
      throw new JobExecutionException(NO_WATCH + " (" + e.getMessage() + ")", e, false);
    }
    if (!(found instanceof CronwatchQuartz q)) {
      throw new JobExecutionException(NO_WATCH, false);
    }
    q.checkNow();
  }
}
