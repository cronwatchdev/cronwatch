using System;
using System.Collections.Generic;
using System.IO;
using System.Linq;
using System.Threading;
using System.Threading.Tasks;
using Cronwatch.Internal;
using Cronwatch.Tests.Triage;
using Cronwatch.Triage;
using Xunit;
using static Cronwatch.Tests.Support;

namespace Cronwatch.Tests;

/// <summary>
/// The SDK's tests of four behaviours every port shares, ported: stuck stays open until a run
/// finishes ok, a run's start is given five seconds of the store, a stranded import that holds an
/// error is closed as failed, and triage that runs out of tokens before it writes says so.
/// </summary>
public class ApprovedChangesTests
{
    private const long T0 = 1_750_000_000_000L;

    private static Run At(string status, long startedAt) =>
        Run.Running("r-" + startedAt, "j", startedAt, "run") with
        {
            Status = new RunStatus(status),
            FinishedAt = status == "running" ? null : startedAt,
            DurationMs = status == "running" ? null : 0,
        };

    private static JobState StartNow(Definition d, JobState state, long at) => Evaluate.OnRunStart(d, At("running", at), state, at);

    private static List<string> Types(Evaluation e) => e.Alerts.Select(a => a.Type.Value).ToList();

    [Fact]
    public void Stuck_stays_open_through_later_starts_failures_and_timeouts_and_closes_with_a_recovery_on_the_next_success()
    {
        Definition d = Definition.Own(new JsObject().Set("name", "j"));
        Evaluation stuck = Evaluate.OnRunFinish(d, At("timeout", T0 - Hour), Evaluate.EmptyState("j"), [], T0);
        Assert.Equal(["stuck"], Types(stuck));
        // The next run starting closes nothing: it may hang as the last one did.
        JobState started = StartNow(d, stuck.State, T0 + Min);
        Assert.Equal(T0, started.OpenAt(Condition.Stuck));
        Assert.True(started.PendingRecovery is not { Count: > 0 });
        // It times out too: stuck is already open, so there is no second alert.
        Evaluation again = Evaluate.OnRunFinish(d, At("timeout", T0 + Min), started, [], T0 + Hour);
        Assert.Empty(again.Alerts);
        Assert.Equal(T0, again.State.OpenAt(Condition.Stuck));
        Assert.Equal(2, again.State.ConsecutiveFailures);
        // A run that fails opens failed by its own rule and leaves stuck open.
        Evaluation failed = Evaluate.OnRunFinish(d, At("failed", T0 + Hour), StartNow(d, again.State, T0 + Hour), [], T0 + Hour + 1000);
        Assert.Equal(["failed"], Types(failed));
        Assert.Equal(T0, failed.State.OpenAt(Condition.Stuck));
        // The first success closes both, with one recovery naming both.
        Evaluation ok = Evaluate.OnRunFinish(d, At("ok", T0 + (2 * Hour)), StartNow(d, failed.State, T0 + (2 * Hour)), [], T0 + (2 * Hour) + 1000);
        Assert.Equal(["recovered"], Types(ok));
        Assert.Equal(
            ["failed", "stuck"],
            Assert.IsType<AlertDetails.Recovered>(ok.Alerts[0].Details).After.Select(c => c.Value).Order(StringComparer.Ordinal));
    }

    [Fact]
    public async Task A_job_that_hangs_on_every_run_alerts_stuck_once_and_recovers_on_its_first_success()
    {
        await using var m = Make();
        Job job = m.Cw.Job("hangs", new JobOptions { Schedule = "*/5 * * * *", Timezone = "UTC", Timeout = "10m" });
        // Twelve runs, five minutes apart, none of which ever finishes.
        var handles = new List<RunHandle>();
        for (int i = 0; i < 12; i++)
        {
            handles.Add(await job.StartAsync());
            m.Clock.Advance(5 * Min);
            await m.Cw.CheckAsync();
        }
        Assert.Equal(["stuck"], m.Alerts.Types());
        Assert.Equal(JobHealth.Stuck, (await m.Cw.JobSummaryAsync("hangs"))!.Health);
        // A run that fails alerts as failed by its own rule; stuck stays open beside it.
        await Quietly(() => job.RunAsync((j, ct) => throw new InvalidOperationException("boom")));
        Assert.Equal(["stuck", "failed"], m.Alerts.Types());
        Assert.Equal(["failed", "stuck"], (await m.Cw.JobSummaryAsync("hangs"))!.Open.Select(c => c.Value).Order(StringComparer.Ordinal));
        // A hung run that reports failing late is not counted again and alerts nothing.
        await handles[0].FailAsync(new InvalidOperationException("late"));
        Assert.Equal(["stuck", "failed"], m.Alerts.Types());
        // The first success closes both with one recovery.
        await job.RunAsync((j, ct) => Task.CompletedTask);
        Assert.Equal(["stuck", "failed", "recovered"], m.Alerts.Types());
        Assert.Equal(
            ["failed", "stuck"],
            Assert.IsType<AlertDetails.Recovered>(m.Alerts.Alerts.ElementAt(2).Details).After.Select(c => c.Value).Order(StringComparer.Ordinal));
        Assert.Empty((await m.Cw.JobSummaryAsync("hangs"))!.Open);
    }

    [Fact]
    public async Task A_stuck_run_that_finishes_ok_late_closes_stuck_with_a_recovery_though_other_runs_started_meanwhile()
    {
        await using var m = Make();
        Job job = m.Cw.Job("slow-queue", new JobOptions { Timeout = "10m" });
        RunHandle first = await job.StartAsync();
        m.Clock.Advance(11 * Min);
        await m.Cw.CheckAsync();
        Assert.Equal(["stuck"], m.Alerts.Types());
        RunHandle second = await job.StartAsync();
        // A start leaves stuck open.
        Assert.Equal(["stuck"], (await m.Cw.JobSummaryAsync("slow-queue"))!.Open.Select(c => c.Value));
        await first.FinishAsync();
        Assert.Equal(["stuck", "recovered"], m.Alerts.Types());
        await second.FinishAsync();
        Assert.Equal(["stuck", "recovered"], m.Alerts.Types());
    }

    /// <summary>A memory store whose first run insert (a run's start) waits to be released, and whose later ones may be down.</summary>
    private sealed class Hanging : IStore, IUpdateRunIfStore, ICompareAndSetStateStore
    {
        private readonly TaskCompletionSource _gate = new(TaskCreationOptions.RunContinuationsAsynchronously);
        private int _inserts;

        public MemoryStore Inner { get; } = new();

        /// <summary>Completed when the first insert is waiting.</summary>
        public TaskCompletionSource Waiting { get; } = new(TaskCreationOptions.RunContinuationsAsynchronously);

        /// <summary>Whether every insert after the first finds the store down.</summary>
        public bool DownAfterFirst { get; init; }

        public void Release() => _gate.TrySetResult();

        public Task InitAsync(CancellationToken cancellationToken = default) => Inner.InitAsync(cancellationToken);

        public Task UpsertJobAsync(Definition definition, long now, CancellationToken cancellationToken = default) => Inner.UpsertJobAsync(definition, now, cancellationToken);

        public Task<StoredJob?> GetJobAsync(string name, CancellationToken cancellationToken = default) => Inner.GetJobAsync(name, cancellationToken);

        public Task<IReadOnlyList<StoredJob>> ListJobsAsync(CancellationToken cancellationToken = default) => Inner.ListJobsAsync(cancellationToken);

        public Task DeleteJobAsync(string name, CancellationToken cancellationToken = default) => Inner.DeleteJobAsync(name, cancellationToken);

        public async Task InsertRunAsync(Run run, CancellationToken cancellationToken = default)
        {
            if (Interlocked.Increment(ref _inserts) == 1)
            {
                Waiting.TrySetResult();
                await _gate.Task;
            }
            else if (DownAfterFirst)
            {
                throw new IOException("store down");
            }
            await Inner.InsertRunAsync(run, cancellationToken);
        }

        public Task UpdateRunAsync(Run run, CancellationToken cancellationToken = default) => Inner.UpdateRunAsync(run, cancellationToken);

        public Task<bool> UpdateRunIfAsync(Run run, IReadOnlyList<RunStatus> from, CancellationToken cancellationToken = default) => Inner.UpdateRunIfAsync(run, from, cancellationToken);

        public Task<Run?> GetRunAsync(string id, CancellationToken cancellationToken = default) => Inner.GetRunAsync(id, cancellationToken);

        public Task<IReadOnlyList<Run>> ListRunsAsync(string job, int limit, CancellationToken cancellationToken = default) => Inner.ListRunsAsync(job, limit, cancellationToken);

        public Task<Run?> LastRunAsync(string job, CancellationToken cancellationToken = default) => Inner.LastRunAsync(job, cancellationToken);

        public Task<IReadOnlyList<Run>> RunningRunsAsync(CancellationToken cancellationToken = default) => Inner.RunningRunsAsync(cancellationToken);

        public Task<JobState?> GetStateAsync(string job, CancellationToken cancellationToken = default) => Inner.GetStateAsync(job, cancellationToken);

        public Task SetStateAsync(JobState state, CancellationToken cancellationToken = default) => Inner.SetStateAsync(state, cancellationToken);

        public Task<bool> CompareAndSetStateAsync(JobState state, long expected, CancellationToken cancellationToken = default) => Inner.CompareAndSetStateAsync(state, expected, cancellationToken);

        public Task<long> PruneAsync(long before, CancellationToken cancellationToken = default) => Inner.PruneAsync(before, cancellationToken);
    }

    private static List<string> Reported(Made m) => m.Errors.Entries.Select(e => e.Where + ": " + e.Error.Message).ToList();

    [Fact]
    public async Task A_store_that_hangs_while_recording_a_runs_start_does_not_hold_the_job_back_and_leaves_no_running_row()
    {
        var store = new Hanging();
        await using var m = Make(store: store);
        Job job = m.Cw.Job("nightly");
        var ran = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
        var proceed = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
        Task<string> running = job.RunAsync(async (j, ct) =>
        {
            ran.TrySetResult();
            j.Log("did the work");
            await proceed.Task;
            return "done";
        });
        await store.Waiting.Task.WaitAsync(TimeSpan.FromSeconds(30));
        // Still waiting on the store, a millisecond short of the deadline (on the client's clock).
        m.Clock.Advance(4_999);
        await Task.Delay(100);
        Assert.False(ran.Task.IsCompleted, "the job ran before the deadline");
        m.Clock.Advance(1);
        await ran.Task.WaitAsync(TimeSpan.FromSeconds(30));
        Assert.Equal(["recording nightly: the store took longer than 5s to record the start of nightly; running it unrecorded"], Reported(m));
        // The late start lands while the job is still going, and the finish is written over it.
        store.Release();
        await Eventually("the late start", async () => (await store.Inner.RunningRunsAsync()).Count == 1);
        proceed.SetResult();
        Assert.Equal("done", await running);
        Run only = Assert.Single(await m.Cw.RunsAsync("nightly"));
        Assert.Equal(RunStatus.Ok, only.Status);
        Assert.Equal("did the work", only.Output);
        Assert.Empty(await store.Inner.RunningRunsAsync());
        Assert.Single(m.Errors.Entries);
        Assert.Empty(m.Alerts.Types());
    }

    [Fact]
    public async Task A_start_write_that_lands_after_the_finish_was_recorded_changes_nothing_and_reports_nothing_more()
    {
        var store = new Hanging();
        await using var m = Make(store: store);
        Task<string> running = m.Cw.Job("nightly").RunAsync((j, ct) => Task.FromResult("done"));
        await store.Waiting.Task.WaitAsync(TimeSpan.FromSeconds(30));
        m.Clock.Advance(5_000);
        // The finish does not wait on the hung write: it writes the run whole.
        Assert.Equal("done", await running);
        Assert.Equal(RunStatus.Ok, Assert.Single(await m.Cw.RunsAsync("nightly")).Status);
        // The start write lands at last and is refused, the run being there: nothing changes.
        store.Release();
        await m.Cw.DisposeAsync();
        Run only = Assert.Single(await store.Inner.ListRunsAsync("nightly", 10));
        Assert.Equal(RunStatus.Ok, only.Status);
        Assert.Empty(await store.Inner.RunningRunsAsync());
        Assert.Single(m.Errors.Entries);
        Assert.Empty(m.Alerts.Types());
    }

    [Fact]
    public async Task A_start_write_that_lands_after_the_finish_failed_is_finished_then_not_left_running()
    {
        var store = new Hanging { DownAfterFirst = true };
        await using var m = Make(store: store);
        Task running = m.Cw.Job("nightly").RunAsync((j, ct) => throw new InvalidOperationException("boom"));
        await store.Waiting.Task.WaitAsync(TimeSpan.FromSeconds(30));
        m.Clock.Advance(5_000);
        // The job's own error is what RunAsync throws.
        Assert.Equal("boom", (await Assert.ThrowsAsync<InvalidOperationException>(() => running)).Message);
        Assert.Equal(["recording nightly", "recording nightly"], m.Errors.Wheres());
        Assert.Empty(await m.Cw.RunsAsync("nightly"));
        // The start write lands at last, after the finish gave up: the run is finished and judged now.
        store.Release();
        await Eventually("the late finish", () => m.Alerts.Types().Count == 1);
        Run only = Assert.Single(await m.Cw.RunsAsync("nightly"));
        Assert.Equal(RunStatus.Failed, only.Status);
        Assert.Contains("boom", only.Error, StringComparison.Ordinal);
        Assert.Empty(await store.Inner.RunningRunsAsync());
        Assert.Equal(["failed"], m.Alerts.Types());
        Assert.Equal(2, m.Errors.Entries.Count);
    }

    private static TriageContext Context() =>
        new(
            new Alert
            {
                Type = AlertType.Failed,
                Details = new AlertDetails.Failure(1, 1),
                Job = "nightly",
                Title = "nightly failed",
                Message = "boom",
                At = T0,
                Definition = Definition.Own(new JsObject().Set("name", "nightly")),
            },
            []);

    private static AnthropicTriage Answering(string body, long? maxTokens = null) =>
        new(
            maxTokens is long limit
                ? new AnthropicTriageOptions { ApiKey = "k", BaseUrl = "https://api.anthropic.com", MaxTokens = limit, Transport = new TriageConformanceTests.Recorder(200, body) }
                : new AnthropicTriageOptions { ApiKey = "k", BaseUrl = "https://api.anthropic.com", Transport = new TriageConformanceTests.Recorder(200, body) },
            TriageConformanceTests.NoEnvironment);

    [Fact]
    public async Task Triage_that_reaches_max_tokens_before_any_text_is_an_error_naming_the_limit()
    {
        Assert.Equal(4096, AnthropicTriage.DefaultMaxTokens);
        Assert.Equal(4096, new AnthropicTriageOptions().MaxTokens);
        const string Empty = "{\"stop_reason\":\"max_tokens\",\"content\":[]}";
        var byDefault = await Assert.ThrowsAsync<CronwatchException>(() => Answering(Empty).TriageAsync(Context(), CancellationToken.None));
        Assert.Equal("triage: the model reached max_tokens (4096) before writing a diagnosis; raise maxTokens or lower effort", byDefault.Message);
        var configured = await Assert.ThrowsAsync<CronwatchException>(() => Answering(Empty, 1000).TriageAsync(Context(), CancellationToken.None));
        Assert.Equal("triage: the model reached max_tokens (1000) before writing a diagnosis; raise maxTokens or lower effort", configured.Message);
        // Thinking alone, and no text after it, is the same.
        const string Thinking = "{\"stop_reason\":\"max_tokens\",\"content\":[{\"type\":\"thinking\",\"thinking\":\"hmm\"}]}";
        await Assert.ThrowsAsync<CronwatchException>(() => Answering(Thinking).TriageAsync(Context(), CancellationToken.None));
        // Text that was cut short is still returned; a refusal and an empty end of turn are nothing, without an error.
        Assert.Equal(
            "The disk",
            await Answering("{\"stop_reason\":\"max_tokens\",\"content\":[{\"type\":\"text\",\"text\":\"The disk\"}]}").TriageAsync(Context(), CancellationToken.None));
        Assert.Null(await Answering("{\"stop_reason\":\"refusal\",\"content\":[]}").TriageAsync(Context(), CancellationToken.None));
        Assert.Null(await Answering("{\"stop_reason\":\"end_turn\",\"content\":[]}").TriageAsync(Context(), CancellationToken.None));
    }

    [Fact]
    public async Task A_client_reports_triage_that_ran_out_of_tokens_and_sends_the_alert_without_a_diagnosis()
    {
        var triage = Answering("{\"stop_reason\":\"max_tokens\",\"content\":[]}");
        await using var m = Make(triage: triage);
        await Quietly(() => m.Cw.RunAsync("nightly", (j, ct) => throw new InvalidOperationException("boom")));
        Alert alert = Assert.Single(m.Alerts.Alerts);
        Assert.Null(alert.Triage);
        Assert.Equal(
            ["triage for nightly: triage: the model reached max_tokens (4096) before writing a diagnosis; raise maxTokens or lower effort"],
            Reported(m));
    }
}
