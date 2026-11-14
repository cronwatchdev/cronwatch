using System;
using System.Collections.Concurrent;
using System.Collections.Generic;
using System.Diagnostics;
using System.Linq;
using System.Threading;
using System.Threading.Tasks;
using Cronwatch.Internal;

namespace Cronwatch;

/// <summary>Runs: a function run in the caller's flow, recorded by the client's own tasks.</summary>
public sealed partial class CronwatchClient
{
    private static readonly int HistoryPage = Evaluate.BaselineWindow + 5;
    private const int HistoryMax = 200;
    private static readonly RunStatus[] OnlyRunning = [RunStatus.Running];
    private static readonly RunStatus[] OnlyTimeout = [RunStatus.Timeout];

    internal const string ShutdownError = "Shutdown: the process stopped while the run was in progress";

    /// <summary>The error a check gives an import whose finish never landed (<see cref="Evaluate.IsStranded"/>).</summary>
    internal const string StrandedError = "Imported, but its finish was never written; marked as timed out";

    /// <summary>Runs open in this process, for the process-exit hook.</summary>
    private readonly ConcurrentDictionary<string, OpenRun> _open = new(StringComparer.Ordinal);

    /// <summary>Whether the process-exit hook would find this run open, for the tests.</summary>
    internal bool IsOpenRun(string id) => _open.ContainsKey(id);

    internal sealed class OpenRun(JobDef def, Run run, Recorder recorder, Task<bool> begun)
    {
        public JobDef Def { get; } = def;

        public Run Run { get; } = run;

        public Recorder Recorder { get; } = recorder;

        /// <summary>The write of the run's row, true once it is there; the hook waits for it.</summary>
        public Task<bool> Begun { get; } = begun;

        public Task<Run>? Recording { get; set; }

        /// <summary>
        /// The write of the run's row was given up on at the deadline, which was reported: what it
        /// throws in the end is not reported again, since nothing was left behind.
        /// </summary>
        public volatile bool GivenUp;
    }

    /// <summary>A run opened by <see cref="OpenAsync"/>: its function running until it is closed.</summary>
    internal sealed class Opened
    {
        private readonly Lock _lock = new();
        private bool _closed;
        private bool _inFunction = true;

        public required JobDef Def { get; init; }

        public required RunOptions Options { get; init; }

        public required Run Run { get; init; }

        public required bool Recorded { get; init; }

        public required Task? Closing { get; init; }

        public required Recorder Recorder { get; init; }

        public required JobContext Context { get; init; }

        public required CancellationTokenSource Timeout { get; init; }

        public required CancellationTokenSource Linked { get; init; }

        public required OpenRun Open { get; init; }

        // A start write given up on at the deadline (Timings.StartDeadline): whether it has landed
        // since, and how the run ended, so the two meet whichever comes first.
        private bool _landedLate;
        private Ending _ending;
        private Run? _finished;
        private long _finishedAt;

        /// <summary>How a run whose start write was given up on has ended, for the write that lands after it.</summary>
        internal enum Ending
        {
            /// <summary>Its function is still running, or its finish is still being recorded.</summary>
            None,

            /// <summary>Its finish was recorded, or failed to be: <c>Run</c> is the run as it finished.</summary>
            Finished,

            /// <summary>It was given back: a row that lands is deleted.</summary>
            GivenBack,
        }

        /// <summary>Whether the start is in the store: written in time, or given up on and landed since.</summary>
        public bool StartWritten
        {
            get
            {
                lock (_lock)
                {
                    return Recorded || _landedLate;
                }
            }
        }

        /// <summary>The start write landed after it was given up on; answers how the run has ended by now.</summary>
        public (Ending Ending, Run? Run, long FinishedAt) Landed()
        {
            lock (_lock)
            {
                _landedLate = true;
                return (_ending, _finished, _finishedAt);
            }
        }

        /// <summary>The run has ended; answers whether its start is in the store by now.</summary>
        public bool Ended(Ending ending, Run? finished, long finishedAt)
        {
            lock (_lock)
            {
                _ending = ending;
                _finished = finished;
                _finishedAt = finishedAt;
                return Recorded || _landedLate;
            }
        }

        public bool Claim()
        {
            lock (_lock)
            {
                if (_closed)
                {
                    return false;
                }
                _closed = true;
                return true;
            }
        }

        public bool IsClosed
        {
            get
            {
                lock (_lock)
                {
                    return _closed;
                }
            }
        }

        public void EndFunction()
        {
            lock (_lock)
            {
                if (!_inFunction)
                {
                    return;
                }
                _inFunction = false;
            }
            Linked.Dispose();
            Timeout.Dispose();
        }
    }

    private static double TimeoutOrDefault(Definition def)
    {
        try
        {
            return Evaluate.TimeoutMs(def);
        }
        catch (Exception)
        {
            return Evaluate.DefaultTimeoutMs;
        }
    }

    internal static string TimeoutText(Definition def) =>
        "Still running after " + Durations.Format(TimeoutOrDefault(def)) + "; marked as timed out";

    internal static string ErrorText(Exception e)
    {
        try
        {
            return OutputText.DescribeError(e);
        }
        catch (Exception)
        {
            // An exception whose message or trace cannot be read must not leave its run running.
            return OutputText.DescribeError(OutputText.ErrorName(e), "", []);
        }
    }

    /// <summary>
    /// The longest run id, in UTF-16 code units (JavaScript's string length): what a run's start,
    /// <c>ResumeAsync</c>, and <c>RecordRunAsync</c> take, and every store holds.
    /// </summary>
    private const int MaxRunId = 200;

    internal static void CheckRunId(string job, string id, string method)
    {
        if (id.Length == 0 || id.Length > MaxRunId)
        {
            throw CronwatchException.Invalid(
                "job " + JsonText.Quote(job) + ": " + method + "() needs a run id of 1 to " + MaxRunId + " characters (got " + id.Length + " characters)");
        }
        // Postgres refuses NUL in text, so no store could hold such an id.
        if (id.Contains('\0', StringComparison.Ordinal))
        {
            throw CronwatchException.Invalid("job " + JsonText.Quote(job) + ": " + method + "() cannot take a run id containing a NUL character");
        }
        if (id.StartsWith(ReservedRunIdPrefix, StringComparison.Ordinal))
        {
            throw CronwatchException.Invalid(
                "job " + JsonText.Quote(job) + ": " + method + "() cannot take a run id starting with " + JsonText.Quote(ReservedRunIdPrefix)
                + ", which the pg_cron source uses for its runs");
        }
    }

    /// <summary>
    /// Runs <paramref name="fn"/> in the caller's flow as a recorded run, and answers what it
    /// answered, or throws what it threw, unchanged, once the run is recorded.
    /// </summary>
    internal async Task<T> ExecuteAsync<T>(JobDef def, RunOptions options, Func<JobContext, CancellationToken, Task<T>> fn, CancellationToken cancellationToken)
    {
        var (_, value, thrown) = await ExecuteCaughtAsync(def, options, fn, cancellationToken).ConfigureAwait(false);
        if (thrown != null)
        {
            System.Runtime.ExceptionServices.ExceptionDispatchInfo.Throw(thrown);
        }
        return value!;
    }

    /// <summary>A run opened from outside its function, with its activity: <see cref="Job.OpenAsync"/>.</summary>
    internal async Task<ObservedRun> OpenObservedAsync(JobDef def, RunOptions options, CancellationToken cancellationToken)
    {
        // Started here, so its parent is the caller's activity; this method's end puts the
        // caller's back as current, and the run's own is made current by MakeCurrent.
        Activity? activity = CronwatchTelemetry.StartActivity("cronwatch.run");
        try
        {
            activity?.SetTag("cronwatch.job", def.Name);
            activity?.SetTag("cronwatch.trigger", options.Trigger);
            Opened o = await OpenAsync(def, options, cancellationToken).ConfigureAwait(false);
            activity?.SetTag("cronwatch.run_id", o.Run.Id);
            return new ObservedRun(this, o, activity);
        }
        catch (Exception)
        {
            CronwatchTelemetry.StopActivity(activity);
            throw;
        }
    }

    /// <summary>Opens a run: the running row written by the client's own task, the timeout armed.</summary>
    internal async Task<Opened> OpenAsync(JobDef def, RunOptions options, CancellationToken cancellationToken)
    {
        ArgumentNullException.ThrowIfNull(options);
        if (options.Id != null)
        {
            CheckRunId(def.Name, options.Id, "run");
        }
        long startedAt = Now();
        Run run = Run.Running(options.Id ?? Guid.NewGuid().ToString(), def.Name, startedAt, options.Trigger ?? "run");
        // The start is written by the client's own task: a caller that stops waiting cannot cut it.
        // The process-exit hook is told of the run before its row is written, so a process that
        // stops once the row is there always finds it: the hook waits for the write and records
        // the run failed.
        var recorder = new Recorder();
        // Listed first and written after: written first, the write could end and the process
        // begin to stop (its hook reading the list) before this thread had listed the run.
        var begin = new TaskCompletionSource<Task<bool>>(TaskCreationOptions.RunContinuationsAsynchronously);
        Task<bool> begun = begin.Task.Unwrap();
        var open = new OpenRun(def, run, recorder, begun);
        // A run id already open in this process keeps its place: this run's insert is refused.
        bool listed = _open.TryAdd(run.Id, open);
        begin.SetResult(Spawn(() => BeginRunAsync(def, run, open)));
        bool recorded;
        bool givenUp = false;
        try
        {
            recorded = await begun.WaitAsync(Timings.StartDeadline, _time, CancellationToken.None).ConfigureAwait(false);
        }
        catch (TimeoutException) when (!begun.IsCompleted)
        {
            // A store that hangs must not hold the job back: it runs now, as one whose start was
            // never written. The write is still under way and may land later, leaving a running
            // row. One that lands before the finish is finished over as usual; one that lands
            // after it is finished then (LandedLate), so no row is left running to be called stuck.
            recorded = false;
            givenUp = true;
            open.GivenUp = true;
            Report(
                "the store took longer than " + Durations.Format(Timings.StartDeadline.TotalMilliseconds) + " to record the start of " + def.Name + "; running it unrecorded",
                "recording " + def.Name);
        }
        if (!recorded && listed && !givenUp)
        {
            // Nothing was written, so the hook has nothing to record.
            _open.TryRemove(new KeyValuePair<string, OpenRun>(run.Id, open));
        }
        // Closing missed happens beside the job, which never waits on it. A run that may
        // be given back closes it only once it is known not to be.
        Task? closing = recorded && !options.TakesBack ? Spawn(() => CloseOnStartAsync(def, run)) : null;

        double timeout = TimeoutOrDefault(def.Stored);
        double delay = Math.Min(Math.Max(0, timeout), Evaluate.MaxDurationMs);
        // A timer holds at most 2^32 - 2 ms (some 49 days); a longer timeout never fires in a
        // process's life.
        var timeoutCts = delay <= uint.MaxValue - 1
            ? new CancellationTokenSource(TimeSpan.FromMilliseconds(Math.Floor(delay)), _time)
            : new CancellationTokenSource();
        var linked = CancellationTokenSource.CreateLinkedTokenSource(cancellationToken, timeoutCts.Token);
        var context = new JobContext(def.Name, run.Id, startedAt, recorder, linked.Token);
        var opened = new Opened
        {
            Def = def,
            Options = options,
            Run = run,
            Recorded = recorded,
            Closing = closing,
            Recorder = recorder,
            Context = context,
            Timeout = timeoutCts,
            Linked = linked,
            Open = open,
        };
        if (givenUp)
        {
            // The write is the client's own task, observed by Spawn; it never throws.
            _ = begun.ContinueWith(
                t =>
                {
                    if (t.Status == TaskStatus.RanToCompletion && t.Result)
                    {
                        LandedLate(opened);
                    }
                    else
                    {
                        // The write failed in the end: nothing was left behind.
                        _open.TryRemove(new KeyValuePair<string, OpenRun>(run.Id, open));
                    }
                },
                CancellationToken.None,
                TaskContinuationOptions.ExecuteSynchronously,
                TaskScheduler.Default);
        }
        return opened;
    }

    /// <summary>
    /// A start write given up on at the deadline has landed. While the run's function is still
    /// going, its finish is written over the row as usual. Once the run has ended, there is no
    /// finish coming for the row: a run whose finish was recorded cannot get here (its row is
    /// there, so this insert was refused), so the finish failed to be recorded, and the run is
    /// finished and judged now; a run given back has its row deleted.
    /// </summary>
    private void LandedLate(Opened o)
    {
        var (ending, finished, finishedAt) = o.Landed();
        string where = "recording " + o.Def.Name;
        switch (ending)
        {
            case Opened.Ending.Finished when finished != null:
                SpawnReported(() => RecordFinishAsync(o.Def, finished, true, finishedAt), where);
                break;
            case Opened.Ending.GivenBack:
                SpawnReported(() => DiscardRunAsync(o.Run), where);
                break;
            default:
                // Still running: its finish finds the row.
                break;
        }
    }

    /// <summary>Closes a run as its function ended, answering the run as recorded.</summary>
    internal async Task<Run> CloseAsync(Opened o, object? value, Exception? thrown, CancellationToken cancellationToken)
    {
        o.EndFunction();
        if (!o.Claim())
        {
            // Closed already: the run as that close recorded it, or as it was opened when it was
            // given back.
            return o.Open.Recording is { } recorded ? await recorded.WaitAsync(cancellationToken).ConfigureAwait(false) : o.Run;
        }
        JobDef def = o.Def;
        Run run = o.Run;
        var discard = o.Options.DiscardWhen;
        if (discard != null && thrown != null && GivenBack(def.Name, discard, thrown) && await TakeBackNowAsync(o).ConfigureAwait(false))
        {
            return run;
        }
        Task? closing = o.Closing;
        if (o.Recorded && o.Options.TakesBack)
        {
            closing = Spawn(() => CloseOnStartAsync(def, run));
        }
        string? failure;
        string? resultText = null;
        if (thrown != null)
        {
            failure = ErrorText(thrown);
        }
        else
        {
            failure = HttpFailure.Of(value);
            resultText = value as string;
        }
        Task<Run> recording = Spawn(() => FinishExecutedAsync(o, resultText, failure, closing));
        o.Open.Recording = recording;
        _ = recording.ContinueWith(
            _ => _open.TryRemove(new KeyValuePair<string, OpenRun>(run.Id, o.Open)),
            CancellationToken.None,
            TaskContinuationOptions.ExecuteSynchronously,
            TaskScheduler.Default);
        try
        {
            // The caller may stop waiting; the recording completes on its own.
            return await recording.WaitAsync(cancellationToken).ConfigureAwait(false);
        }
        catch (OperationCanceledException) when (cancellationToken.IsCancellationRequested)
        {
            if (thrown == null)
            {
                throw;
            }
            return run;
        }
        catch (Exception e)
        {
            Report(e, "recording " + def.Name);
            return run;
        }
    }

    /// <summary>Takes a run back: deletes its running row, judging nothing.</summary>
    internal async Task<bool> TakeBackAsync(Opened o)
    {
        o.EndFunction();
        if (o.IsClosed)
        {
            return true;
        }
        if (!await TakeBackNowAsync(o).ConfigureAwait(false))
        {
            return false;
        }
        o.Claim();
        return true;
    }

    private async Task<bool> TakeBackNowAsync(Opened o)
    {
        bool back = true;
        bool written = o.StartWritten;
        if (written)
        {
            back = await Spawn(() => DiscardRunAsync(o.Run)).ConfigureAwait(false);
        }
        if (back && !written && o.Ended(Opened.Ending.GivenBack, null, 0))
        {
            // A start write given up on landed just now: its row goes with the run.
            back = await Spawn(() => DiscardRunAsync(o.Run)).ConfigureAwait(false);
        }
        if (back)
        {
            _open.TryRemove(new KeyValuePair<string, OpenRun>(o.Run.Id, o.Open));
        }
        return back;
    }

    private bool GivenBack(string name, Func<Exception, bool> discard, Exception thrown)
    {
        try
        {
            return discard(thrown);
        }
        catch (Exception e)
        {
            Report(e, "discarding " + name);
            return false;
        }
    }

    private async Task<bool> DiscardRunAsync(Run run)
    {
        string where = "discarding " + run.Job;
        if (_store is not IDeleteRunIfStore deleting)
        {
            Report("the store cannot take back a run (it has no DeleteRunIfAsync); recorded as it ended", where);
            return false;
        }
        bool deleted;
        try
        {
            deleted = await CallAsync(() => deleting.DeleteRunIfAsync(run.Id, run.Job, RunStatus.Running)).ConfigureAwait(false);
        }
        catch (Exception e)
        {
            Report(e, where);
            return false;
        }
        if (!deleted)
        {
            Report("run " + run.Id + " of " + run.Job + " is no longer running; left as it is", where);
        }
        return true;
    }

    private async Task<bool> BeginRunAsync(JobDef def, Run run, OpenRun open)
    {
        try
        {
            await SyncAsync(def, confirm: true).ConfigureAwait(false);
            await CallAsync(() => _store.InsertRunAsync(run)).ConfigureAwait(false);
            return true;
        }
        catch (Exception e)
        {
            if (!open.GivenUp)
            {
                Report(e, "recording " + def.Name);
            }
            return false;
        }
    }

    /// <summary>
    /// A run's start as the job's state takes it: missed closes (when the run catches the schedule
    /// up, <see cref="Evaluate.CatchesUp"/>; stuck stays open until a run finishes ok), and the
    /// run is held in <c>pendingRuns</c> (<see cref="Evaluate.HoldRun"/>) until its finish is evaluated.
    /// <paramref name="run"/> is the run as it started.
    /// </summary>
    internal async Task CloseOnStartAsync(JobDef def, Run run)
    {
        try
        {
            await UpdateStateAsync(def.Name, s => (Evaluate.HoldRun(Evaluate.OnRunStart(def.Stored, run, s, Now()), run.Id), true)).ConfigureAwait(false);
        }
        catch (Exception e)
        {
            Report(e, "starting " + def.Name);
        }
    }

    private async Task<Run> FinishExecutedAsync(Opened o, string? resultText, string? failure, Task? closing)
    {
        JobDef def = o.Def;
        Run run = o.Run;
        Recorder recorder = o.Recorder;
        string name = def.Name;
        long finishedAt = Now();
        string? output = recorder.Output();
        if (output == null && failure == null && resultText != null)
        {
            // Capped by Conclude, after it is redacted.
            output = resultText;
        }
        string? expectText = recorder.ExpectText();
        if (expectText == null && failure == null)
        {
            expectText = resultText;
        }
        Run finished = run with
        {
            FinishedAt = finishedAt,
            DurationMs = Evaluate.RunDuration(run.StartedAt, finishedAt),
            Output = output,
            Metrics = recorder.Metrics(),
        };
        finished = ConcludeGuarded(def, finished, failure, expectText, timedOut: false);
        if (closing != null)
        {
            try
            {
                await closing.ConfigureAwait(false);
            }
            catch (Exception e)
            {
                Report(e, "starting " + name);
            }
        }
        // In the store when written in time, or when given up on at the deadline and landed since.
        bool written = o.StartWritten;
        bool lost = false;
        try
        {
            string? why = await RecordFinishAsync(def, finished, written, finishedAt).ConfigureAwait(false);
            if (why != null)
            {
                Report("run " + finished.Id + " of " + name + " " + why + "; ignored", "finishing " + name);
            }
        }
        catch (Exception e)
        {
            lost = true;
            Report(e, "recording " + name);
        }
        // From here a start write that lands late finishes the run itself (LandedLate). One that
        // landed while this finish was failing is finished now, so its row is not left running.
        if (o.Ended(Opened.Ending.Finished, finished, finishedAt) && !written && lost)
        {
            try
            {
                await RecordFinishAsync(def, finished, true, finishedAt).ConfigureAwait(false);
            }
            catch (Exception e)
            {
                Report(e, "recording " + name);
            }
        }
        return finished;
    }

    /// <summary>
    /// <see cref="Conclude"/>, and when the app's own redaction or expect check throws anyway, the
    /// run failed with that throw, written through the default redaction.
    /// </summary>
    internal Run ConcludeGuarded(JobDef def, Run run, string? failure, string? expectText, bool timedOut)
    {
        Run concluded;
        try
        {
            concluded = Conclude(def, run, failure, expectText, timedOut);
        }
        catch (Exception e)
        {
            concluded = run with
            {
                Status = timedOut ? RunStatus.Timeout : RunStatus.Failed,
                Error = OutputText.RedactAndCap(ErrorText(e), OutputText.RedactSecrets),
                Output = run.Output == null ? null : OutputText.RedactAndCap(run.Output, OutputText.RedactSecrets),
            };
        }
        CronwatchTelemetry.Add(CronwatchTelemetry.Runs, new("cronwatch.job", def.Name), new("cronwatch.status", concluded.Status.Value));
        if (concluded.DurationMs is long ms)
        {
            CronwatchTelemetry.RecordDuration(ms, new("cronwatch.job", def.Name));
        }
        return concluded;
    }

    internal Run Conclude(JobDef def, Run run, string? failure, string? expectText, bool timedOut)
    {
        RunStatus status;
        string? error;
        if (timedOut)
        {
            status = RunStatus.Timeout;
            error = failure;
        }
        else if (failure != null)
        {
            status = RunStatus.Failed;
            error = failure;
        }
        else
        {
            string? unmet = Expect.CheckExpectation(def.Expect, expectText);
            status = unmet == null ? RunStatus.Ok : RunStatus.Failed;
            error = unmet;
        }
        // Redacted after the expect check, so a rule can still match what was logged, and before
        // the cap, so the cut cannot keep half a secret. NULs go last, so not even a custom
        // redact can store one.
        return run with
        {
            Status = status,
            Error = error == null ? null : OutputText.RedactAndCap(error, Redact),
            Output = run.Output == null ? null : OutputText.RedactAndCap(run.Output, Redact),
        };
    }

    /// <summary>Writes a finished run and judges it when this process's write landed; answers why it was ignored, or null.</summary>
    internal async Task<string?> RecordFinishAsync(JobDef def, Run run, bool recorded, long finishedAt)
    {
        if (!recorded)
        {
            // The start was never written; the store may be back by now.
            await SyncAsync(def, confirm: false, running: true).ConfigureAwait(false);
            try
            {
                await CallAsync(() => _store.InsertRunAsync(run)).ConfigureAwait(false);
                await FinishRunAsync(def.Stored, run, finishedAt).ConfigureAwait(false);
                return null;
            }
            catch (CronwatchException)
            {
                // Another process may have recorded a run with this id meanwhile.
                Run? stored;
                try
                {
                    stored = await CallAsync(() => _store.GetRunAsync(run.Id)).ConfigureAwait(false);
                }
                catch (Exception)
                {
                    // The insert's failure is the one thrown, as the SDK throws it.
                    stored = null;
                }
                if (stored == null)
                {
                    throw;
                }
                if (stored.Job != run.Job)
                {
                    return "belongs to job " + JsonText.Quote(stored.Job);
                }
            }
        }
        JobState state = await ReadStateAsync(run.Job).ConfigureAwait(false);
        bool tracked = state.PendingRuns?.Contains(run.Id) ?? false;
        var (late, ignored) = await ClaimFinishAsync(run).ConfigureAwait(false);
        if (ignored != null)
        {
            return ignored;
        }
        if (!late || run.Status == RunStatus.Ok)
        {
            await FinishRunAsync(def.Stored, run, finishedAt, tracked && !late).ConfigureAwait(false);
        }
        return null;
    }

    /// <summary>
    /// Writes the finish over a row still running, else over one a check marked timed out
    /// (<c>late</c>), else answers why it was ignored.
    /// </summary>
    internal async Task<(bool Late, string? Ignored)> ClaimFinishAsync(Run run)
    {
        if (await WriteRunIfAsync(run, OnlyRunning).ConfigureAwait(false))
        {
            return (false, null);
        }
        if (await WriteRunIfAsync(run, OnlyTimeout).ConfigureAwait(false))
        {
            return (true, null);
        }
        Run? stored = await CallAsync(() => _store.GetRunAsync(run.Id)).ConfigureAwait(false);
        return (false, stored == null ? "was not found" : "was already finished as " + stored.Status.Value);
    }

    /// <summary>Judges a finished run and sends what it raised.</summary>
    internal async Task<IReadOnlyList<Alert>> FinishRunAsync(Definition def, Run run, long now, bool pendingOnly = false)
    {
        Held held;
        try
        {
            var result = await UpdateStateAsync<IReadOnlyList<Run>, Held>(
                run.Job,
                () => HistoryAsync(run),
                (previous, history) =>
                {
                    if (pendingOnly && (previous.PendingRuns == null || !previous.PendingRuns.Contains(run.Id)))
                    {
                        return (previous, new Held([], 0));
                    }
                    // The run's evaluation is written here, so it leaves pendingRuns in the same write.
                    JobState before = Evaluate.ReleaseRun(previous, run.Id);
                    return Outbox(Evaluate.ApplySilence(before, Evaluate.OnRunFinish(def, run, before, history, now), now), def, now);
                }).ConfigureAwait(false);
            held = result.Result;
        }
        catch (Exception e)
        {
            Report(e, "evaluating " + run.Job);
            return [];
        }
        ReportDropped(run.Job, held.Dropped);
        return await DispatchAsync(run.Job, held.Alerts, now).ConfigureAwait(false);
    }

    private async Task<IReadOnlyList<Run>> HistoryAsync(Run run)
    {
        var page = await _store.ListRunsAsync(run.Job, HistoryPage).ConfigureAwait(false);
        var runs = Without(page, run);
        bool full = page.Count == HistoryPage;
        int ok = 0;
        foreach (var r in runs)
        {
            if (r.Status == RunStatus.Ok)
            {
                ok++;
            }
        }
        if (full && ok < Evaluate.BaselineWindow)
        {
            runs = Without(await _store.ListRunsAsync(run.Job, HistoryMax).ConfigureAwait(false), run);
        }
        return runs;
    }

    private static List<Run> Without(IReadOnlyList<Run> runs, Run run)
    {
        var output = new List<Run>(runs.Count);
        foreach (var r in runs)
        {
            if (r.Id != run.Id)
            {
                output.Add(r);
            }
        }
        return output;
    }

    private async Task<IReadOnlyList<Alert>> RecordRunNowAsync(Run input, bool evaluate)
    {
        JobDef def = Declared(input.Job)
            ?? throw CronwatchException.Invalid("recordRun: job " + JsonText.Quote(input.Job) + " is not declared; call Job() first");
        // The longest id a run's start takes; MySQL's column would hold 255, but every store holds 200.
        if (input.Id.Length == 0 || input.Id.Length > MaxRunId)
        {
            throw CronwatchException.Invalid(
                "recordRun: run ids must be 1 to " + MaxRunId + " characters (got " + input.Id.Length + " characters; job " + JsonText.Quote(input.Job) + ")");
        }
        if (input.Id.Contains('\0', StringComparison.Ordinal))
        {
            throw CronwatchException.Invalid("recordRun: run ids cannot contain a NUL character (job " + JsonText.Quote(input.Job) + ")");
        }
        // The fields evaluation reads that C#'s types still let be wrong: a status is any string,
        // and a trigger may be a null given past the compiler.
        string where = "job " + JsonText.Quote(input.Job) + ", run " + JsonText.Quote(input.Id);
        if (input.Status != RunStatus.Running && input.Status != RunStatus.Ok && input.Status != RunStatus.Failed && input.Status != RunStatus.Timeout)
        {
            throw CronwatchException.Invalid("recordRun: status must be \"running\", \"ok\", \"failed\", or \"timeout\" (" + where + ")");
        }
        if (input.Trigger is null)
        {
            throw CronwatchException.Invalid("recordRun: trigger must be a string (" + where + ")");
        }
        // Refused as a job's Metric refuses them: a store keeps NaN and infinity as null.
        foreach (var metric in input.Metrics)
        {
            if (!double.IsFinite(metric.Value))
            {
                throw CronwatchException.Invalid(
                    "recordRun: metric " + JsonText.Quote(metric.Key) + " must be a finite number (job " + JsonText.Quote(input.Job) + ", run " + JsonText.Quote(input.Id) + ")");
            }
        }
        await SyncAsync(def).ConfigureAwait(false);
        Run run = input;
        if (run.Status == RunStatus.Ok)
        {
            string? unmet = Expect.CheckExpectation(def.Expect, run.Output);
            if (unmet != null)
            {
                run = run with { Status = RunStatus.Failed, Error = unmet };
            }
        }
        run = run with
        {
            Output = run.Output == null ? null : OutputText.RedactAndCap(run.Output, Redact),
            Error = run.Error == null ? null : OutputText.RedactAndCap(run.Error, Redact),
        };
        Run toWrite = run;
        Run? stored = await CallAsync(() => _store.GetRunAsync(toWrite.Id)).ConfigureAwait(false);
        if (stored != null)
        {
            return await RecordOverAsync(def.Stored, stored, run, evaluate).ConfigureAwait(false);
        }
        // Keep an evaluated import retryable until its pending marker is durable. A finished
        // import's placeholder has FinishedAt set to when it was written, so a check can tell one
        // whose finish never landed (Evaluate.IsStranded); the finish writes the run's own. One
        // still running keeps FinishedAt null and times out by the job's timeout.
        Run inserted = evaluate
            ? run with { Status = RunStatus.Running, FinishedAt = run.Status == RunStatus.Running ? null : Now() }
            : run;
        try
        {
            await CallAsync(() => _store.InsertRunAsync(inserted)).ConfigureAwait(false);
        }
        catch (CronwatchException)
        {
            // Another process recorded it first.
            Run? again;
            try
            {
                again = await CallAsync(() => _store.GetRunAsync(toWrite.Id)).ConfigureAwait(false);
            }
            catch (Exception)
            {
                // The insert's failure is the one thrown, as the SDK throws it.
                again = null;
            }
            if (again == null)
            {
                throw;
            }
            return await RecordOverAsync(def.Stored, again, run, evaluate).ConfigureAwait(false);
        }
        if (!evaluate)
        {
            return [];
        }
        return await RecordOverAsync(def.Stored, inserted, run, true).ConfigureAwait(false);
    }

    // Read the row again on every CAS attempt, so a finished evaluation's marker stays consumed.
    private async Task HoldImportedRunAsync(Definition def, Run run)
    {
        using var turn = await Queues.EnterAsync(run.Job).ConfigureAwait(false);
        for (int attempt = 1; ; attempt++)
        {
            JobState before = await ReadStateAsync(run.Job).ConfigureAwait(false);
            Run? current = await CallAsync(() => _store.GetRunAsync(run.Id)).ConfigureAwait(false);
            if (current == null || current.Job != run.Job || current.Status != RunStatus.Running || (before.PendingRuns?.Contains(run.Id) ?? false))
            {
                return;
            }
            JobState next = Evaluated(() => Evaluate.HoldRun(Evaluate.OnRunStart(def, run, before, Now()), run.Id));
            long version = before.CountedVersion;
            if (await WriteStateAsync(next with { Version = version + 1 }, version).ConfigureAwait(false))
            {
                return;
            }
            if (attempt >= StateAttempts)
            {
                throw new CronwatchException("the state of " + run.Job + " changed under " + StateAttempts + " attempts in a row to update it; gave up");
            }
        }
    }

    private async Task<IReadOnlyList<Alert>> RecordOverAsync(Definition def, Run stored, Run run, bool evaluate)
    {
        string where = "recording " + run.Job;
        if (stored.Job != run.Job)
        {
            Report("run " + run.Id + " of " + run.Job + " belongs to job " + JsonText.Quote(stored.Job) + "; ignored", where);
            return [];
        }
        bool open = stored.Status == RunStatus.Running || stored.Status == RunStatus.Timeout;
        if (!open)
        {
            return [];
        }
        if (evaluate && stored.Status == RunStatus.Running)
        {
            await HoldImportedRunAsync(def, run).ConfigureAwait(false);
        }
        if (run.Status == RunStatus.Running)
        {
            return [];
        }
        JobState state = await ReadStateAsync(run.Job).ConfigureAwait(false);
        bool tracked = state.PendingRuns?.Contains(run.Id) ?? false;
        var (late, ignored) = await ClaimFinishAsync(run).ConfigureAwait(false);
        if (ignored != null)
        {
            Report("run " + run.Id + " of " + run.Job + " " + ignored + "; ignored", where);
            return [];
        }
        if (!evaluate)
        {
            // Stored unjudged on purpose: a check must not evaluate it as one whose evaluation was lost.
            try
            {
                await UpdateStateAsync(run.Job, s => (Evaluate.ReleaseRun(s, run.Id), true)).ConfigureAwait(false);
            }
            catch (Exception e)
            {
                Report(e, where);
            }
            return [];
        }
        if (late && run.Status != RunStatus.Ok)
        {
            return [];
        }
        return await FinishRunAsync(def, run, Now(), tracked && !late).ConfigureAwait(false);
    }

    /// <summary>
    /// Records the runs still open in this process failed (a function's whose recording is under
    /// way is waited for instead), within the shutdown's five seconds, all at once.
    /// </summary>
    private async Task RecordOpenRunsAsync()
    {
        var work = new List<Task>();
        foreach (var o in _open.Values)
        {
            if (o.Recording is { } recording)
            {
                work.Add(recording);
                continue;
            }
            work.Add(Spawn(() => FailOpenAsync(o)));
        }
        try
        {
            await Task.WhenAll(work).WaitAsync(Timings.Shutdown, _time).ConfigureAwait(false);
        }
        catch (TimeoutException)
        {
            // What is left is reported stuck at its timeout, as a killed process's run is.
        }
        catch (Exception e)
        {
            Report(e, "shutdown");
        }
    }

    private async Task FailOpenAsync(OpenRun o)
    {
        // A run opening as the process stops: its row is waited for, and one never written is
        // left alone.
        if (!await o.Begun.ConfigureAwait(false))
        {
            return;
        }
        // The hook and a disposal during the shutdown may both get here: one records the run.
        if (!_open.TryRemove(new KeyValuePair<string, OpenRun>(o.Run.Id, o)))
        {
            return;
        }
        string name = o.Def.Name;
        long now = Now();
        Run run = o.Run;
        string? output = o.Recorder.Output();
        Run failed = run with
        {
            Status = RunStatus.Failed,
            FinishedAt = now,
            DurationMs = Evaluate.RunDuration(run.StartedAt, now),
            Error = OutputText.RedactAndCap(ShutdownError, Redact),
            Output = output == null ? null : OutputText.RedactAndCap(output, Redact),
            Metrics = o.Recorder.Metrics(),
        };
        try
        {
            if (await WriteRunIfAsync(failed, OnlyRunning).ConfigureAwait(false))
            {
                await FinishRunAsync(o.Def.Stored, failed, now).ConfigureAwait(false);
            }
        }
        catch (Exception e)
        {
            Report(e, "recording " + name);
        }
    }
}
