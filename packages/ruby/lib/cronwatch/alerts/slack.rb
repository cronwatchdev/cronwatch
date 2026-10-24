# frozen_string_literal: true

module Cronwatch
  module Alerts
    # Sends alerts to a Slack channel through an incoming webhook.
    #
    #   Cronwatch::Alerts::Slack.new(webhook_url: ENV.fetch("SLACK_WEBHOOK_URL"),
    #                                link: ->(alert) { "https://app.example.com/cronwatch/jobs/#{ERB::Util.url_encode(alert.job)}" })
    #
    # Any `<`, `>`, or `|` in the link is percent-encoded, so it cannot end Slack's link early.
    class Slack
      EMOJI = {
        missed: ":hourglass_flowing_sand:", failed: ":x:", stuck: ":no_entry:", slow: ":turtle:",
        over_budget: ":moneybag:", under_floor: ":chart_with_downwards_trend:", recovered: ":white_check_mark:",
      }.freeze
      private_constant :EMOJI

      attr_reader :name

      def initialize(webhook_url:, link: nil, http: HTTP.default)
        raise ArgumentError, "Cronwatch::Alerts::Slack needs a webhook_url" if webhook_url.nil? || webhook_url.to_s.empty?

        @name = "slack"
        @webhook_url = webhook_url
        @link = link
        @http = http
      end

      def call(alert)
        url = @link&.call(alert)
        title = "#{EMOJI[alert.type]} *#{Slack.escape(alert.title)}*#{url && !url.empty? ? " (<#{Slack.link_target(url)}|open>)" : ""}"
        body = Slack.cut_escaped(Slack.code_block_safe(Slack.escape(alert.message)), 2900)
        blocks = [
          { "type" => "section", "text" => { "type" => "mrkdwn", "text" => title } },
          { "type" => "section", "text" => { "type" => "mrkdwn", "text" => "```#{body}```" } },
        ]
        if alert.triage && !alert.triage.empty?
          # Its own block, so a long diagnosis cannot push a block past Slack's 3000 character limit.
          blocks << { "type" => "section", "text" => { "type" => "mrkdwn", "text" => Slack.cut_escaped("_Triage:_ #{Slack.escape(alert.triage)}", 3000) } }
        end
        payload = {
          # The notification fallback is parsed as mrkdwn too, so it is escaped like the blocks.
          "text" => Slack.escape("#{alert.title}\n#{alert.message}"),
          "blocks" => blocks,
        }
        response = @http.post(@webhook_url, JS.json(payload), { "content-type" => "application/json" })
        raise "Slack webhook answered #{response.status}: #{Provider.webhook_error_body(response, @webhook_url)}" unless response.ok?
      end

      # Slack's three control characters. Escaping < and > also stops <!channel> and <url|links>.
      def self.escape(text)
        text.gsub("&", "&amp;").gsub("<", "&lt;").gsub(">", "&gt;")
      end

      # Escaped text cut to at most `max` UTF-16 units, splitting neither an
      # entity escape wrote nor a surrogate pair.
      def self.cut_escaped(text, max)
        kept = JS.head16(text, max)
        amp = kept.rindex("&")
        amp && !kept.index(";", amp) ? kept[0, amp] : kept
      end

      # A link's URL as Slack's <url|text> can hold it: `<`, `>`, and `|`
      # percent-encoded, which ends neither the link nor its text.
      def self.link_target(url)
        url.gsub("<", "%3C").gsub(">", "%3E").gsub("|", "%7C")
      end

      # Breaks up ``` so text inside a code block cannot close it.
      def self.code_block_safe(text)
        text.gsub("```", "`\u200b`\u200b`")
      end
    end
  end
end
