using System;
using System.Collections.Generic;
using System.Diagnostics;
using System.Globalization;
using System.Linq;
using System.Threading;
using System.Threading.Tasks;
using Cronwatch.Internal;

namespace Cronwatch.Alerts;

/// <summary>Configures <see cref="WebhookChannel"/>. Its <see cref="ToString"/> never shows the URL, a header's value, or the secret.</summary>
[DebuggerDisplay("{ToString(),nq}")]
public sealed class WebhookOptions
{
    /// <summary>
    /// Where each alert is posted. Errors name only its origin, since a webhook URL's path or
    /// query is often the credential.
    /// </summary>
    public string? Url { internal get; init; }

    /// <summary>
    /// Extra request headers, an <c>authorization</c> header say, as name and value pairs sent in
    /// the order given; a name given again keeps its place and takes the new value. Values are
    /// trimmed of the spaces and newlines a paste leaves. A name is matched without regard to
    /// case, so a <c>Content-Type</c> given here replaces the default one rather than going beside
    /// it; the signature headers are always the channel's own. <c>host</c>, <c>content-length</c>,
    /// <c>connection</c>, <c>expect</c>, <c>upgrade</c>, and <c>transfer-encoding</c> are set by
    /// the default transport itself, which drops them.
    /// </summary>
    public IEnumerable<KeyValuePair<string, string>>? Headers { internal get; init; }

    /// <summary>
    /// When set, each request is signed with this secret (HMAC-SHA256, as lowercase hex), so the
    /// receiver can verify it (<see cref="WebhookChannel.Signature"/>):
    /// <list type="bullet">
    /// <item><c>x-cronwatch-signature: sha256=&lt;hex&gt;</c> signs the raw body alone, as it
    /// always has.</item>
    /// <item><c>x-cronwatch-timestamp: &lt;unix seconds&gt;</c> is when it was sent, and
    /// <c>x-cronwatch-timestamp-signature: sha256=&lt;hex&gt;</c> signs
    /// <c>&lt;timestamp&gt;.&lt;raw body&gt;</c>, so a receiver that checks this one with a
    /// constant-time compare and refuses a timestamp more than five minutes from its own clock
    /// cannot be sent a captured alert again later.</item>
    /// </list>
    /// An empty string (an unset variable read as <c>""</c>, say) signs nothing, and each send
    /// reports that through the channel's <see cref="ChannelContext.ReportError(string)"/>.
    /// </summary>
    public string? Secret { internal get; init; }

    /// <summary>The clock the <c>x-cronwatch-timestamp</c> header's time comes from. Default <see cref="TimeProvider.System"/>.</summary>
    public TimeProvider? Clock { get; init; }

    /// <summary>Sends this channel's requests; by default the client's transport.</summary>
    public ITransport? Transport { get; init; }

    /// <summary>Names what is set, never a value.</summary>
    public override string ToString() =>
        "WebhookOptions(url " + ChannelShared.Set(Url) + ", headers " + (Headers?.Count() ?? 0).ToString(CultureInfo.InvariantCulture)
        + (string.IsNullOrEmpty(Secret) ? "" : ", secret set") + ")";
}

/// <summary>
/// POSTs each alert as JSON to any URL (<c>alerts/webhook.ts</c>). The body is the payload's
/// version, <c>"schema":1</c>, first, then the <see cref="Alert"/> as the SDK writes it
/// (<see cref="Alert.ToJson"/>); its JSON Schema is https://cronwatch.dev/schemas/webhook/1.json.
/// A redirect is an error: point the URL at where the receiver really is.
/// </summary>
public sealed class WebhookChannel : IChannel
{
    /// <summary>
    /// The payload's version, sent as its first field. It goes up only if a major release changes
    /// the payload in a way that is not additive.
    /// </summary>
    private const int Schema = 1;

    private readonly string _url;
    private readonly List<KeyValuePair<string, string>> _headers;
    private readonly string _secret;
    private readonly bool _emptySecret;
    private readonly ITransport? _transport;
    private readonly TimeProvider _clock;

    /// <summary>The channel.</summary>
    /// <exception cref="CronwatchException">Without a URL.</exception>
    public WebhookChannel(WebhookOptions options)
    {
        ArgumentNullException.ThrowIfNull(options);
        if (string.IsNullOrEmpty(options.Url))
        {
            throw ChannelShared.Invalid("Webhook needs a Url");
        }
        _url = options.Url;
        _headers = (options.Headers ?? []).ToList();
        _secret = options.Secret ?? "";
        _emptySecret = options.Secret is { Length: 0 };
        _transport = options.Transport;
        _clock = options.Clock ?? TimeProvider.System;
    }

    /// <summary>
    /// The webhook's signature of a body: the HMAC-SHA256 of the body with the secret, as
    /// lowercase hex. The request carries it as <c>x-cronwatch-signature: sha256=&lt;signature&gt;</c>,
    /// so a receiver checks one with the raw body it got (before any JSON parsing) and a
    /// constant-time compare. The timestamped one is the signature of
    /// <c>&lt;timestamp&gt;.&lt;body&gt;</c>, with the <c>x-cronwatch-timestamp</c> value as sent.
    /// </summary>
    public static string Signature(string secret, string body)
    {
        ArgumentNullException.ThrowIfNull(secret);
        ArgumentNullException.ThrowIfNull(body);
        return ChannelShared.Hex(ChannelShared.HmacSha256(Js.Utf8(secret), body));
    }

    /// <inheritdoc/>
    public string Name => "webhook";

    /// <inheritdoc/>
    public async Task SendAsync(Alert alert, ChannelContext context, CancellationToken cancellationToken)
    {
        ArgumentNullException.ThrowIfNull(alert);
        ArgumentNullException.ThrowIfNull(context);
        var payload = new JsObject().Set("schema", Schema);
        foreach (var field in alert.ToValue())
        {
            payload.Set(field.Key, field.Value);
        }
        string body = payload.ToJson();
        if (_emptySecret)
        {
            context.ReportError("webhook: secret is an empty string, so this request is sent unsigned; set the secret, or leave it out to send unsigned on purpose");
        }
        // A JavaScript object's keys: an exact name given again keeps its place.
        var given = new JsObject();
        foreach (var h in _headers)
        {
            // A pasted Authorization value often carries a stray space or newline, which fetch
            // would refuse.
            given.Set(h.Key, Js.Trim(h.Value ?? ""));
        }
        var headers = new JsObject().Set("content-type", "application/json").Set("user-agent", "cronwatch");
        foreach (var h in given)
        {
            SetHeader(headers, h.Key, (string)h.Value!);
        }
        if (_secret.Length > 0)
        {
            string timestamp = Js.FloorDiv(_clock.GetUtcNow().ToUnixTimeMilliseconds(), 1000).ToString(CultureInfo.InvariantCulture);
            SetHeader(headers, "x-cronwatch-signature", "sha256=" + Signature(_secret, body));
            SetHeader(headers, "x-cronwatch-timestamp", timestamp);
            SetHeader(headers, "x-cronwatch-timestamp-signature", "sha256=" + Signature(_secret, timestamp + "." + body));
        }
        var list = new List<KeyValuePair<string, string>>();
        foreach (var e in headers)
        {
            list.Add(new(e.Key, (string)e.Value!));
        }
        // A redirect is refused, not followed: the headers (and the signature) would go with it.
        var answer = await Post.FetchAsync(ChannelShared.Transport(_transport, context), Post.Timeout, _url, list, body, cancellationToken).ConfigureAwait(false);
        if (!answer.Ok)
        {
            // Only the origin: a webhook URL's path or query often is the credential.
            throw Post.Fail("Webhook " + Post.Origin(_url) + " answered " + answer.Status.ToString(CultureInfo.InvariantCulture));
        }
    }

    /// <summary>Sets a header, first taking out any of the same name in another case.</summary>
    private static void SetHeader(JsObject headers, string name, string value)
    {
        foreach (string key in headers.Select(e => e.Key).ToList())
        {
            if (string.Equals(key, name, StringComparison.OrdinalIgnoreCase))
            {
                headers.Remove(key);
            }
        }
        headers.Set(name, value);
    }

    /// <summary>Names the channel.</summary>
    public override string ToString() => "WebhookChannel";
}
