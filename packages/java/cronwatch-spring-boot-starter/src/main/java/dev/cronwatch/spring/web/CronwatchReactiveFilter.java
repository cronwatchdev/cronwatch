package dev.cronwatch.spring.web;

import dev.cronwatch.web.Adapters;
import dev.cronwatch.web.Endpoint;
import dev.cronwatch.web.Request;
import dev.cronwatch.web.Response;
import java.io.IOException;
import java.net.URI;
import java.util.Map;
import java.util.Objects;
import java.util.concurrent.Executor;
import org.springframework.core.Ordered;
import org.springframework.core.io.buffer.DataBuffer;
import org.springframework.core.io.buffer.DataBufferLimitException;
import org.springframework.core.io.buffer.DataBufferUtils;
import org.springframework.http.HttpStatusCode;
import org.springframework.http.server.reactive.ServerHttpRequest;
import org.springframework.http.server.reactive.ServerHttpResponse;
import org.springframework.web.server.ServerWebExchange;
import org.springframework.web.server.WebFilter;
import org.springframework.web.server.WebFilterChain;
import reactor.core.publisher.Mono;
import reactor.core.scheduler.Scheduler;
import reactor.core.scheduler.Schedulers;

/**
 * The dashboard on a Spring WebFlux app: a {@link WebFilter} that answers the requests under its
 * path and passes the rest down the chain. The routes read the store, and a Netty event loop must
 * never block, so each request is answered on a virtual thread of its own; the body is joined there
 * only when a route wants it, to at most 1 MiB and one byte. A request whose client goes away is
 * cancelled and reports nothing.
 */
public final class CronwatchReactiveFilter implements WebFilter, Ordered {
  private static final Executor VIRTUAL =
      task -> Thread.ofVirtual().name("cronwatch-web").start(task);

  private final Endpoint endpoint;
  private final String path;
  private final int order;
  private final Scheduler scheduler = Schedulers.fromExecutor(VIRTUAL);

  /** {@code endpoint} at {@code path} within the context, at this filter order. */
  public CronwatchReactiveFilter(Endpoint endpoint, String path, int order) {
    this.endpoint = Objects.requireNonNull(endpoint, "endpoint");
    String p = Objects.requireNonNull(path, "path");
    while (p.endsWith("/")) {
      p = p.substring(0, p.length() - 1);
    }
    this.path = p;
    this.order = order;
  }

  @Override
  public int getOrder() {
    return order;
  }

  @Override
  public Mono<Void> filter(ServerWebExchange exchange, WebFilterChain chain) {
    ServerHttpRequest req = exchange.getRequest();
    String context = req.getPath().contextPath().value();
    URI uri = req.getURI();
    String raw = uri.getRawPath() == null ? "/" : uri.getRawPath();
    // The path as the routes read it, dot segments resolved: /cronwatch/../api/jobs is not ours.
    if (!path.isEmpty() && !Adapters.under(raw, context + path)) {
      return chain.filter(exchange);
    }
    Request.Builder b =
        Request.builder(req.getMethod().name(), Adapters.target(raw, uri.getRawQuery()))
            .tls(tls(req))
            .mount(context + path);
    req.getHeaders()
        .forEach(
            (name, values) -> {
              for (String v : values) {
                b.header(name, v);
              }
            });
    b.body(req.getHeaders().getContentLength(), limit -> read(req, limit));
    Request request = b.build();
    return Mono.fromCallable(() -> endpoint.handle(request))
        .subscribeOn(scheduler)
        .flatMap(answer -> write(exchange.getResponse(), answer));
  }

  /**
   * Whether the request came over TLS: the request URI's scheme, which Spring's forwarded-header
   * handling ({@code server.forward-headers-strategy}) sets from a TLS-terminating proxy's headers,
   * as the servlet filter reads {@code isSecure()}. A connection's own TLS is not enough: behind
   * such a proxy it has none, and Spring's transformer removes the headers {@code
   * cronwatch.web.trust-proxy} would read.
   */
  static boolean tls(ServerHttpRequest req) {
    return "https".equalsIgnoreCase(req.getURI().getScheme()) || req.getSslInfo() != null;
  }

  /** The body, joined to at most {@code limit} and one byte, on the virtual thread asking. */
  private static byte[] read(ServerHttpRequest req, int limit) throws IOException {
    try {
      byte[] body =
          DataBufferUtils.join(req.getBody(), limit + 1)
              .map(
                  buffer -> {
                    try {
                      byte[] bytes = new byte[buffer.readableByteCount()];
                      buffer.read(bytes);
                      return bytes;
                    } finally {
                      DataBufferUtils.release(buffer);
                    }
                  })
              .block();
      return body == null ? new byte[0] : body;
    } catch (DataBufferLimitException e) {
      throw new Request.BodyTooLargeException();
    } catch (RuntimeException e) {
      // The client went away or the body could not be read to its end: none, never the part.
      throw new IOException("the request body could not be read", e);
    }
  }

  private static Mono<Void> write(ServerHttpResponse res, Response answer) {
    res.setStatusCode(HttpStatusCode.valueOf(answer.status()));
    for (Map.Entry<String, String> h : answer.headers()) {
      res.getHeaders().add(h.getKey(), h.getValue());
    }
    // The whole body is at hand: its length, so no server frames it as chunked (Netty frames an
    // empty answer to a HEAD so when it is given none).
    res.getHeaders().setContentLength(answer.bodyLength());
    if (answer.bodyLength() == 0) {
      return res.setComplete();
    }
    DataBuffer buffer = res.bufferFactory().wrap(answer.body());
    return res.writeWith(Mono.just(buffer));
  }

  @Override
  public String toString() {
    return "CronwatchReactiveFilter[path=" + path + ", endpoint=" + endpoint + "]";
  }
}
