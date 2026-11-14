# frozen_string_literal: true

require_relative "web/helpers"
require "stringio"

# Ruby only: Rails builds the dashboard again each time it reloads
# config/routes.rb (`mount Cronwatch.client.routes => "/cronwatch"`), which it
# does after any change to the app's code. The development token is the
# client's, so the app built again takes the cookie the last one set.
class WebDevelopmentTokenTest < Minitest::Test
  include WebHelpers

  def printed
    before = $stdout
    $stdout = StringIO.new
    result = yield
    [$stdout.string.lines.map(&:chomp), result]
  ensure
    $stdout = before
  end

  def development(&block)
    with_env("CRONWATCH_ENV" => nil, "APP_ENV" => nil, "RAILS_ENV" => "development", "RACK_ENV" => nil, "CRONWATCH_TOKEN" => nil, &block)
  end

  def client
    Cronwatch.new(store: Cronwatch::Stores::Memory.new, alerts: [Capture.new], cron_secret: nil)
  end

  def test_an_app_built_again_for_the_same_client_keeps_the_token_and_the_browser_signed_in
    development do
      cw = client
      first = cw.routes
      lines, = printed { send_request(first, "GET", "http://localhost:3000/cronwatch/", script_name: "/cronwatch") }
      assert_equal 1, lines.length
      token = lines[0][/token=([A-Za-z0-9_-]{43})\z/, 1]
      assert token, lines[0]
      sign_in = send_request(first, "GET", "http://localhost:3000/cronwatch/?token=#{token}", script_name: "/cronwatch")
      cookie = sign_in.headers["set-cookie"].split(";").first

      # What a reload of config/routes.rb does.
      again = cw.routes
      refute_same first, again
      lines, response = printed do
        send_request(again, "GET", "http://localhost:3000/cronwatch/", { "cookie" => cookie }, script_name: "/cronwatch")
      end
      assert_equal 200, response.status, "the cookie the first app set still signs in"
      assert_empty lines, "and the same link is not printed again"
      bearer = { "authorization" => "Bearer #{token}" }
      assert_equal 200, send_request(again, "GET", "http://localhost:3000/cronwatch/api/jobs", bearer, script_name: "/cronwatch").status
    end
  end

  def test_a_link_that_differs_is_still_printed_and_each_client_has_its_own_token
    development do
      cw = client
      lines, = printed do
        send_request(cw.routes(base_path: "/cronwatch"), "GET", "http://localhost:3000/cronwatch/")
        send_request(cw.routes(base_path: "/cronwatch"), "GET", "http://localhost:3000/cronwatch/")
        send_request(cw.routes(base_path: "/admin"), "GET", "http://localhost:3000/admin/")
        send_request(cw.routes(base_path: "/cronwatch"), "GET", "http://127.0.0.1:4000/cronwatch/")
      end
      assert_equal 3, lines.length, lines.inspect
      tokens = lines.map { |line| line[/token=([A-Za-z0-9_-]{43})/, 1] }
      assert_equal 1, tokens.uniq.length, "one token for the client, wherever it is mounted"

      other, = printed { send_request(client.routes(base_path: "/cronwatch"), "GET", "http://localhost:3000/cronwatch/") }
      refute_equal tokens[0], other[0][/token=([A-Za-z0-9_-]{43})/, 1], "each client makes its own"
    end
  end

  def test_an_app_without_a_client_makes_a_token_of_its_own
    development do
      webs = Array.new(2) { Cronwatch::Web.build(nil, base_path: "/cronwatch") }
      lines, = printed { webs.each { |web| send_request(web, "GET", "http://localhost:3000/cronwatch/") } }
      assert_equal 2, lines.length
      refute_equal(*lines)
    end
  end
end
