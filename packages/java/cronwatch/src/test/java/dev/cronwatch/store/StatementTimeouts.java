package dev.cronwatch.store;

import java.lang.reflect.InvocationTargetException;
import java.lang.reflect.Method;
import java.lang.reflect.Proxy;
import java.sql.Connection;
import java.sql.PreparedStatement;
import java.sql.Statement;
import java.util.List;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.function.BiConsumer;
import java.util.function.Function;
import javax.sql.DataSource;

/**
 * A data source that notes, for every statement run through it, the query timeout it was given
 * ({@code 0} for none), as {@code execute=60}.
 */
final class StatementTimeouts {
  private StatementTimeouts() {}

  /** The data source to give the store, and what its statements were given. */
  record Watched(DataSource dataSource, List<String> seen) {}

  static Watched watch(DataSource real) {
    List<String> seen = new CopyOnWriteArrayList<>();
    Function<Object, Object> statements =
        made -> {
          if (made instanceof PreparedStatement ps) {
            return noted(PreparedStatement.class, ps, seen);
          }
          if (made instanceof Statement s) {
            return noted(Statement.class, s, seen);
          }
          return made;
        };
    DataSource ds =
        proxied(
            DataSource.class,
            real,
            (m, a) -> {},
            made ->
                made instanceof Connection c
                    ? proxied(Connection.class, c, (m, a) -> {}, statements)
                    : made);
    return new Watched(ds, seen);
  }

  private static <T> T noted(Class<T> type, T statement, List<String> seen) {
    int[] timeout = {0};
    return proxied(
        type,
        statement,
        (m, a) -> {
          if (m.getName().equals("setQueryTimeout")) {
            timeout[0] = (Integer) a[0];
          } else if (m.getName().startsWith("execute")) {
            seen.add(m.getName() + "=" + timeout[0]);
          }
        },
        r -> r);
  }

  private static <T> T proxied(
      Class<T> type,
      Object target,
      BiConsumer<Method, Object[]> calls,
      Function<Object, Object> wrap) {
    return type.cast(
        Proxy.newProxyInstance(
            type.getClassLoader(),
            new Class<?>[] {type},
            (proxy, method, args) -> {
              calls.accept(method, args);
              try {
                return wrap.apply(method.invoke(target, args));
              } catch (InvocationTargetException e) {
                throw e.getCause();
              }
            }));
  }
}
