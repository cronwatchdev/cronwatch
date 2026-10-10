package dev.cronwatch.spring;

import dev.cronwatch.Cronwatch;
import dev.cronwatch.quartz.CronwatchQuartz;
import dev.cronwatch.quartz.QuartzOptions;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.CopyOnWriteArrayList;
import org.quartz.Scheduler;
import org.quartz.SchedulerException;
import org.springframework.beans.factory.DisposableBean;
import org.springframework.beans.factory.ObjectProvider;
import org.springframework.beans.factory.SmartInitializingSingleton;

/**
 * The app's Quartz schedulers watched with {@link CronwatchQuartz}, once every bean is made and
 * before the schedulers start (Spring's {@code SchedulerFactoryBean} starts its scheduler when the
 * context starts); with the check run as {@code CronwatchCheckJob} when the cluster should run it
 * through Quartz. Stopped when the context closes.
 */
final class CronwatchQuartzRegistrar
    implements SmartInitializingSingleton, DisposableBean, CronwatchChecker.QuartzChecks {
  private final Cronwatch cw;
  private final CronwatchProperties properties;
  private final String app;
  private final ObjectProvider<Scheduler> schedulers;
  private final ObjectProvider<CronwatchChecker.ClusterLock> clusterLock;
  private final List<CronwatchQuartz> watched = new CopyOnWriteArrayList<>();
  private volatile boolean runsTheCheck;

  CronwatchQuartzRegistrar(
      Cronwatch cw,
      CronwatchProperties properties,
      String app,
      ObjectProvider<Scheduler> schedulers,
      ObjectProvider<CronwatchChecker.ClusterLock> clusterLock) {
    this.cw = cw;
    this.properties = properties;
    this.app = app;
    this.schedulers = schedulers;
    this.clusterLock = clusterLock;
  }

  @Override
  public void afterSingletonsInstantiated() {
    List<Scheduler> all = schedulers.orderedStream().toList();
    CronwatchProperties.CheckMode mode = properties.getCheckMode();
    boolean quartzCheck =
        mode == CronwatchProperties.CheckMode.QUARTZ
            || (mode == CronwatchProperties.CheckMode.AUTO
                && clusterLock.getIfAvailable() == null
                && clustered(all));
    for (Scheduler s : all) {
      try {
        watched.add(CronwatchQuartz.watch(cw, s, QuartzOptions.defaults().app(app)));
      } catch (SchedulerException e) {
        cw.reportError(e, "quartz");
      }
    }
    if (!quartzCheck) {
      return;
    }
    // The check job on one scheduler only, a clustered one first (each clustered scheduler would
    // run it once per cluster, and one that is not, once per instance); the next if one refuses.
    // When none takes it, runsTheCheck stays false and the starter checks in each instance.
    List<Scheduler> order = new ArrayList<>();
    for (Scheduler s : all) {
      if (isClustered(s)) {
        order.add(s);
      }
    }
    for (Scheduler s : all) {
      if (!order.contains(s)) {
        order.add(s);
      }
    }
    for (Scheduler s : order) {
      try {
        CronwatchQuartz.scheduleCheck(s, CronwatchChecker.interval(properties.getCheckEvery()));
        runsTheCheck = true;
        return;
      } catch (SchedulerException | RuntimeException e) {
        cw.reportError(e, "quartz");
      }
    }
  }

  private static boolean isClustered(Scheduler s) {
    try {
      return s.getMetaData().isJobStoreClustered();
    } catch (SchedulerException e) {
      return false;
    }
  }

  private static boolean clustered(List<Scheduler> all) {
    for (Scheduler s : all) {
      // One the app cannot ask is not counted.
      if (isClustered(s)) {
        return true;
      }
    }
    return false;
  }

  @Override
  public boolean clustered() {
    return clustered(schedulers.orderedStream().toList());
  }

  @Override
  public boolean runsTheCheck() {
    return runsTheCheck;
  }

  @Override
  public void sync() {
    for (CronwatchQuartz q : new ArrayList<>(watched)) {
      q.sync();
    }
  }

  @Override
  public void destroy() {
    for (CronwatchQuartz q : watched) {
      q.close();
    }
    watched.clear();
  }

  @Override
  public String toString() {
    return "CronwatchQuartzRegistrar[" + watched.size() + " schedulers]";
  }
}
