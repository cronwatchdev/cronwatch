# frozen_string_literal: true

require "openssl"

module Cronwatch
  module Alerts
    # POSTs the alert as JSON to any URL. The body is `"schema": 1` and then
    # the alert's JSON: { schema, type, run, details, job, definition, title,
    # message, at, triage }, described by https://cronwatch.dev/schemas/webhook/1.json.
    # With a secret, each request is signed with it (HMAC-SHA256, as
    # lowercase hex; Webhook.signature), so the receiver can verify it:
    #
    # - `X-CronWatch-Signature: sha256=<hex>` signs the raw body alone, as it
    #   always has.
    # - `X-CronWatch-Timestamp: <unix seconds>` is when it was sent, and
    #   `X-CronWatch-Timestamp-Signature: sha256=<hex>` signs
    #   `<timestamp>.<raw body>`, so a receiver that checks this one (in
    #   constant time) and refuses a timestamp more than five minutes from its
    #   own clock cannot be sent a captured alert again later.
    #
    # Header values are trimmed, and a name is matched without regard to
    # case, so a Content-Type given in `headers` replaces the default one
    # rather than going beside it; the signature headers are always the
    # channel's own. A redirect is an error, not followed (the headers and the
    # signature would go with it): point the url at where the receiver really
    # is. `now` is the clock the timestamp is read from, a callable returning
    # epoch milliseconds. For tests.
    class Webhook
      attr_reader :name

      def initialize(url:, headers: {}, secret: nil, now: nil, http: HTTP.default)
        raise ArgumentError, "Cronwatch::Alerts::Webhook needs a url" if url.nil? || url.to_s.empty?

        @name = "webhook"
        @url = url
        @headers = headers || {}
        @secret = secret
        @now = now || -> { Process.clock_gettime(Process::CLOCK_REALTIME, :millisecond) }
        @http = http
      end

      # The version of the body's shape, its first key. It goes up only in a
      # major release; fields are added within one.
      SCHEMA = 1
      private_constant :SCHEMA

      # The HMAC-SHA256 of `body` with `secret`, as lowercase hex, without the
      # `sha256=` the header puts before it. A receiver compares it with the
      # header in constant time (Rack::Utils.secure_compare). The timestamped
      # one is the signature of `<timestamp>.<body>`, with the
      # X-CronWatch-Timestamp value as sent.
      def self.signature(secret, body)
        OpenSSL::HMAC.hexdigest("SHA256", secret.to_s, body.to_s)
      end

      # `context` is the client's channel context: an empty-string secret (an
      # unset variable read as "", say) signs nothing, and each send reports
      # that through its on_error.
      def call(alert, context = nil)
        body = JS.json({ "schema" => SCHEMA }.merge(alert.to_h))
        if @secret == "" && context.respond_to?(:on_error)
          context.on_error(RuntimeError.new("webhook: secret is an empty string, so this request is sent unsigned; " \
                                            "set the secret, or leave it out to send unsigned on purpose"))
        end
        headers = { "content-type" => "application/json", "user-agent" => "cronwatch" }
        # A pasted Authorization value often carries a stray space or newline, which a header would refuse.
        @headers.each { |name, value| Webhook.set_header(headers, name.to_s, value.is_a?(String) ? JS.trim(value) : value) }
        if @secret && !@secret.empty?
          timestamp = @now.call.div(1000).to_s
          Webhook.set_header(headers, "x-cronwatch-signature", "sha256=#{Webhook.signature(@secret, body)}")
          Webhook.set_header(headers, "x-cronwatch-timestamp", timestamp)
          Webhook.set_header(headers, "x-cronwatch-timestamp-signature", "sha256=#{Webhook.signature(@secret, "#{timestamp}.#{body}")}")
        end
        response = @http.post(@url, body, headers)
        # Only the origin: a webhook URL's path or query often is the credential.
        raise "Webhook #{Webhook.origin(@url)} answered #{response.status}" unless response.ok?
      end

      # Sets a header, first taking out any of the same name in another case.
      def self.set_header(headers, name, value)
        lower = name.downcase
        headers.delete_if { |key, _| key.downcase == lower }
        headers[name] = value
      end

      def self.origin(url)
        uri = URI(HTTP.clean_url(url))
        raise URI::InvalidURIError unless uri.scheme && uri.host

        port = uri.port && uri.port != uri.default_port ? ":#{uri.port}" : ""
        "#{uri.scheme.downcase}://#{uri.host.downcase}#{port}"
      rescue URI::Error, ArgumentError
        "(invalid URL)"
      end
    end
  end
end
