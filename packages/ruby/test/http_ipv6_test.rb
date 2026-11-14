# frozen_string_literal: true

require_relative "test_helper"
require "socket"

# Ruby only: a URL's host is an IPv6 literal in brackets, and the name lookup
# takes it without them. fetch takes such a URL as it is.
class HttpIpv6Test < Minitest::Test
  include TestHelpers

  def ipv6_server(status)
    server = begin
      TCPServer.new("::1", 0)
    rescue SystemCallError, SocketError
      skip "this machine has no IPv6 loopback"
    end
    requests = Queue.new
    thread = Thread.new do
      loop do
        client = server.accept
        head = +""
        head << client.readpartial(4096) until head.include?("\r\n\r\n")
        requests << head
        client.write("HTTP/1.1 #{status} X\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
        client.close
      end
    rescue IOError, SystemCallError
      nil
    end
    yield server.addr[1], requests
  ensure
    server&.close
    thread&.kill
  end

  def test_a_url_with_an_ipv6_literal_host_is_posted_to
    ipv6_server(200) do |port, requests|
      response = Cronwatch::HTTP::NetHTTP.new(timeout: 5).post("http://[::1]:#{port}/hook?a=1", "{}", { "content-type" => "application/json" })
      assert_equal 200, response.status
      head = requests.pop
      assert_match(%r{\APOST /hook\?a=1 HTTP/1\.1\r\n}, head)
      assert_match(/^Host: \[::1\]:#{port}\r$/i, head, "the Host header keeps the brackets")
    end
  end

  def test_a_webhook_channel_reaches_an_ipv6_literal_and_names_it_in_an_error
    alert = Cronwatch::Alert.new(type: :failed, job: "nightly", title: "nightly failed", message: "boom", at: T0, details: {})
    ipv6_server(200) do |port, requests|
      Cronwatch::Alerts::Webhook.new(url: "http://[::1]:#{port}/hook").call(alert)
      assert_match(%r{\APOST /hook }, requests.pop)
    end
    ipv6_server(500) do |port, _|
      error = assert_raises(RuntimeError) { Cronwatch::Alerts::Webhook.new(url: "http://[::1]:#{port}/hook").call(alert) }
      assert_equal "Webhook http://[::1]:#{port} answered 500", error.message
    end
  end
end
