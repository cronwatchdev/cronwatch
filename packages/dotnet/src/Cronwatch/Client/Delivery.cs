using System;
using System.Collections.Generic;
using System.Threading;
using System.Threading.Tasks;
using Cronwatch.Internal;

namespace Cronwatch;

/// <summary>Delivery: channels, triage, and the queue of alerts no channel accepted.</summary>
public sealed partial class CronwatchClient
{
    /// <summary>Alerts written with the state that opened their conditions, and how many older ones the queue let go.</summary>
    internal sealed record Held(IReadOnlyList<Alert> Alerts, int Dropped);

    /// <summary>
    /// An evaluation as it is written: its drafts composed into alerts and held in the same state
    /// (<see cref="Evaluate.HoldAlerts"/>), so the write that opens a condition also keeps its
    /// alerts, and a process that stops before sending them does not lose them. Called inside
    /// <see cref="UpdateStateAsync{TResult}"/>, so it only computes.
    /// </summary>
    internal (JobState State, Held Result) Outbox(Evaluation settled, Definition def, long now)
    {
        var alerts = new List<Alert>(settled.Alerts.Count);
        foreach (var draft in settled.Alerts)
        {
            alerts.Add(AlertFormat.ComposeAlert(draft, def, now));
        }
        var (state, dropped) = Evaluate.HoldAlerts(settled.State, alerts, Evaluate.SaturatingAdd(Now(), Evaluate.SendLeaseMs), _deferDelivery);
        return (state, new Held(alerts, dropped));
    }

    /// <summary>Reports alerts let go because a job's queue was full.</summary>
    private void ReportDropped(string name, int dropped)
    {
        if (dropped <= 0)
        {
            return;
        }
        Report(
            dropped + " undelivered alert" + (dropped == 1 ? "" : "s") + " for " + name + " dropped: only the newest "
            + Evaluate.MaxUndelivered + " are kept for retry",
            "alert queue for " + name);
    }

    /// <summary>
    /// Triages and sends each alert the outbox holds (see <see cref="Outbox"/>). The state, with
    /// the alerts in it, was saved before this, so a slow channel holds up nothing else;
    /// afterwards only the delivery fields are written back, onto a fresh read of the state, and
    /// the alerts leave <c>sending</c>. Triage is made here, never stored with the held alert.
    /// Delivering at check, the alerts were queued for a check elsewhere instead.
    /// </summary>
    internal async Task<IReadOnlyList<Alert>> DispatchAsync(string name, IReadOnlyList<Alert> alerts, long now)
    {
        if (alerts.Count == 0)
        {
            return alerts;
        }
        if (_deferDelivery)
        {
            foreach (var alert in alerts)
            {
                Count(alert, "queued");
            }
            return alerts;
        }
        var sent = new List<Alert>(alerts.Count);
        var delivered = new List<Alert>();
        var failed = new List<Alert>();
        foreach (var held in alerts)
        {
            Alert alert = held;
            if (_triage != null && alert.Type != AlertType.Recovered)
            {
                alert = await AddTriageAsync(alert, Timings.Triage).ConfigureAwait(false);
            }
            (await DeliverAsync(alert).ConfigureAwait(false) ? delivered : failed).Add(alert);
            sent.Add(alert);
        }
        await RecordDeliveryAsync(name, delivered, failed, [], now).ConfigureAwait(false);
        return sent;
    }

    /// <summary>What one check has spent retrying alerts, across all jobs.</summary>
    internal sealed class RetryBudget
    {
        public TimeSpan Spent { get; set; }
    }

    /// <summary>
    /// Retries a job's queued alerts once, oldest first, within what is left of the check's
    /// budget. They are claimed first (<see cref="Evaluate.ClaimUndelivered"/>): moved into
    /// <c>sending</c> with a lease, in one conditional write, so when several processes check at
    /// once only the one whose write lands sends them. One that no longer describes the job is
    /// dropped instead. Claimed alerts the budget has no time left for go back to the queue for
    /// the next check.
    /// </summary>
    internal async Task<IReadOnlyList<Alert>> RetryUndeliveredAsync(string name, JobState state, long now, RetryBudget budget)
    {
        if (state.Undelivered is not { Count: > 0 } || _deferDelivery)
        {
            return [];
        }
        List<Alert> claimed;
        try
        {
            (_, (claimed, List<Alert> stale)) = await UpdateStateAsync(name, previous =>
            {
                var claim = Evaluate.ClaimUndelivered(previous, now, Now() + Evaluate.SendLeaseMs);
                return (claim.State, (claim.Claimed, claim.Stale));
            }).ConfigureAwait(false);
            foreach (var a in stale)
            {
                Count(a, "dropped");
            }
        }
        catch (Exception e)
        {
            Report(e, "claiming undelivered alerts for " + name);
            return [];
        }
        if (claimed.Count == 0)
        {
            return [];
        }
        var delivered = new List<Alert>();
        var failed = new List<Alert>();
        foreach (var a in claimed)
        {
            TimeSpan left = Timings.RetryBudget - budget.Spent;
            if (left <= TimeSpan.Zero)
            {
                // Not tried: back to the queue as it was.
                failed.Add(a);
                continue;
            }
            long started = _time.GetTimestamp();
            Alert alert = a;
            // An alert queued by a process that delivers at check time was never triaged. One
            // that was tried is not tried again.
            if (_triage != null && alert.Type != AlertType.Recovered && !alert.TriageTried)
            {
                alert = await AddTriageAsync(alert, left < Timings.Triage ? left : Timings.Triage).ConfigureAwait(false);
            }
            (await DeliverAsync(alert).ConfigureAwait(false) ? delivered : failed).Add(alert);
            budget.Spent += _time.GetElapsedTime(started);
        }
        await RecordDeliveryAsync(name, delivered, failed, [], now).ConfigureAwait(false);
        return delivered;
    }

    /// <summary>
    /// Marks delivered alerts done, drops stale ones, and keeps failed ones for the next check,
    /// taking them all out of <c>sending</c> (<see cref="Evaluate.RecordSent"/>). A failed alert
    /// replaces its stored copy, so a triage made on this attempt is kept. <c>lastAlertAt</c>
    /// moves only on a delivery. When this write fails, alerts still in <c>sending</c> are retried
    /// once their lease runs out.
    /// </summary>
    private async Task RecordDeliveryAsync(string name, List<Alert> delivered, List<Alert> failed, List<Alert> dropped, long now)
    {
        try
        {
            var (_, trimmed) = await UpdateStateAsync(name, previous =>
                Evaluate.RecordSent(Evaluate.NormalizeState(previous, name), delivered, failed, dropped, now)).ConfigureAwait(false);
            ReportDropped(name, trimmed);
        }
        catch (Exception e)
        {
            Report(e, "recording alert delivery for " + name);
        }
    }

    /// <summary>
    /// Sends the alert to every channel at once, each within its time; true when any accepted it
    /// (or there are none).
    /// </summary>
    internal async Task<bool> DeliverAsync(Alert alert)
    {
        if (_channels.Count == 0)
        {
            return true;
        }
        var sends = new Task<bool>[_channels.Count];
        for (int i = 0; i < _channels.Count; i++)
        {
            IChannel channel = _channels[i];
            sends[i] = Spawn(() => SendOneAsync(channel, alert));
        }
        bool ok = false;
        foreach (bool sent in await Task.WhenAll(sends).ConfigureAwait(false))
        {
            ok |= sent;
        }
        return ok;
    }

    private async Task<bool> SendOneAsync(IChannel channel, Alert alert)
    {
        string where = "alert channel " + channel.Name;
        var context = new ChannelContext(e => Report(e, where), _transport);
        using var cts = CancellationTokenSource.CreateLinkedTokenSource(_closing.Token);
        Task? send = null;
        try
        {
            send = channel.SendAsync(alert, context, cts.Token);
            await send.WaitAsync(Timings.Channel, _time).ConfigureAwait(false);
            Count(alert, "sent", channel.Name);
            return true;
        }
        catch (TimeoutException) when (send is { IsCompleted: false })
        {
            // The client's deadline. A channel's own TimeoutException, thrown or faulting its
            // task, is its failure, reported as any other.
            Abandon(send);
            await CancelQuietlyAsync(cts, where).ConfigureAwait(false);
            Report("timed out after " + (long)Timings.Channel.TotalMilliseconds + "ms", where);
        }
        catch (Exception e)
        {
            Report(e, where);
        }
        Count(alert, "failed", channel.Name);
        return false;
    }

    /// <summary>Cancels work given up on; a callback registered on its token that throws is reported, not thrown.</summary>
    private async Task CancelQuietlyAsync(CancellationTokenSource cts, string where)
    {
        try
        {
            await cts.CancelAsync().ConfigureAwait(false);
        }
        catch (Exception e)
        {
            Report(e, where);
        }
    }

    private static void Count(Alert alert, string outcome, string? channel = null) =>
        CronwatchTelemetry.Add(
            CronwatchTelemetry.Alerts,
            new("cronwatch.channel", channel),
            new("cronwatch.type", alert.Type.Value),
            new("cronwatch.outcome", outcome));

    private async Task<Alert> AddTriageAsync(Alert alert, TimeSpan timeout)
    {
        string where = "triage for " + alert.Job;
        ITriage? triage = _triage;
        if (triage == null)
        {
            return alert.WithTriage(null);
        }
        IReadOnlyList<Run> recent;
        try
        {
            recent = await CallAsync(() => _store.ListRunsAsync(alert.Job, 5)).ConfigureAwait(false);
        }
        catch (Exception e)
        {
            Report(e, where);
            return alert.WithTriage(null);
        }
        using var cts = CancellationTokenSource.CreateLinkedTokenSource(_closing.Token);
        Task<string?>? task = null;
        try
        {
            var context = new TriageContext(alert, recent) { Transport = _transport };
            task = Spawn(() => triage.TriageAsync(context, cts.Token));
            string? text = await task.WaitAsync(timeout, _time).ConfigureAwait(false);
            return alert.WithTriage(string.IsNullOrEmpty(text) ? null : text);
        }
        catch (TimeoutException) when (task is { IsCompleted: false })
        {
            await CancelQuietlyAsync(cts, where).ConfigureAwait(false);
            Report("timed out after " + (long)timeout.TotalMilliseconds + "ms", where);
        }
        catch (Exception e)
        {
            Report(e, where);
        }
        return alert.WithTriage(null);
    }
}
