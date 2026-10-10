package dev.cronwatch;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;

import dev.cronwatch.json.JsObject;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import org.jspecify.annotations.Nullable;
import org.junit.jupiter.api.Test;

/**
 * The environment as every port reads it: {@code CRONWATCH_ENV}, then {@code APP_ENV}, then the
 * port's own (here the Spring starter's profiles, given as the fallback), the first not blank once
 * trimmed, lowercased, with the aliases ({@code test} and {@code testing} are not development). The
 * SDK's table ({@code packages/sdk/test/env.test.ts}), and {@code client.json}'s cases.
 */
class EnvTest {
  private static String read(
      @Nullable String cronwatchEnv, @Nullable String appEnv, @Nullable String own) {
    Map<String, String> vars = new HashMap<>();
    if (cronwatchEnv != null) {
      vars.put("CRONWATCH_ENV", cronwatchEnv);
    }
    if (appEnv != null) {
      vars.put("APP_ENV", appEnv);
    }
    return Env.environment(vars::get, own);
  }

  @Test
  void theSdksTable() {
    assertEquals("", read(null, null, null));
    assertEquals("development", read(null, null, "development"));
    assertEquals("test", read(null, null, "test"));
    assertEquals("production", read(null, null, "production"));
    assertEquals("development", read(null, "local", "production"));
    assertEquals("production", read("production", "dev", "development"));
    assertEquals("staging", read("staging", null, "development"));
    assertEquals("production", read("  PROD ", null, null));
    assertEquals("testing", read(null, "Testing", null));
    assertEquals("development", read(null, "DEV", null));
    assertEquals("production", read("", "   ", "production"));
    assertEquals("", read(" \t", null, null));
  }

  /** {@code client.json}'s {@code environment}: the port's own variable is the fallback here. */
  @Test
  void everyCaseOfClientJsonsEnvironment() {
    List<JsObject> cases = Fixtures.objects(Fixtures.load("client"), "environment");
    assertTrue(!cases.isEmpty());
    for (JsObject c : cases) {
      String got =
          read(
              (String) c.get("CRONWATCH_ENV"), (String) c.get("APP_ENV"), (String) c.get("native"));
      String what = c.toJson();
      assertEquals(c.get("environment"), got, what);
      assertEquals(c.get("development"), got.equals("development"), what);
      assertEquals(c.get("production"), got.equals("production"), what);
    }
  }

  /** {@code client.json}'s {@code secrets}: trimmed as JavaScript trims, and blank is unset. */
  @Test
  void everyCaseOfClientJsonsSecrets() {
    List<JsObject> cases = Fixtures.objects(Fixtures.load("client"), "secrets");
    assertTrue(!cases.isEmpty());
    for (JsObject c : cases) {
      assertEquals(c.get("secret"), Env.secret((String) c.get("value")), c.toJson());
    }
  }

  @Test
  void spacesAreTrimmedAsJavaScriptTrimsThem() {
    // A no-break space and a byte order mark are spaces to JavaScript's trim, not to strip().
    assertEquals("production", read(" prod﻿", null, null));
  }
}
