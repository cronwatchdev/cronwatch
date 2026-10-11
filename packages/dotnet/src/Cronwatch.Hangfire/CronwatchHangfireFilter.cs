using System;
using System.Diagnostics;
using System.Diagnostics.CodeAnalysis;
using System.Globalization;
using System.Threading;
using System.Threading.Tasks;
using Hangfire.Common;
using Hangfire.Server;
using Hangfire.States;
using CronwatchJob = Cronwatch.Job;

namespace Cronwatch.Hangfire;

/// <summary>
/// The filter <see cref="CronwatchHangfire"/> adds to Hangfire: a server filter that records each
/// attempt of a watched job as a run, and a state election filter that records a job Hangfire
/// fails before it performs it. Nothing in it ever throws into Hangfire; what fails is reported to
/// the client's error handler.
/// </summary>
/// <remarks>
/// Hangfire calls <see cref="OnPerforming"/> in the worker's thread just before the job's method
/// and <see cref="OnPerformed"/> there just after, synchronously, awaiting an async method between
/// them. So the run is opened and made current in the first, which reaches the method (Hangfire
/// runs it inside the worker's execution context), and closed in the second, which takes it off
/// the worker's thread whatever happened, so a pooled worker keeps nothing. Each blocks the worker
/// on the store for at most <see cref="CronwatchHangfire.StoreWait"/>: past it the work goes on in
/// the background, so a store that does not answer costs an attempt one bounded wait, not its
/// worker.
/// </remarks>
[DebuggerDisplay("{ToString(),nq}")]
[SuppressMessage("Naming", "CA1725", Justification = "Hangfire renamed these parameters within 1.8 (filterContext in 1.8.0, context later), so no one name matches every supported release.")]
public sealed class CronwatchHangfireFilter : JobFilterAttribute, IServerFilter, IElectStateFilter
{
    private const string RunKey = "cronwatch.run";
    private const string CurrentKey = "cronwatch.current";

    private readonly CronwatchHangfire _integration;

    internal CronwatchHangfireFilter(CronwatchHangfire integration)
    {
        _integration = integration;
    }

    /// <summary>Opens a run for a watched job and makes it current in the worker's thread.</summary>
    public void OnPerforming(PerformingContext context)
    {
        ArgumentNullException.ThrowIfNull(context);
        try
        {
            if (_integration.Disposed || context.BackgroundJob.Job == null)
            {
                return; // a job that does not load is recorded from its state change
            }
            string? name = _integration.NameOf(context.BackgroundJob.Job, context.GetJobParameter<string>("RecurringJobId", true));
            if (name == null)
            {
                return;
            }
            // Hangfire's worker has no synchronization context and the client awaits nothing on
            // the caller's, so this blocks only for the store's write, and at most StoreWait.
            Task<CronwatchJob?> finding = _integration.JobAsync(name);
            if (!Waited(finding, "declaring " + name))
            {
                return; // declared in the background; this attempt goes unrecorded
            }
            CronwatchJob? job = finding.Result;
            if (job == null)
            {
                return;
            }
            // Counted by the integration, not Hangfire's RetryCount, which stays as it was when a
            // job is requeued from the dashboard or fetched again after its worker died, so a
            // second attempt at one retry count would take the first's id and go unrecorded.
            int attempt = context.GetJobParameter<int>(CronwatchHangfire.AttemptParameter, true);
            context.SetJobParameter(CronwatchHangfire.AttemptParameter, attempt + 1);
            var options = new RunOptions { Trigger = CronwatchHangfire.Trigger, Id = _integration.RunId(context.BackgroundJob.Id, attempt), MayTakeBack = true };
            Task<ObservedRun> opening = job.OpenAsync(options, context.CancellationToken.ShutdownToken);
            if (!Waited(opening, "opening a run of " + name))
            {
                // Still opening: closed when it is open, without being current in the job.
                context.Items[RunKey] = opening;
                return;
            }
            ObservedRun run = opening.Result;
            context.Items[RunKey] = run;
            context.Items[CurrentKey] = run.MakeCurrent();
        }
        catch (Exception e)
        {
            _integration.Client.ReportError(e, "hangfire");
        }
    }

    /// <summary>
    /// Closes the run: failed with what the job's method threw, given back when the server's
    /// shutdown stopped it or a filter after this one cancelled it, and succeeded otherwise.
    /// </summary>
    public void OnPerformed(PerformedContext context)
    {
        ArgumentNullException.ThrowIfNull(context);
        if (!context.Items.TryGetValue(RunKey, out object? found) || found is not (ObservedRun or Task<ObservedRun>))
        {
            return;
        }
        context.Items.Remove(RunKey);
        try
        {
            if (context.Items.TryGetValue(CurrentKey, out object? current) && current is IDisposable restore)
            {
                context.Items.Remove(CurrentKey);
                restore.Dispose();
            }
            Exception? raw = context.Exception;
            Exception? thrown = raw is JobPerformanceException { InnerException: { } inner } ? inner : raw;
            object? result = context.Result;
            Func<ObservedRun, Task> close = context.Canceled || GivenBack(raw, context)
                ? run => run.TakeBackAsync(thrown)
                : thrown == null
                    ? run => run.CloseWithAsync(result)
                    : run => run.CloseAsync(thrown);
            if (found is ObservedRun opened)
            {
                Waited(close(opened), "closing run " + opened.Id);
                return;
            }
            // Opened too late to wait for: closed as it ended, once it is open.
            var opening = (Task<ObservedRun>)found;
            _integration.Track(opening.ContinueWith(
                async t =>
                {
                    if (t.Status != TaskStatus.RanToCompletion)
                    {
                        return; // reported as the open failed
                    }
                    try
                    {
                        await close(await t.ConfigureAwait(false)).ConfigureAwait(false);
                    }
                    catch (Exception e)
                    {
                        _integration.Client.ReportError(e, "hangfire");
                    }
                },
                CancellationToken.None,
                TaskContinuationOptions.ExecuteSynchronously,
                TaskScheduler.Default).Unwrap());
        }
        catch (Exception e)
        {
            _integration.Client.ReportError(e, "hangfire");
        }
    }

    /// <summary>
    /// Waits for <paramref name="work"/> at most <see cref="CronwatchHangfire.StoreWait"/>, throwing
    /// what it threw. Past the wait it goes on in the background, which is reported, and false is
    /// answered; a failure it meets later is reported then.
    /// </summary>
    private bool Waited(Task work, string what)
    {
        TimeSpan wait = _integration.StoreWait;
        try
        {
            work.WaitAsync(wait).GetAwaiter().GetResult();
            return true;
        }
        catch (TimeoutException) when (!work.IsCompleted)
        {
            _integration.Client.ReportError(
                new TimeoutException(
                    "cronwatch: the store did not answer within " + ((long)wait.TotalMilliseconds).ToString(CultureInfo.InvariantCulture)
                    + "ms while " + what + ", so the worker went on and it finishes in the background"),
                "hangfire");
            _integration.Track(work.ContinueWith(
                t =>
                {
                    if (t.Exception is { } failed)
                    {
                        _integration.Client.ReportError(failed.GetBaseException(), "hangfire");
                    }
                },
                CancellationToken.None,
                TaskContinuationOptions.ExecuteSynchronously,
                TaskScheduler.Default));
            return false;
        }
    }

    /// <summary>
    /// A job stopped by its server's shutdown, which Hangfire puts back in its queue rather than
    /// fail; one aborted because it was deleted is a failure.
    /// </summary>
    private static bool GivenBack(Exception? thrown, PerformedContext context) =>
        thrown is OperationCanceledException and not JobAbortedException && context.CancellationToken.ShutdownToken.IsCancellationRequested;

    /// <summary>
    /// Records a failed run for a recurring job Hangfire fails before it performs it (its type or
    /// arguments no longer load), which never reaches <see cref="OnPerforming"/>: a failed state
    /// among the election's candidates, before a retry filter moves it to a scheduled one.
    /// </summary>
    public void OnStateElection(ElectStateContext context)
    {
        ArgumentNullException.ThrowIfNull(context);
        try
        {
            if (_integration.Disposed || context.BackgroundJob.Job != null)
            {
                return;
            }
            FailedState? failed = context.CandidateState as FailedState;
            if (failed == null)
            {
                foreach (IState state in context.TraversedStates)
                {
                    if (state is FailedState f)
                    {
                        failed = f;
                        break;
                    }
                }
            }
            if (failed == null)
            {
                return;
            }
            string? name = _integration.NameOf(null, context.GetJobParameter<string>("RecurringJobId", true));
            if (name == null)
            {
                return;
            }
            int attempt = context.GetJobParameter<int>(CronwatchHangfire.AttemptParameter, true);
            context.SetJobParameter(CronwatchHangfire.AttemptParameter, attempt + 1);
            Exception cause = failed.Exception is JobLoadException { InnerException: { } inner } ? inner : failed.Exception;
            string error = cause.GetType().Name + ": " + cause.Message;
            _integration.RecordLoadFailure(name, _integration.RunId(context.BackgroundJob.Id, attempt), error);
        }
        catch (Exception e)
        {
            _integration.Client.ReportError(e, "hangfire");
        }
    }

    /// <summary>Names the integration.</summary>
    public override string ToString() => "CronwatchHangfireFilter(" + _integration.Watch.AppTag + ")";
}
