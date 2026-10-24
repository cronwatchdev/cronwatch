//! Slack, through an incoming webhook (`alerts/slack.ts`).

use std::sync::Arc;

use super::post::{self, Transport, cut};
use super::shared::{LinkFn, invalid, link_for, triage};
use crate::deliver::{Channel, ChannelContext};
use crate::error::Error;
use crate::js::{LoneJson, Object, Value};
use crate::store::{BoxError, BoxFuture};
use crate::types::{Alert, AlertType};

/// Configures [`slack`].
#[derive(Clone, Default)]
#[non_exhaustive]
pub struct SlackOptions {
    /// An incoming webhook URL from api.slack.com/messaging/webhooks. It is
    /// its own credential: errors never quote it.
    pub webhook_url: String,
    /// A link back to the job in your dashboard. Any `<`, `>`, or `|` in it
    /// is percent-encoded, so it cannot end Slack's link early.
    pub link: Option<LinkFn>,
    /// Sends the request; `None` for the default [`ReqwestTransport`](super::ReqwestTransport).
    pub transport: Option<Arc<dyn Transport>>,
}

super::setters!(SlackOptions {
    text webhook_url,
    link link,
    some transport: Arc<dyn Transport>,
});

struct Slack(SlackOptions);

fn emoji(t: &AlertType) -> &'static str {
    match t {
        AlertType::Missed => ":hourglass_flowing_sand:",
        AlertType::Failed => ":x:",
        AlertType::Stuck => ":no_entry:",
        AlertType::Slow => ":turtle:",
        AlertType::OverBudget => ":moneybag:",
        AlertType::UnderFloor => ":chart_with_downwards_trend:",
        AlertType::Recovered => ":white_check_mark:",
        AlertType::Other(_) => "",
    }
}

/// Sends alerts to a Slack channel through an incoming webhook.
pub fn slack(options: SlackOptions) -> Result<Arc<dyn Channel>, Error> {
    if options.webhook_url.is_empty() {
        return Err(invalid("alerts::slack needs a webhook_url"));
    }
    Ok(Arc::new(Slack(options)))
}

impl Channel for Slack {
    fn name(&self) -> &str {
        "slack"
    }

    fn send<'a>(&'a self, alert: &'a Alert, _: &'a ChannelContext) -> BoxFuture<'a, Result<(), BoxError>> {
        Box::pin(async move {
            let o = &self.0;
            let link = link_for(&o.link, alert);
            let mut head = format!("{} *{}*", emoji(&alert.alert_type), escape(&alert.title));
            if !link.is_empty() {
                head.push_str(&format!(" (<{}|open>)", link_target(&link)));
            }
            let mut lone = LoneJson::new();
            let mut code: Vec<u16> = "```".encode_utf16().collect();
            code.extend(cut_escaped(&code_block_safe(&escape(&alert.message)), 2900).encode_utf16());
            code.extend("```".encode_utf16());
            let section = |text: Value| {
                Value::Object(
                    Object::new()
                        .with("type", "section")
                        .with("text", Object::new().with("type", "mrkdwn").with("text", text)),
                )
            };
            let mut blocks = vec![section(Value::from(head)), section(lone.string(&code))];
            if !triage(alert).is_empty() {
                // Its own block, so a long diagnosis cannot push a block past Slack's 3000 character limit.
                let text = cut_escaped(&format!("_Triage:_ {}", escape(triage(alert))), 3000);
                blocks.push(section(Value::from(text)));
            }
            let payload = Value::Object(
                Object::new()
                    // The notification fallback is parsed as mrkdwn too, so it is escaped like the blocks.
                    .with("text", escape(&format!("{}\n{}", alert.title, alert.message)))
                    .with("blocks", blocks),
            );
            let body = lone.stringify(&payload);
            let t = post::transport(&o.transport)?;
            // A redirect is refused, not followed: a webhook URL is its own credential.
            let answer = post::fetch(
                &*t,
                post::DEADLINE,
                &o.webhook_url,
                &[("content-type", "application/json".into())],
                body.into_bytes(),
            )
            .await?;
            if !answer.ok() {
                // The start of the answer, with the URL's credential cut out in case a proxy echoes the request.
                let secrets = post::url_secrets(&o.webhook_url);
                let secrets: Vec<&str> = secrets.iter().map(String::as_str).collect();
                return Err(post::fail(format!(
                    "Slack webhook answered {}: {}",
                    answer.status,
                    post::error_body(&answer.body, &secrets)
                )));
            }
            Ok(())
        })
    }
}

/// Slack's three control characters. Escaping `<` and `>` also stops
/// `<!channel>` and `<url|links>`.
fn escape(text: &str) -> String {
    text.replace('&', "&amp;").replace('<', "&lt;").replace('>', "&gt;")
}

/// Escaped text cut to at most `max` UTF-16 code units, splitting neither
/// an entity [`escape`] wrote nor a surrogate pair.
fn cut_escaped(text: &str, max: usize) -> String {
    let mut kept = cut(text, max);
    if let Some(amp) = kept.rfind('&') {
        if !kept[amp..].contains(';') {
            kept.truncate(amp);
        }
    }
    kept
}

/// A link's URL as Slack's `<url|text>` can hold it: `<`, `>`, and `|`
/// percent-encoded, which ends neither the link nor its text.
fn link_target(url: &str) -> String {
    url.replace('<', "%3C").replace('>', "%3E").replace('|', "%7C")
}

/// Breaks up ``` so text inside a code block cannot close it.
pub(crate) fn code_block_safe(text: &str) -> String {
    text.replace("```", "`\u{200b}`\u{200b}`")
}
