import type { Alert, AlertChannel } from "../types.js";
import { cut, drain, errorBody, errorText, fetchPost, urlSecrets } from "./shared.js";

export interface SlackOptions {
  /** An incoming webhook URL from api.slack.com/messaging/webhooks. */
  webhookUrl: string;
  /**
   * Link back to the job in your dashboard:
   * (alert) => `https://app.example.com/cronwatch/jobs/${encodeURIComponent(alert.job)}`.
   * Any `<`, `>`, or `|` in it is percent-encoded, so it cannot end Slack's link early.
   */
  link?: (alert: Alert) => string;
}

const EMOJI: Record<Alert["type"], string> = {
  missed: ":hourglass_flowing_sand:",
  failed: ":x:",
  stuck: ":no_entry:",
  slow: ":turtle:",
  over_budget: ":moneybag:",
  under_floor: ":chart_with_downwards_trend:",
  recovered: ":white_check_mark:",
};

/** Sends alerts to a Slack channel through an incoming webhook. */
export function slack(options: SlackOptions): AlertChannel {
  if (!options.webhookUrl) throw new Error("slack() needs a webhookUrl");
  return {
    name: "slack",
    async send(alert) {
      const url = options.link?.(alert);
      const title = `${EMOJI[alert.type]} *${escape(alert.title)}*${url ? ` (<${linkTarget(url)}|open>)` : ""}`;
      const body = cutEscaped(codeBlockSafe(escape(alert.message)), 2900);
      const response = await fetchPost(options.webhookUrl, {
        headers: { "content-type": "application/json" },
        body: JSON.stringify({
          // The notification fallback is parsed as mrkdwn too, so it is escaped like the blocks.
          text: escape(`${alert.title}\n${alert.message}`),
          blocks: [
            { type: "section", text: { type: "mrkdwn", text: title } },
            { type: "section", text: { type: "mrkdwn", text: "```" + body + "```" } },
            // Its own block, so a long diagnosis cannot push a block past Slack's 3000 character limit.
            ...(alert.triage ? [{ type: "section", text: { type: "mrkdwn", text: cutEscaped(`_Triage:_ ${escape(alert.triage)}`, 3000) } }] : []),
          ],
        }),
      });
      // The start of the answer, with the URL's credential cut out in case a proxy echoes the request.
      if (!response.ok) throw new Error(`Slack webhook answered ${response.status}: ${errorBody(await errorText(response), urlSecrets(options.webhookUrl))}`);
      await drain(response);
    },
  };
}

/** Slack's three control characters. Escaping < and > also stops <!channel> and <url|links>. */
function escape(text: string): string {
  return text.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
}

/** Escaped text cut to at most `max` units, splitting neither an entity escape() wrote nor a surrogate pair. */
function cutEscaped(text: string, max: number): string {
  const kept = cut(text, max);
  const amp = kept.lastIndexOf("&");
  return amp !== -1 && !kept.includes(";", amp) ? kept.slice(0, amp) : kept;
}

/** A link's URL as Slack's <url|text> can hold it: `<`, `>`, and `|` percent-encoded, which ends neither the link nor its text. */
function linkTarget(url: string): string {
  return url.replace(/</g, "%3C").replace(/>/g, "%3E").replace(/\|/g, "%7C");
}

/** Breaks up ``` so text inside a code block cannot close it. */
function codeBlockSafe(text: string): string {
  return text.replace(/```/g, "`​`​`");
}
