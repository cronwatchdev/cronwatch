package dev.cronwatch.spring;

import java.util.Locale;
import org.springframework.boot.autoconfigure.condition.ConditionOutcome;
import org.springframework.boot.autoconfigure.condition.SpringBootCondition;
import org.springframework.context.annotation.ConditionContext;
import org.springframework.core.type.AnnotatedTypeMetadata;

/**
 * Matches unless {@code cronwatch.enabled} is {@code false}, as
 * {@code @ConditionalOnProperty(prefix = "cronwatch", name = "enabled", matchIfMissing = true)}
 * reads it. Every auto-configuration of the starter carries it beside its own switch, so {@code
 * cronwatch.enabled=false} turns the whole starter off, even when the app has a client of its own.
 * Public only so the web package can use it; not for apps.
 */
public final class OnCronwatchEnabled extends SpringBootCondition {
  /** Made by Spring. */
  public OnCronwatchEnabled() {}

  @Override
  public ConditionOutcome getMatchOutcome(
      ConditionContext context, AnnotatedTypeMetadata metadata) {
    String value = context.getEnvironment().getProperty("cronwatch.enabled");
    if (value != null && value.toLowerCase(Locale.ROOT).equals("false")) {
      return ConditionOutcome.noMatch("cronwatch.enabled is false");
    }
    return ConditionOutcome.match("cronwatch.enabled is not false");
  }
}
