package dev.cronwatch;

import dev.cronwatch.internal.js.Js;
import java.util.Locale;
import java.util.function.UnaryOperator;
import org.jspecify.annotations.Nullable;

/**
 * The environment, read in one place, when used. The SDK reads {@code NODE_ENV}; Java has no
 * convention of its own, so CronWatch's own variable comes first, then {@code APP_ENV}, as the Go,
 * PHP, Rust, and Elixir ports read them.
 */
final class Env {
  /** Where the client's own warnings and default error handler write. */
  static final System.Logger LOGGER = System.getLogger("dev.cronwatch");

  private static final String[] VARIABLES = {"CRONWATCH_ENV", "APP_ENV"};

  private Env() {}

  /**
   * The environment's name, lowercased, or {@code ""} when no variable names one. {@code
   * development}, {@code dev}, and {@code local} count as {@code development} and {@code prod} as
   * {@code production}. An unset environment is not development, which is the safe reading, and
   * neither are {@code test} and {@code testing}: a test run or CI needs {@code CRON_SECRET} or a
   * token, as production does.
   */
  static String environment() {
    return environment(null);
  }

  /**
   * {@link #environment()}, or {@code fallback} (read the same way) when no variable names one: the
   * Spring starter's, from the app's active profiles.
   */
  static String environment(@Nullable String fallback) {
    return environment(System::getenv, fallback);
  }

  /**
   * {@link #environment(String)} over {@code getenv}: the first of {@code CRONWATCH_ENV}, {@code
   * APP_ENV}, and {@code fallback} whose value, trimmed as JavaScript trims, is not empty.
   */
  static String environment(UnaryOperator<@Nullable String> getenv, @Nullable String fallback) {
    for (String name : VARIABLES) {
      String value = getenv.apply(name);
      if (value == null) {
        continue;
      }
      String v = normalize(value);
      if (!v.isEmpty()) {
        return v;
      }
    }
    return fallback == null ? "" : normalize(fallback);
  }

  private static String normalize(String value) {
    String v = Js.trim(value).toLowerCase(Locale.ROOT);
    return switch (v) {
      case "prod" -> "production";
      case "dev", "local" -> "development";
      default -> v;
    };
  }

  /**
   * A secret, from the environment or given in code, trimmed as JavaScript's {@code trim} trims (a
   * value read from a file often ends in a newline, which no header can carry, since HTTP strips
   * the whitespace around a header's value); null when it is null, empty, or only whitespace, so a
   * blank value counts as not set and the routes and handlers fail closed.
   */
  static @Nullable String secret(@Nullable String value) {
    if (value == null) {
      return null;
    }
    String trimmed = Js.trim(value);
    return trimmed.isEmpty() ? null : trimmed;
  }

  /** A variable's value, or null when it is unset. */
  static String read(String name) {
    return System.getenv(name);
  }
}
