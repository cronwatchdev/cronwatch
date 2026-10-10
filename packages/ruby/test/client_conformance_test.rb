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
end
