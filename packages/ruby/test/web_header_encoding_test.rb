# frozen_string_literal: true

require_relative "web/helpers"

# Ruby only: a Rack server hands a header's bytes over as they came (Puma as
# ASCII-8BIT), and a cookie or an Authorization header may hold any. Node's
# header strings never fail to match a pattern; Ruby's raise on bytes that
# are not the pattern's encoding, so the routes read every header as UTF-8.
class WebHeaderEncodingTest < Minitest::Test
  include WebHelpers

  def app
    errors = []
    cw, = make(on_error: ->(e, where) { errors << [where, e.class.name] })
    cw.job("nightly", schedule: "0 2 * * *")
    [cw.routes(token: "tok", base_path: "/cronwatch"), errors]
  end

  def test_another_cookie_with_bytes_that_are_not_ascii_does_not_break_a_signed_in_browser
    web, errors = app
    [
      "name=Jos\xC3\xA9; cronwatch_token=#{DIGEST}".b,
      "cronwatch_token=#{DIGEST}; name=Jos\xC3\xA9".b,
      "name=\xFF\xFE; cronwatch_token=#{DIGEST}".b,
      "name=Jos\xC3\xA9; cronwatch_token=#{DIGEST}".dup.force_encoding(Encoding::UTF_8),
      "name=\xFF; cronwatch_token=#{DIGEST}".dup.force_encoding(Encoding::UTF_8),
    ].each do |cookie|
      assert_equal 200, send_request(web, "GET", "/cronwatch/api/jobs", { "cookie" => cookie }).status, cookie.inspect
      assert_equal 200, send_request(web, "GET", "/cronwatch/", { "cookie" => cookie }).status, cookie.inspect
    end
    assert_empty errors
  end

  def test_credentials_with_bytes_that_are_not_ascii_are_refused_not_an_error
    web, errors = app
    [
      { "authorization" => "Bearer \xE2\x9C\x93".b },
      { "authorization" => "Bearer \xFF".b },
      { "authorization" => "Bearer \xFF".dup.force_encoding(Encoding::UTF_8) },
      { "authorization" => "Basic \xFF".b },
      { "cookie" => "cronwatch_token=\xFF".b },
      { "cookie" => "cronwatch_token=%FF; a=\xC3\xA9".b },
    ].each do |headers|
      assert_equal 401, send_request(web, "GET", "/cronwatch/api/jobs", headers).status, headers.inspect
      assert_equal 401, send_request(web, "GET", "/cronwatch/", headers).status, headers.inspect
    end
    assert_empty errors, "nothing is reported for a request that is only refused"
  end

  def test_a_bearer_still_signs_in_beside_such_a_cookie
    web, errors = app
    headers = { "authorization" => "Bearer tok".b, "cookie" => "name=Jos\xC3\xA9".b }
    assert_equal 200, send_request(web, "GET", "/cronwatch/api/jobs", headers).status
    assert_empty errors
  end

  def test_the_other_headers_the_routes_read_take_any_bytes
    web, errors = app
    form = { "content-type" => "application/x-www-form-urlencoded" }
    [
      [{ "origin" => "http://ex\xFFample.test".b }, 403],
      [{ "sec-fetch-site" => "cross\xFF".b }, 403],
      [{ "referer" => "http://app.test/cronwatch/\xFF".b }, 303],
      [{ "x-forwarded-host" => "a\xFF".b, "x-forwarded-proto" => "htt\xFFp".b }, 303],
    ].each do |headers, status|
      response = send_request(web, "POST", "/cronwatch/check", COOKIE.merge(form).merge(headers), "")
      assert_equal status, response.status, headers.inspect
    end
    assert_equal 401, send_request(web, "POST", "/cronwatch/signin", { "content-type" => "application/x-www-form-urlencoded; \xFF".b }, "token=x").status
    assert_empty errors
  end
end
