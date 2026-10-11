# frozen_string_literal: true

require "net/http"
require "timeout"
require "uri"

module Cronwatch
  # POSTs for the alert channels. Anything with `post(url, body, headers)`
  # returning a Response can stand in for it, which is how the tests run.
  #
  # @api private
  module HTTP
    TIMEOUT = 10
    # The most of an answer's body read (64 KiB, the SDK's ERROR_READ_MAX),
    # far more than any secret plus the 200 characters of a refusal a channel
    # keeps: a hostile or broken endpoint could otherwise stream gigabytes
    # into the worker within the deadline. Reading stops there. A 2xx
    # answer's body is not read at all.
    MAX_BODY = 64 * 1024

    Response = Struct.new(:status, :body, keyword_init: true) do
      def ok? = status >= 200 && status < 300
    end

    # The request took longer than its deadline. fetch's message for it.
    class TimeoutError < ::Timeout::Error
      def initialize(message = "The operation was aborted due to timeout")
        super
      end
    end

    class NetHTTP
      # The deadline is the whole request's, as fetch's
      # AbortSignal.timeout(10_000) is; `timeout` is for tests.
      def initialize(timeout: TIMEOUT)
        @timeout = timeout
      end

      # Header values are sent with the whitespace around them trimmed, as
      # fetch trims them, so a credential read with a trailing newline still
      # sends. Past the deadline before an answer, raises HTTP::TimeoutError;
      # past it while the body is still arriving, returns the answer with
      # what of the body had come, as the SDK's errorText keeps it.
      # A 2xx answer comes back with an empty body, unread, since no channel
      # needs it; any other with at most MAX_BODY bytes of it.
      def post(url, body, headers)
        uri = HTTP.postable(url)
        deadline = HTTP.monotonic + @timeout
        request = Net::HTTP::Post.new(uri.request_uri)
        headers.each { |k, v| request[k] = HTTP.trim_header(v) }
        request.body = body
        http = connection(uri)
        # Net::HTTP opens the socket under Timeout.timeout(open_timeout), or
        # TCPSocket's own open_timeout where Ruby has one, and both cover the
        # name lookup: it can be interrupted from Ruby 3.3. On Ruby 3.2 a lookup
        # that hangs is cut off only by the client's channel deadline.
        http.open_timeout = HTTP.remaining(deadline)
        http.start do |conn|
          remaining = HTTP.remaining(deadline)
          conn.read_timeout = remaining
          conn.write_timeout = remaining
          conn.request(request) do |response|
            status = response.code.to_i
            # Returning from inside the block leaves the rest unread; the connection closes after.
            return Response.new(status: status, body: "") if status >= 200 && status < 300

            return Response.new(status: status, body: read_body(conn, response, deadline))
          end
        end
      rescue Net::OpenTimeout, Net::ReadTimeout, Net::WriteTimeout
        raise TimeoutError
      end

      # A connection that gives up after ten seconds, as the SDK's requests do.
      def connection(uri)
        http = Net::HTTP.new(uri.host, uri.port)
        http.use_ssl = uri.scheme == "https"
        http.open_timeout = @timeout
        http.read_timeout = @timeout
        http.write_timeout = @timeout
        http
      end

      private

      # The body, read in chunks until it ends or MAX_BODY bytes have come,
      # whichever is first; what had come when the deadline passes first.
      def read_body(conn, response, deadline)
        body = "".b
        conn.read_timeout = HTTP.remaining(deadline)
        response.read_body do |chunk|
          raise TimeoutError if HTTP.monotonic >= deadline

          body << chunk.b.byteslice(0, MAX_BODY - body.bytesize)
          break if body.bytesize >= MAX_BODY

          conn.read_timeout = HTTP.remaining(deadline)
        end
        body
      rescue Net::ReadTimeout, TimeoutError
        body
      end
    end

    module_function

    def default
      @default ||= NetHTTP.new
    end

    def monotonic
      Process.clock_gettime(Process::CLOCK_MONOTONIC)
    end

    # Seconds left before `deadline`, never quite zero (Net::HTTP reads 0 as no wait).
    def remaining(deadline)
      [deadline - monotonic, 0.001].max
    end

    # A URL as fetch reads it: the spaces and control characters around it,
    # and any tab or line break inside it, dropped (a pasted webhook URL often
    # ends in a newline).
    def clean_url(url)
      url.to_s.gsub(/\A[\u0000-\u0020]+|[\u0000-\u0020]+\z/, "").delete("\t\n\r")
    end

    # The URL parsed, once it is one a channel can post to. Refused without
    # quoting it: URI's own errors quote the whole URL, and a webhook URL's
    # path is its credential.
    def postable(url)
      uri = begin
        URI(clean_url(url))
      rescue URI::Error, ArgumentError
        nil
      end
      scheme = uri&.scheme&.downcase
      unless %w[http https].include?(scheme) && uri.host && !uri.host.empty?
        shown = uri&.scheme && uri.host ? "#{scheme}:" : "this URL"
        raise ArgumentError, "only http and https URLs can be posted to, not #{shown}"
      end
      uri
    end

    # A header value without the spaces, tabs, and line breaks around it, as fetch sends it.
    def trim_header(value)
      value.to_s.gsub(/\A[ \t\r\n]+|[ \t\r\n]+\z/, "")
    end

    # Compares what a request `sent` with the `secret` without stopping at the
    # first differing byte, and in time that depends only on the length of
    # what was sent, so neither the secret's bytes nor its length can be
    # timed. Every byte of `sent` is compared, against the secret's byte at
    # the same place (repeating it past its end), and a length that differs
    # counts as a difference. The SDK's constantTimeEqual, over bytes.
    def constant_time_equal?(sent, secret)
      expected = secret.b
      return sent.bytesize.zero? if expected.empty?

      diff = sent.bytesize ^ expected.bytesize
      size = expected.bytesize
      sent.b.each_byte.with_index { |byte, i| diff |= byte ^ expected.getbyte(i % size) }
      diff.zero?
    end
  end
end
