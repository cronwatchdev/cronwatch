package dev.cronwatch;

import static org.junit.jupiter.api.Assertions.assertEquals;

import dev.cronwatch.json.JsObject;
import dev.cronwatch.store.MemoryStore;
import java.util.ArrayList;
import java.util.HashSet;
import java.util.List;
import java.util.Objects;
import java.util.Set;
import java.util.concurrent.atomic.AtomicLong;
import org.jspecify.annotations.Nullable;
import org.junit.jupiter.api.Test;

/**
 * {@code conformance/client.json}'s {@code outbox}: per-channel retry and the trimmed held copy,
 * through a real client on the memory store, with fake channels that refuse while their key is
 * down. Each step's sends, the stored {@code undelivered} and {@code sending}, {@code lastAlertAt},
 * and the errors reported are the SDK's.
 */
class OutboxConformanceTest {
  /** One scenario's world: the store, the clock, the channels down, and what was seen. */
  private static final class World {
    final MemoryStore store = new MemoryStore();
    final AtomicLong clock = new AtomicLong();
    final Set<String> down = new HashSet<>();
    final List<String> errors = new ArrayList<>();
    List<List<JsObject>> seen = new ArrayList<>();
    @Nullable Cronwatch cw;

    void configure(List<String> names) {
      if (cw != null) {
        cw.close();
      }
      List<String> keys = dev.cronwatch.internal.evaluate.Evaluate.channelKeys(names);
      List<List<JsObject>> lists = new ArrayList<>();
      Cronwatch.Builder b =
          Cronwatch.builder()
              .store(store)
              .clock(clock::get)
              .noCronSecret()
              .noShutdownHook()
              .alerts(List.of())
              .onError(
                  (where, e) -> {
                    synchronized (errors) {
                      errors.add(where);
                    }
                  });
      for (int i = 0; i < names.size(); i++) {
        String key = keys.get(i);
        List<JsObject> mine = new ArrayList<>();
        lists.add(mine);
        b.alert(
            Channel.of(
                names.get(i),
                (alert, context) -> {
                  if (alert.toValue().has("owed")) {
                    // A channel is never given the list of channels that owe the alert.
                    synchronized (errors) {
                      errors.add("owed reached " + key);
                    }
                  }
                  boolean accepted;
                  synchronized (down) {
                    accepted = !down.contains(key);
                  }
                  Run run = alert.run();
                  JsObject sent =
                      new JsObject()
                          .set("channel", key)
                          .set("type", alert.type().value())
                          .set("run", run == null ? null : run.id())
                          .set("accepted", accepted)
                          .set(
                              "errorLength",
                              run == null || run.error() == null ? null : run.error().length())
                          .set(
                              "outputLength",
                              run == null || run.output() == null ? null : run.output().length());
                  synchronized (mine) {
                    mine.add(sent);
                  }
                  if (!accepted) {
                    throw new IllegalStateException(key + " is down");
                  }
                }));
      }
      seen = lists;
      cw = b.build();
    }

    Cronwatch cw() {
      return Objects.requireNonNull(cw);
    }
  }

  private static List<String> strings(JsObject o, String key) {
    List<String> out = new ArrayList<>();
    for (Object x : Fixtures.list(o, key)) {
      out.add((String) x);
    }
    return out;
  }

  private static List<Object> held(@Nullable List<Alert> alerts) {
    List<Object> out = new ArrayList<>();
    if (alerts == null) {
      return out;
    }
    for (Alert a : alerts) {
      JsObject v = a.toValue();
      JsObject o =
          new JsObject()
              .set("type", v.get("type"))
              .set("run", v.get("run") instanceof JsObject r ? r.get("id") : null);
      if (v.has("owed")) {
        o.set("owed", v.get("owed"));
      }
      out.add(o);
    }
    return out;
  }

  @Test
  void perChannelRetryIsTheSdks() throws Exception {
    Fixtures.Failures failures = new Fixtures.Failures();
    int steps = 0;
    for (JsObject scenario : Fixtures.objects(Fixtures.load("client"), "outbox")) {
      String name = Fixtures.string(scenario, "name");
      World w = new World();
      w.configure(strings(scenario, "channels"));
      try {
        int i = 0;
        for (JsObject step : Fixtures.objects(scenario, "steps")) {
          steps++;
          String op = Objects.requireNonNull(Fixtures.string(step, "op"));
          String what = name + ": step " + i++ + " (" + op + ")";
          for (List<JsObject> list : w.seen) {
            synchronized (list) {
              list.clear();
            }
          }
          synchronized (w.errors) {
            w.errors.clear();
          }
          switch (op) {
            case "fail", "ok" -> {
              long at = Fixtures.integer(step, "at");
              w.clock.set(at);
              RunHandle handle =
                  w.cw()
                      .job("nightly")
                      .start(StartOptions.id(Objects.requireNonNull(Fixtures.string(step, "id"))));
              w.clock.set(at + 1000);
              if (op.equals("ok")) {
                handle.finish("done");
              } else {
                handle.fail(step.has("error") ? Fixtures.expand(step.get("error")) : "boom");
              }
            }
            case "check" -> {
              w.clock.set(Fixtures.integer(step, "at"));
              w.cw().check();
            }
            case "down" -> {
              synchronized (w.down) {
                w.down.addAll(strings(step, "channels"));
              }
            }
            case "up" -> {
              synchronized (w.down) {
                w.down.removeAll(strings(step, "channels"));
              }
            }
            case "configure" -> w.configure(strings(step, "channels"));
            case "legacy" -> {
              JsObject state = Objects.requireNonNull(w.store.getState("nightly")).toValue();
              if (state.get("undelivered") instanceof List<?> list) {
                for (Object entry : list) {
                  ((JsObject) entry).remove("owed");
                }
              }
              w.store.setState(JobState.fromValue(state));
            }
            default -> throw new IllegalStateException("unknown op " + op);
          }
          List<Object> sent = new ArrayList<>();
          for (List<JsObject> list : w.seen) {
            synchronized (list) {
              sent.addAll(list);
            }
          }
          JobState state = w.store.getState("nightly");
          List<Alert> sending = new ArrayList<>();
          if (state != null && state.sending() != null) {
            for (SendingAlert s : state.sending()) {
              if (s.alert() != null) {
                sending.add(s.alert());
              }
            }
          }
          List<String> errors;
          synchronized (w.errors) {
            errors = List.copyOf(w.errors);
          }
          JsObject got =
              new JsObject()
                  .set("sent", sent)
                  .set("undelivered", held(state == null ? null : state.undelivered()))
                  .set("sending", held(sending))
                  .set("lastAlertAt", state == null ? null : state.lastAlertAt())
                  .set("errors", errors);
          failures.same(what, got, step.get("expect"));
        }
      } finally {
        w.cw().close();
      }
    }
    assertEquals(41, steps, "outbox steps");
    failures.check("client outbox");
  }
}
