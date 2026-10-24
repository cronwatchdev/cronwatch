package dev.cronwatch.jobrunr.mdc;

import org.slf4j.ILoggerFactory;
import org.slf4j.IMarkerFactory;
import org.slf4j.helpers.BasicMDCAdapter;
import org.slf4j.helpers.BasicMarkerFactory;
import org.slf4j.helpers.NOPLoggerFactory;
import org.slf4j.spi.MDCAdapter;
import org.slf4j.spi.SLF4JServiceProvider;

/**
 * An SLF4J provider for the tests that logs nothing but keeps the MDC, so a test can see the keys a
 * run sets on a worker's thread (SLF4J without a provider keeps none).
 */
public final class MdcProvider implements SLF4JServiceProvider {
  private final ILoggerFactory loggers = new NOPLoggerFactory();
  private final IMarkerFactory markers = new BasicMarkerFactory();
  private final MDCAdapter mdc = new BasicMDCAdapter();

  /** Made by SLF4J's service loader. */
  public MdcProvider() {}

  @Override
  public ILoggerFactory getLoggerFactory() {
    return loggers;
  }

  @Override
  public IMarkerFactory getMarkerFactory() {
    return markers;
  }

  @Override
  public MDCAdapter getMDCAdapter() {
    return mdc;
  }

  @Override
  public String getRequestedApiVersion() {
    return "2.0.99";
  }

  @Override
  public void initialize() {}
}
