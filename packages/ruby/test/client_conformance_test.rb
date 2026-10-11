# frozen_string_literal: true

require_relative "test_helper"
require_relative "support/client_fixture"

# conformance/client.json over the memory store. The ActiveRecord store
# replays unknownFields in test/active_record/store_test.rb.
class ClientConformanceTest < Minitest::Test
  def test_run_ids
    failures = ClientFixture::FIXTURE["runIds"].filter_map do |c|
      got = ClientFixture.run_id_outcome(c["method"], c["id"])
      expected = ClientFixture.expected_error(c)
      next if got == expected

      "#{c["method"]}(#{c["id"][0, 20].inspect}, #{Cronwatch::JS.length16(c["id"])}): expected #{expected.inspect}, got #{got.inspect}"
    end
    assert_empty failures
  end

  def with_env(vars)
    saved = vars.keys.to_h { |k| [k, ENV.fetch(k, nil)] }
    vars.each { |k, v| v.nil? ? ENV.delete(k) : ENV[k] = v }
    yield
  ensure
    saved.each { |k, v| v.nil? ? ENV.delete(k) : ENV[k] = v }
  end

  # The language's own variable ("native") is RAILS_ENV here, with Rails not loaded and RACK_ENV unset.
  def test_environment
    failures = ClientFixture::FIXTURE["environment"].filter_map do |c|
      env = { "CRONWATCH_ENV" => c["CRONWATCH_ENV"], "APP_ENV" => c["APP_ENV"], "RAILS_ENV" => c["native"], "RACK_ENV" => nil }
      got = with_env(env) do
        { "environment" => Cronwatch::Environment.environment.to_s, "development" => Cronwatch::Environment.development?,
          "production" => Cronwatch::Environment.production? }
      end
      expected = c.slice("environment", "development", "production")
      "#{env.inspect}: expected #{expected}, got #{got}" unless got == expected
    end
    assert_empty failures
  end

  def test_secrets
    failures = ClientFixture::FIXTURE["secrets"].flat_map do |c|
      from_env = with_env("CRON_SECRET" => c["value"]) { Cronwatch::Environment.secret("CRON_SECRET") }
      from_option = Cronwatch::Environment.secret_option(c["value"], "token")
      [["env", from_env], ["option", from_option]].filter_map do |where, got|
        "#{where} #{c["value"].inspect}: expected #{c["secret"].inspect}, got #{got.inspect}" unless got == c["secret"]
      end
    end
    assert_empty failures
  end

  def test_unknown_fields_survive_over_the_memory_store
    steps = 0
    ClientFixture.replay_unknown_fields(Cronwatch::Stores::Memory.new) do |op, expected, got|
      assert_equal expected, got, op
      steps += 1
    end
    assert_equal ClientFixture::FIXTURE["unknownFields"]["steps"].length + 1, steps
  end

  # A channel of the outbox scenarios: records what it is given, and refuses
  # while its key is down.
  class OutboxChannel
    attr_reader :name

    def initialize(name, position, key, down, log, lock)
      @name = name
      @position = position
      @key = key
      @down = down
      @log = log
      @lock = lock
    end

    def call(alert)
      accepted = !@down.include?(@key)
      run = alert.run.is_a?(Cronwatch::Run) ? alert.run : nil
      length = ->(text) { text.is_a?(String) ? Cronwatch::JS.length16(text) : nil }
      @lock.synchronize do
        @log << { "position" => @position, "channel" => @key, "type" => alert.type.to_s, "run" => run&.id, "accepted" => accepted,
                  "errorLength" => length.call(run&.error), "outputLength" => length.call(run&.output) }
      end
      raise "#{@key} down" unless accepted
    end
  end

  # conformance/client.json's outbox: which channels each send and retry
  # reaches, and what the job's state holds after each step.
  def test_outbox_retries_go_only_to_the_channels_that_owe_them
    ClientFixture::FIXTURE["outbox"].each do |scenario|
      now = 0
      store = Cronwatch::Stores::Memory.new
      down = Set.new
      log = []
      lock = Mutex.new
      errors = []
      names = scenario["channels"]
      make = lambda do
        keys = Cronwatch::Evaluate.channel_keys(names)
        channels = names.each_with_index.map { |name, i| OutboxChannel.new(name, i, keys[i], down, log, lock) }
        Cronwatch.new(store: store, now: -> { now }, cron_secret: nil, alerts: channels, on_error: ->(_e, where) { errors << where })
      end
      cw = make.call
      brief = lambda do |alert|
        out = { "type" => alert.type.to_s, "run" => alert.run.is_a?(Cronwatch::Run) ? alert.run.id : nil }
        out["owed"] = alert.owed if alert.owed?
        out
      end
      scenario["steps"].each_with_index do |step, index|
        case step["op"]
        when "fail", "ok"
          now = step["at"]
          handle = cw.job("nightly").start(id: step["id"])
          now = step["at"] + 1000
          if step["op"] == "fail"
            error = step.fetch("error", "boom")
            # Long text travels as { "parts" => [[piece, times], ...] }.
            error = error["parts"].map { |piece, times| piece * times }.join if error.is_a?(Hash)
            handle.fail(error)
          else
            handle.finish("done")
          end
        when "check"
          now = step["at"]
          cw.check
        when "down" then down.merge(step["channels"])
        when "up" then down.subtract(step["channels"])
        when "configure"
          cw.close
          names = step["channels"]
          cw = make.call
        when "legacy"
          state = store.get_state("nightly")
          state.undelivered = state.undelivered.map { |alert| Cronwatch::Evaluate.without_owed(alert) }
          store.set_state(state)
        else flunk "unknown step #{step["op"]}"
        end
        state = store.get_state("nightly")
        sent = log.each_with_index.sort_by { |entry, order| [entry["position"], order] }.map { |entry, _| entry.except("position") }
        log.clear
        got = {
          "sent" => sent,
          "undelivered" => (state&.undelivered || []).map(&brief),
          "sending" => (state&.sending || []).map { |entry| brief.call(entry["alert"]) },
          "lastAlertAt" => state&.last_alert_at,
          "errors" => errors.dup,
        }
        errors.clear
        assert_equal Cronwatch::JS.json(step["expect"]), Cronwatch::JS.json(got), "#{scenario["name"]}: step #{index} (#{step["op"]})"
      end
      cw.close
    end
  end
end
