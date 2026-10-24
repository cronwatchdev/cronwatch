using System;
using System.Collections.Concurrent;
using System.Collections.Generic;
using System.Linq;
using System.Threading;
using System.Threading.Tasks;
using Cronwatch.Internal;
using Xunit;
using static Cronwatch.Tests.Support;

namespace Cronwatch.Tests;

/// <summary>
/// The SDK's <c>outbox.test.ts</c>, ported: an alert is written with the state that opens its
/// condition, so a process that dies before sending it does not lose it, and while it is being
/// sent no check anywhere sends it too.
/// </summary>
public class OutboxTests
{
    [Fact]
    public async Task A_finished_import_retries_after_its_pending_marker_write_fails()
    {
        var store = new Wrapped();
        await using var k = Make(store: store);
        k.Cw.Job("imported");
        var run = Run.Running("external", "imported", T0, "x") with { Status = RunStatus.Failed, FinishedAt = T0, DurationMs = 0, Error = "disk full" };
        store.Break("compareAndSetState");
        await Assert.ThrowsAsync<CronwatchException>(() => k.Cw.RecordRunAsync(run));
        Assert.Equal(RunStatus.Running, (await store.Inner.GetRunAsync(run.Id))!.Status);
        store.Mend("compareAndSetState");
        await k.Cw.RecordRunAsync(run);
        await k.Cw.RecordRunAsync(run);
        Assert.Equal(["failed"], k.Alerts.Types());
        Assert.Equal(RunStatus.Failed, (await store.Inner.GetRunAsync(run.Id))!.Status);
        Assert.Null((await store.Inner.GetStateAsync(run.Job))!.PendingRuns);
    }

    private const long Min = 60_000;

    /// <summary>
    /// A store for a process that is about to die: once <see cref="Kill"/> is called, nothing it
    /// asks of the store ever completes, as when the process is gone. It counts the state writes.
    /// </summary>
    private sealed class Mortal(MemoryStore inner) : IStore, IUpdateRunIfStore, ICompareAndSetStateStore
    {
        private volatile bool _dead;
        private int _stateWrites;

        public int StateWrites => Volatile.Read(ref _stateWrites);

        public void Kill() => _dead = true;

        private Task<T> Call<T>(Func<Task<T>> call) => _dead ? new TaskCompletionSource<T>().Task : call();

        private Task Call(Func<Task> call) => _dead ? new TaskCompletionSource().Task : call();

        public Task InitAsync(CancellationToken cancellationToken = default) => Call(() => inner.InitAsync(cancellationToken));

        public Task UpsertJobAsync(Definition definition, long now, CancellationToken cancellationToken = default) =>
            Call(() => inner.UpsertJobAsync(definition, now, cancellationToken));

        public Task<StoredJob?> GetJobAsync(string name, CancellationToken cancellationToken = default) => Call(() => inner.GetJobAsync(name, cancellationToken));

        public Task<IReadOnlyList<StoredJob>> ListJobsAsync(CancellationToken cancellationToken = default) => Call(() => inner.ListJobsAsync(cancellationToken));

        public Task DeleteJobAsync(string name, CancellationToken cancellationToken = default) => Call(() => inner.DeleteJobAsync(name, cancellationToken));

        public Task InsertRunAsync(Run run, CancellationToken cancellationToken = default) => Call(() => inner.InsertRunAsync(run, cancellationToken));

        public Task UpdateRunAsync(Run run, CancellationToken cancellationToken = default) => Call(() => inner.UpdateRunAsync(run, cancellationToken));

        public Task<Run?> GetRunAsync(string id, CancellationToken cancellationToken = default) => Call(() => inner.GetRunAsync(id, cancellationToken));

        public Task<IReadOnlyList<Run>> ListRunsAsync(string job, int limit, CancellationToken cancellationToken = default) =>
            Call(() => inner.ListRunsAsync(job, limit, cancellationToken));

        public Task<Run?> LastRunAsync(string job, CancellationToken cancellationToken = default) => Call(() => inner.LastRunAsync(job, cancellationToken));

        public Task<IReadOnlyList<Run>> RunningRunsAsync(CancellationToken cancellationToken = default) => Call(() => inner.RunningRunsAsync(cancellationToken));

        public Task<JobState?> GetStateAsync(string job, CancellationToken cancellationToken = default) => Call(() => inner.GetStateAsync(job, cancellationToken));

        public Task SetStateAsync(JobState state, CancellationToken cancellationToken = default) => Call(() => inner.SetStateAsync(state, cancellationToken));

        public Task<long> PruneAsync(long before, CancellationToken cancellationToken = default) => Call(() => inner.PruneAsync(before, cancellationToken));

        public Task<bool> UpdateRunIfAsync(Run run, IReadOnlyList<RunStatus> from, CancellationToken cancellationToken = default) =>
            Call(() => inner.UpdateRunIfAsync(run, from, cancellationToken));

        public Task<bool> CompareAndSetStateAsync(JobState state, long expected, CancellationToken cancellationToken = default) =>
            Call(() =>
            {
                Interlocked.Increment(ref _stateWrites);
                return inner.CompareAndSetStateAsync(state, expected, cancellationToken);
            });
    }

    private sealed class FuncTriage(Func<TriageContext, Task<string?>> answer) : ITriage
    {
        public Task<string?> TriageAsync(TriageContext context, CancellationToken cancellationToken) => answer(context);
    }

    /// <summary>A client for a process that is about to die: its own clock, no hook, errors ignored.</summary>
    private static CronwatchClient Dying(IStore store, IEnumerable<IChannel> channels, ITriage? triage = null) => new(new CronwatchOptions
    {
        Store = store,
        Clock = Clock(),
        Alerts = channels.ToList(),
        CronSecret = CronSecret.None,
        OnError = (e, where) => { },
        OnWarning = _ => { },
        ProcessExitHook = false,
        Triage = triage,
    });

    [Fact]
    public async Task The_write_that_opens_a_condition_holds_its_alert_so_a_process_that_dies_before_sending_it_does_not_lose_it()
    {
        var shared = new MemoryStore();
        var mortal = new Mortal(shared);
        var triaging = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
        // The process dies while its triage call is out: no channel was ever called. Never
        // disposed, as a dead process is not.
        var dying = Dying(mortal, [new Capture()], new FuncTriage(_ =>
        {
            mortal.Kill();
            triaging.TrySetResult();
            return new TaskCompletionSource<string?>().Task;
        }));
        _ = dying.RunAsync("nightly", (j, ct) => throw new InvalidOperationException("disk full"));
        await triaging.Task.WaitAsync(TimeSpan.FromMinutes(1));
        JobState state = (await shared.GetStateAsync("nightly"))!;
        Assert.Equal(T0, state.OpenAt(Condition.Failed));
        SendingAlert entry = Assert.Single(state.Sending!);
        Assert.Equal((AlertType.Failed, T0, T0 + Evaluate.SendLeaseMs), (entry.Alert!.Type, entry.Alert.At, entry.Until!.Value));
        Assert.False(entry.Alert.TriageTried, "triage is made at send time, never stored here");
        Assert.DoesNotContain("triage", state.ToJson(), StringComparison.Ordinal);
        Assert.Empty(state.Undelivered!);

        // Another process's checks leave it alone while its sender's lease runs.
        await using var server = Make(store: shared, triage: new FuncTriage(_ => Task.FromResult<string?>("The disk is full.")));
        server.Clock.Advance(Min);
        await server.Cw.CheckAsync();
        Assert.Empty(server.Alerts.Types());

        // Once it has run out, the next check sends it, triaged, once.
        server.Clock.Set(T0 + Evaluate.SendLeaseMs + 1);
        CheckResult result = await server.Cw.CheckAsync();
        Assert.Equal([AlertType.Failed], result.Alerts.Select(a => a.Type));
        Alert sent = Assert.Single(server.Alerts.List());
        Assert.Equal((AlertType.Failed, T0, "The disk is full."), (sent.Type, sent.At, sent.Triage));
        JobState after = (await shared.GetStateAsync("nightly"))!;
        Assert.Null(after.Sending);
        Assert.DoesNotContain("\"sending\"", after.ToJson(), StringComparison.Ordinal);
        Assert.Empty(after.Undelivered!);
        await server.Cw.CheckAsync();
        await Quietly(() => server.Cw.RunAsync("nightly", (j, ct) => throw new InvalidOperationException("again")));
        Assert.Equal(["failed"], server.Alerts.Types());
    }

    [Fact]
    public async Task An_alert_a_channel_took_just_before_its_process_died_is_sent_again_after_the_lease()
    {
        var shared = new MemoryStore();
        var mortal = new Mortal(shared);
        var first = new ConcurrentQueue<Alert>();
        // Accepted, then the process is gone before it records that.
        var dying = Dying(mortal, [CustomChannel.Create("first", (a, ctx, ct) =>
        {
            first.Enqueue(a);
            mortal.Kill();
            return Task.CompletedTask;
        })]);
        _ = dying.RunAsync("nightly", (j, ct) => throw new InvalidOperationException("x"));
        await Eventually("the first send", () => !first.IsEmpty);
        await using var server = Make(store: shared);
        server.Clock.Set(T0 + Evaluate.SendLeaseMs + 1);
        await server.Cw.CheckAsync();
        // Sent a second time: the one duplicate a crash can cause.
        Assert.Equal(["failed"], server.Alerts.Types());
    }

    [Fact]
    public async Task While_an_alert_is_being_sent_no_check_anywhere_sends_it_too()
    {
        var shared = new MemoryStore();
        // Clocks of their own: moving the sender's would run out its channel's time.
        var clock = Clock();
        var sending = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
        var gate = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
        var sent = new ConcurrentQueue<Alert>();
        var held = CustomChannel.Create("held", async (a, ctx, ct) =>
        {
            sending.TrySetResult();
            await gate.Task;
            sent.Enqueue(a);
        });
        await using var worker = new CronwatchClient(new CronwatchOptions
        {
            Store = shared,
            Clock = clock,
            Alerts = [held],
            CronSecret = CronSecret.None,
            OnError = (e, where) => { },
            ProcessExitHook = false,
        });
        await using var server = Make(store: shared);
        Task run = Quietly(() => worker.RunAsync("nightly", (j, ct) => throw new InvalidOperationException("x")));
        await sending.Task.WaitAsync(TimeSpan.FromMinutes(1));
        server.Clock.Advance(Min);
        await server.Cw.CheckAsync();
        // The sending process's own check, too.
        await worker.CheckAsync().WaitAsync(TimeSpan.FromMinutes(1));
        gate.SetResult();
        await run;
        Assert.Equal([AlertType.Failed], sent.Select(a => a.Type));
        Assert.Empty(server.Alerts.Types());
        JobState state = (await shared.GetStateAsync("nightly"))!;
        Assert.Null(state.Sending);
        Assert.Empty(state.Undelivered!);
        Assert.Equal(T0, state.LastAlertAt);
        clock.Set(T0 + Evaluate.SendLeaseMs + Min);
        server.Clock.Set(T0 + Evaluate.SendLeaseMs + Min);
        await server.Cw.CheckAsync();
        await worker.CheckAsync();
        Assert.Empty(server.Alerts.Types());
        Assert.Single(sent);
    }

    [Fact]
    public async Task An_alert_no_channel_took_moves_from_the_outbox_to_the_retry_queue_with_its_triage()
    {
        var down = CustomChannel.Create("down", (a, ctx, ct) => throw new InvalidOperationException("down"));
        var store = new MemoryStore();
        await using var cw = new CronwatchClient(new CronwatchOptions
        {
            Store = store,
            Clock = Clock(),
            Alerts = [down],
            CronSecret = CronSecret.None,
            OnError = (e, where) => { },
            ProcessExitHook = false,
            Triage = new FuncTriage(_ => Task.FromResult<string?>("Look at the disk.")),
        });
        await Quietly(() => cw.RunAsync("nightly", (j, ct) => throw new InvalidOperationException("x")));
        JobState state = (await store.GetStateAsync("nightly"))!;
        Assert.Null(state.Sending);
        Alert queued = Assert.Single(state.Undelivered!);
        Assert.Equal((AlertType.Failed, "Look at the disk."), (queued.Type, queued.Triage));
    }

    [Fact]
    public async Task A_process_that_queues_its_alerts_for_a_check_elsewhere_writes_them_with_the_state_that_opens_the_condition()
    {
        var shared = new MemoryStore();
        var counting = new Mortal(shared);
        await using var recorder = Make(store: counting, deliver: Deliver.AtCheck);
        await Quietly(() => recorder.Cw.RunAsync("backup", (j, ct) => throw new InvalidOperationException("disk full")));
        JobState state = (await shared.GetStateAsync("backup"))!;
        Assert.Equal([AlertType.Failed], state.Undelivered!.Select(a => a.Type));
        Assert.Null(state.Sending);
        Assert.Null(state.PendingRuns);
        // The start holds the run; then one write: the failure, its alert, and the run's release together.
        Assert.Equal(2, counting.StateWrites);
    }

    [Fact]
    public void A_malformed_sending_entry_never_makes_the_state_unreadable()
    {
        JobState state = JobState.FromJson(
            "{\"job\":\"j\",\"open\":{},\"consecutiveFailures\":0,\"silencedUntil\":null,\"lastAlertAt\":null,"
            + "\"sending\":[null,7,{\"until\":\"x\"},{\"until\":5,\"alert\":\"x\"},{\"until\":1.5}]}");
        Assert.Equal(5, state.Sending!.Count);
        Assert.All(state.Sending, e => Assert.Null(e.Alert));
        Assert.Equal([null, null, null, 5L, 2L], state.Sending.Select(e => e.Until));
        // Every one has run out by 5, and none has an alert to queue.
        var (released, dropped) = Evaluate.ReleaseSending(state, 5);
        Assert.Null(released.Sending);
        Assert.Empty(released.Undelivered!);
        Assert.Equal(0, dropped);
        // An empty list is no key at all.
        Assert.Null(JobState.FromJson("{\"job\":\"j\",\"sending\":[]}").Sending);
    }

    /// <summary>A channel that fails while <see cref="Down"/> and keeps what it sends otherwise.</summary>
    private sealed class Flaky : IChannel
    {
        public volatile bool Down = true;

        public ConcurrentQueue<Alert> Sent { get; } = new();

        public string Name => "flaky";

        public Task SendAsync(Alert alert, ChannelContext context, CancellationToken cancellationToken)
        {
            if (Down)
            {
                throw new InvalidOperationException("down");
            }
            Sent.Enqueue(alert);
            return Task.CompletedTask;
        }
    }

    private static CronwatchClient Client(IStore store, Microsoft.Extensions.Time.Testing.FakeTimeProvider clock, IChannel channel) =>
        new(new CronwatchOptions
        {
            Store = store,
            Clock = clock,
            Alerts = [channel],
            CronSecret = CronSecret.None,
            OnError = (_, _) => { },
            OnWarning = _ => { },
            ProcessExitHook = false,
        });

    [Fact]
    public async Task Two_processes_checking_at_once_retry_a_queued_alert_once()
    {
        var clock = Clock();
        var shared = new MemoryStore();
        var channel = new Flaky();
        await using var a = Client(shared, clock, channel);
        await using var b = Client(shared, clock, channel);
        await Quietly(() => a.RunAsync("nightly", (j, ct) => throw new InvalidOperationException("disk full")));
        Assert.Equal([AlertType.Failed], (await shared.GetStateAsync("nightly"))!.Undelivered!.Select(x => x.Type));
        channel.Down = false;
        clock.Advance(TimeSpan.FromMinutes(1));
        // The one whose claim lands sends it.
        await Task.WhenAll(a.CheckAsync(), b.CheckAsync());
        Assert.Equal([AlertType.Failed], channel.Sent.Select(x => x.Type));
        JobState state = (await shared.GetStateAsync("nightly"))!;
        Assert.Empty(state.Undelivered!);
        Assert.Null(state.Sending);
    }

    [Fact]
    public async Task While_a_job_is_silenced_a_retry_claims_only_recoveries()
    {
        var clock = Clock();
        var shared = new MemoryStore();
        var channel = new Flaky();
        await using var cw = Client(shared, clock, channel);
        await Quietly(() => cw.RunAsync("nightly", (j, ct) => throw new InvalidOperationException("disk full")));
        await cw.SilenceAsync("nightly", "1h");
        channel.Down = false;
        clock.Advance(TimeSpan.FromMinutes(1));
        await cw.CheckAsync();
        Assert.Empty(channel.Sent);
        Assert.Equal([AlertType.Failed], (await shared.GetStateAsync("nightly"))!.Undelivered!.Select(x => x.Type));
        await cw.UnsilenceAsync("nightly");
        await cw.CheckAsync();
        Assert.Equal([AlertType.Failed], channel.Sent.Select(x => x.Type));
    }

    [Fact]
    public async Task A_finished_run_whose_evaluation_was_never_written_is_evaluated_by_a_later_check_once()
    {
        // The finish's state write keeps losing, as if the process stopped between the run's write
        // and its evaluation.
        var contested = new Wrapped();
        await using var m = Make(store: contested);
        RunHandle handle = await m.Cw.Job("nightly").StartAsync();
        contested.CasMode = Cas.Refused;
        await handle.FailAsync(new InvalidOperationException("disk full"));
        contested.CasMode = Cas.Normal;
        Assert.Equal(RunStatus.Failed, (await contested.Inner.GetRunAsync(handle.Id))!.Status);
        Assert.Empty(m.Alerts.Types());
        Assert.Equal([handle.Id], (await contested.Inner.GetStateAsync("nightly"))!.PendingRuns!);
        // Left alone at first, in case its finish is still being written elsewhere.
        m.Clock.Advance(TimeSpan.FromMinutes(1));
        await m.Cw.CheckAsync();
        Assert.Empty(m.Alerts.Types());
        m.Clock.Advance(TimeSpan.FromMilliseconds(Evaluate.SendLeaseMs));
        await m.Cw.CheckAsync();
        Assert.Equal(["failed"], m.Alerts.Types());
        Assert.Null((await contested.Inner.GetStateAsync("nightly"))!.PendingRuns);
        await m.Cw.CheckAsync();
        Assert.Equal(["failed"], m.Alerts.Types());
    }

    [Fact]
    public async Task A_pending_run_the_store_no_longer_has_is_let_go_and_one_stored_unjudged_is_not_evaluated()
    {
        await using var m = Make();
        m.Cw.Job("nightly");
        await m.Store.SetStateAsync(JobState.Initial("nightly") with { PendingRuns = ValueList<string>.Of(["gone"]) });
        await m.Cw.CheckAsync();
        Assert.Null((await m.Store.GetStateAsync("nightly"))!.PendingRuns);
        long now = m.Clock.Ms();
        var run = new Run { Id = "imported", Job = "nightly", Status = RunStatus.Running, StartedAt = now, Trigger = "x" };
        await m.Cw.RecordRunAsync(run);
        Assert.Equal(["imported"], (await m.Store.GetStateAsync("nightly"))!.PendingRuns!);
        await m.Cw.RecordRunAsync(run with { Status = RunStatus.Failed, FinishedAt = now, DurationMs = 0, Error = "x" }, evaluate: false);
        Assert.Null((await m.Store.GetStateAsync("nightly"))!.PendingRuns);
        m.Clock.Advance(TimeSpan.FromMilliseconds(Evaluate.SendLeaseMs + 60_000));
        await m.Cw.CheckAsync();
        Assert.Empty(m.Alerts.Types());
    }

    [Fact]
    public async Task An_evaluated_import_whose_state_write_fails_and_is_not_retried_is_closed_by_a_check()
    {
        var store = new Wrapped();
        await using var m = Make(store: store);
        m.Cw.Job("imported", new JobOptions { Schedule = "every 5m", Timeout = "1m" });
        await m.Cw.CheckAsync();
        long now = m.Clock.Ms();
        var run = new Run
        {
            Id = "lost-import",
            Job = "imported",
            Status = RunStatus.Failed,
            StartedAt = now - 2 * Min,
            FinishedAt = now - Min,
            DurationMs = Min,
            Error = "boom",
            Trigger = "source",
        };
        store.Break("compareAndSetState");
        await Assert.ThrowsAsync<CronwatchException>(() => m.Cw.RecordRunAsync(run));
        store.Mend("compareAndSetState");
        await m.Cw.CheckAsync();
        Assert.Equal(RunStatus.Running, (await store.Inner.GetRunAsync(run.Id))!.Status);
        m.Clock.Advance(TimeSpan.FromMilliseconds(Evaluate.SendLeaseMs + Min));
        await m.Cw.CheckAsync();
        Run closed = (await store.Inner.GetRunAsync(run.Id))!;
        Assert.Equal(RunStatus.Timeout, closed.Status);
        Assert.Equal(Min, closed.DurationMs);
        Assert.Equal("Imported, but its finish was never written; marked as timed out\nboom", closed.Error);
        Assert.Equal(["stuck"], m.Alerts.Types());
        Assert.NotEqual(JobHealth.Healthy, (await m.Cw.JobSummaryAsync("imported"))!.Health);
    }

    [Fact]
    public async Task An_evaluated_import_whose_finish_write_fails_after_its_marker_is_closed_by_a_check_and_a_late_success_recovers_it()
    {
        var store = new Wrapped();
        await using var m = Make(store: store);
        m.Cw.Job("imported", new JobOptions { Schedule = "0 * * * *", Timeout = "1m" });
        await m.Cw.CheckAsync();
        long now = m.Clock.Ms();
        var run = new Run
        {
            Id = "lost-finish",
            Job = "imported",
            Status = RunStatus.Ok,
            StartedAt = now,
            FinishedAt = now,
            DurationMs = 0,
            Trigger = "source",
        };
        store.Break("updateRunIf");
        await Assert.ThrowsAsync<CronwatchException>(() => m.Cw.RecordRunAsync(run));
        store.Mend("updateRunIf");
        m.Clock.Advance(TimeSpan.FromMilliseconds(Evaluate.SendLeaseMs + Min));
        await m.Cw.CheckAsync();
        Assert.Equal(RunStatus.Timeout, (await store.Inner.GetRunAsync(run.Id))!.Status);
        Assert.Null((await store.Inner.GetStateAsync(run.Job))!.PendingRuns);
        Assert.Equal(["stuck"], m.Alerts.Types());
        await m.Cw.RecordRunAsync(run);
        Assert.Equal(RunStatus.Ok, (await store.Inner.GetRunAsync(run.Id))!.Status);
        Assert.Equal(["stuck", "recovered"], m.Alerts.Types());
    }

    [Fact]
    public async Task An_import_still_running_is_left_to_the_jobs_timeout_not_closed_as_stranded()
    {
        await using var m = Make();
        m.Cw.Job("long", new JobOptions { Timeout = "1h" });
        await m.Cw.RecordRunAsync(Run.Running("long-run", "long", m.Clock.Ms(), "source"));
        m.Clock.Advance(TimeSpan.FromMilliseconds(Evaluate.SendLeaseMs + Min));
        await m.Cw.CheckAsync();
        Run still = (await m.Store.GetRunAsync("long-run"))!;
        Assert.Equal(RunStatus.Running, still.Status);
        Assert.Null(still.FinishedAt);
        m.Clock.Advance(TimeSpan.FromMinutes(60));
        await m.Cw.CheckAsync();
        Assert.Equal(RunStatus.Timeout, (await m.Store.GetRunAsync("long-run"))!.Status);
        Assert.Equal(["stuck"], m.Alerts.Types());
    }
}
