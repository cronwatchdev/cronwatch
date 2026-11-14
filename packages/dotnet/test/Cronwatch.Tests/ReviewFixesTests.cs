using System;
using System.Collections.Generic;
using System.Data.Common;
using System.IO;
using System.Linq;
using System.Runtime.CompilerServices;
using System.Threading;
using System.Threading.Tasks;
using Cronwatch.Bridge;
using Cronwatch.Tests.Posting;
using Xunit;
using static Cronwatch.Tests.Support;
using P = Cronwatch.Internal.Post;

namespace Cronwatch.Tests;

/// <summary>
/// The cases of the 0.14 review's findings in the core: a channel that blocks its caller, tasks
/// given up on that fail later, and a name or an id with a NUL on Postgres.
/// </summary>
public class ReviewFixesTests
{
    [Fact]
    public async Task A_channel_that_blocks_its_caller_is_held_to_the_deadline_as_one_that_awaits_is()
    {
        using var release = new ManualResetEventSlim();
        var entered = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
        // A synchronous client inside SendAsync: it never gets as far as a task to wait on.
        var blocking = CustomChannel.Create("blocking", (a, ctx, ct) =>
        {
            entered.TrySetResult();
            release.Wait(TimeSpan.FromMinutes(2), CancellationToken.None);
            return Task.CompletedTask;
        });
        var m = Make(channels: [blocking], timings: new Timings { Channel = TimeSpan.FromSeconds(1) });
        try
        {
            var failing = Assert.ThrowsAsync<InvalidOperationException>(() => m.Cw.RunAsync("h", (j, ct) => throw new InvalidOperationException("x")));
            await entered.Task.WaitAsync(TimeSpan.FromSeconds(30));
            await Eventually("the run to be recorded", () =>
            {
                m.Clock.Advance(1000);
                return failing.IsCompleted;
            });
            await failing;
            Assert.Equal(["failed"], m.Alerts.Types());
            Assert.Equal(["alert channel blocking"], m.Errors.Wheres());
            Assert.Contains("timed out after 1000ms", m.Errors.Messages()[0], StringComparison.Ordinal);
            Assert.Equal(["blocking"], Assert.Single((await m.Store.GetStateAsync("h"))!.Undelivered!).Owed!);
        }
        finally
        {
            release.Set();
            await m.DisposeAsync();
        }
    }

    [Fact]
    public async Task A_transports_answer_that_fails_after_its_deadline_leaves_no_unobserved_task_exception()
    {
        const string Marker = "the transport failed after the deadline, review probe";
        await Unobserved(Marker, async () =>
        {
            var gate = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
            var transport = new PostHardeningTests.FuncTransport(async (request, ct) =>
            {
                // An app's own transport that does not honour its token and fails once given up on.
                await gate.Task;
                throw new IOException(Marker);
            });
            var e = await Assert.ThrowsAsync<CronwatchException>(() =>
                P.FetchAsync(transport, TimeSpan.FromMilliseconds(50), "https://hooks.example.com/x", [], "{}", CancellationToken.None));
            Assert.Equal(P.TimedOut, e.Message);
            gate.SetResult();
        });
    }

    [Fact]
    public async Task A_sync_given_up_on_that_fails_later_leaves_no_unobserved_task_exception()
    {
        const string Marker = "the sync failed after it was given up on, review probe";
        await using var m = Make();
        await Unobserved(Marker, async () =>
        {
            var gate = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
            bool done = await SchedulerBridge.SyncWithinAsync(m.Cw, TimeSpan.FromMilliseconds(50), "probe", async () =>
            {
                await gate.Task;
                throw new IOException(Marker);
            });
            Assert.False(done);
            gate.SetResult();
        });
        Assert.Equal(["probe"], m.Errors.Wheres());
    }

    /// <summary>Runs <paramref name="body"/> and fails if a task's exception with this message went unobserved.</summary>
    private static async Task Unobserved(string marker, Func<Task> body)
    {
        var unobserved = new List<Exception>();
        EventHandler<UnobservedTaskExceptionEventArgs> handler = (sender, e) =>
        {
            if (e.Exception.Flatten().InnerExceptions.Any(x => x.Message == marker))
            {
                lock (unobserved)
                {
                    unobserved.Add(e.Exception);
                }
            }
        };
        TaskScheduler.UnobservedTaskException += handler;
        try
        {
            await Apart(body);
            for (int i = 0; i < 5; i++)
            {
                await Task.Delay(50);
                GC.Collect();
                GC.WaitForPendingFinalizers();
            }
            lock (unobserved)
            {
                Assert.Empty(unobserved);
            }
        }
        finally
        {
            TaskScheduler.UnobservedTaskException -= handler;
        }
    }

    // In a frame of its own, so nothing the body made is still reachable when the collector runs.
    [MethodImpl(MethodImplOptions.NoInlining)]
    private static Task Apart(Func<Task> body) => Task.Run(body);

    [Fact]
    public async Task On_postgres_a_name_or_an_id_with_a_nul_reads_as_none_without_asking_the_database()
    {
        // Postgres refuses U+0000 in TEXT, so a statement binding one fails where the other
        // stores find nothing: the dashboard's /jobs/%00 and /api/runs/%00 answered 500 there.
        // The data source refuses to open, so any statement sent fails this test.
        await using var store = SqlStore.Postgres(new Unopened());
        Assert.Null(await store.GetJobAsync("a\0b"));
        Assert.Null(await store.GetRunAsync("a\0b"));
        Assert.Null(await store.LastRunAsync("a\0b"));
        Assert.Empty(await store.ListRunsAsync("a\0b", 20));
        Assert.Null(await store.GetStateAsync("a\0b"));
        Assert.False(await store.DeleteRunIfAsync("a\0b", "job", RunStatus.Running));
        await store.DeleteJobAsync("a\0b");
        // A name without one is still asked for.
        await Assert.ThrowsAsync<NotSupportedException>(() => store.GetJobAsync("ab"));
    }

    [Fact]
    public async Task An_error_handler_that_throws_does_not_stop_a_check_the_jobs_after_it_or_a_dashboard_read()
    {
        var store = new Failing();
        int calls = 0;
        var alerts = new Capture();
        var clock = Clock();
        await using var cw = new CronwatchClient(new CronwatchOptions
        {
            Store = store,
            Clock = clock,
            Alerts = [alerts],
            CronSecret = CronSecret.None,
            ProcessExitHook = false,
            Sources = [new Down()],
            OnWarning = _ => { },
            OnError = (e, where) =>
            {
                Interlocked.Increment(ref calls);
                throw new InvalidOperationException("the error handler threw");
            },
        });
        cw.Job("a-broken", new JobOptions { Schedule = "every 5m" });
        cw.Job("b-late", new JobOptions { Schedule = "every 5m" });
        await cw.JobsAsync(); // written to the store now, so both are due an hour on
        store.Broken.Add("a-broken");
        clock.Advance(Hour);
        CheckResult result = await cw.CheckAsync();
        Assert.True(Volatile.Read(ref calls) >= 3, "the source, the broken job, and the prune were each reported");
        Assert.Equal(["a-broken", "b-late"], result.Jobs.Select(j => j.Name));
        // The job after the broken one was still checked.
        Assert.Equal(["b-late missed"], alerts.Alerts.Select(a => a.Job + " " + a.Type.Value));
        // A dashboard read of the broken job answers, showing it as failing.
        Assert.Equal(JobHealth.Failing, (await cw.JobSummaryAsync("a-broken"))!.Health);
    }

    private sealed class Down : ISource
    {
        public string Name => "bad";

        public Task<IReadOnlyList<Alert>?> SyncAsync(CronwatchClient client, CancellationToken cancellationToken) =>
            throw new IOException("source down");
    }

    /// <summary>A memory store whose runs cannot be listed for the jobs named, and which cannot prune.</summary>
    private sealed class Failing : IStore, IUpdateRunIfStore, ICompareAndSetStateStore
    {
        private readonly MemoryStore _inner = new();

        public HashSet<string> Broken { get; } = new(StringComparer.Ordinal);

        public Task InitAsync(CancellationToken cancellationToken = default) => _inner.InitAsync(cancellationToken);

        public Task UpsertJobAsync(Definition definition, long now, CancellationToken cancellationToken = default) => _inner.UpsertJobAsync(definition, now, cancellationToken);

        public Task<StoredJob?> GetJobAsync(string name, CancellationToken cancellationToken = default) => _inner.GetJobAsync(name, cancellationToken);

        public Task<IReadOnlyList<StoredJob>> ListJobsAsync(CancellationToken cancellationToken = default) => _inner.ListJobsAsync(cancellationToken);

        public Task DeleteJobAsync(string name, CancellationToken cancellationToken = default) => _inner.DeleteJobAsync(name, cancellationToken);

        public Task InsertRunAsync(Run run, CancellationToken cancellationToken = default) => _inner.InsertRunAsync(run, cancellationToken);

        public Task UpdateRunAsync(Run run, CancellationToken cancellationToken = default) => _inner.UpdateRunAsync(run, cancellationToken);

        public Task<bool> UpdateRunIfAsync(Run run, IReadOnlyList<RunStatus> from, CancellationToken cancellationToken = default) => _inner.UpdateRunIfAsync(run, from, cancellationToken);

        public Task<Run?> GetRunAsync(string id, CancellationToken cancellationToken = default) => _inner.GetRunAsync(id, cancellationToken);

        public Task<IReadOnlyList<Run>> ListRunsAsync(string job, int limit, CancellationToken cancellationToken = default) =>
            Broken.Contains(job) ? throw new IOException("store down") : _inner.ListRunsAsync(job, limit, cancellationToken);

        public Task<Run?> LastRunAsync(string job, CancellationToken cancellationToken = default) => _inner.LastRunAsync(job, cancellationToken);

        public Task<IReadOnlyList<Run>> RunningRunsAsync(CancellationToken cancellationToken = default) => _inner.RunningRunsAsync(cancellationToken);

        public Task<JobState?> GetStateAsync(string job, CancellationToken cancellationToken = default) => _inner.GetStateAsync(job, cancellationToken);

        public Task SetStateAsync(JobState state, CancellationToken cancellationToken = default) => _inner.SetStateAsync(state, cancellationToken);

        public Task<bool> CompareAndSetStateAsync(JobState state, long expected, CancellationToken cancellationToken = default) => _inner.CompareAndSetStateAsync(state, expected, cancellationToken);

        public Task<long> PruneAsync(long before, CancellationToken cancellationToken = default) => throw new IOException("prune down");
    }

    [Fact]
    public async Task A_secret_that_starts_in_one_flush_and_ends_in_the_next_is_redacted_whole()
    {
        await using var m = Make();
        RunHandle run = await m.Cw.Job("deploy").StartAsync();
        run.Log("starting");
        run.Log("-----BEGIN PRIVATE KEY-----");
        run.Log("MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQC7VJTUt9Us8cKj");
        await run.FlushAsync();
        Assert.Equal("starting\n[redacted]", (await m.Cw.GetRunAsync(run.Id))!.Output);
        run.Log("MFNRV0X0e1nSecondHalfOfTheKeySecondHalfOfTheKeySecondHalfOfTheKe");
        run.Log("-----END PRIVATE KEY-----");
        run.Log("api_token:");
        await run.FlushAsync();
        Assert.Equal("starting\n[redacted]\napi_token:", (await m.Cw.GetRunAsync(run.Id))!.Output);
        // A label flushed, then its value: at the next flush, and again at finish.
        run.Log("hunter2hunter2");
        await run.FlushAsync();
        Assert.Equal("starting\n[redacted]\napi_token:\n[redacted]", (await m.Cw.GetRunAsync(run.Id))!.Output);
        run.Log("password:");
        await run.FlushAsync();
        run.Log("correct-horse");
        Run? finished = await run.FinishAsync();
        Assert.Equal("starting\n[redacted]\napi_token:\n[redacted]\npassword:\n[redacted]", finished!.Output);
        string stored = (await m.Cw.GetRunAsync(run.Id))!.ToJson();
        Assert.DoesNotContain("SecondHalf", stored, StringComparison.Ordinal);
        Assert.DoesNotContain("hunter2", stored, StringComparison.Ordinal);
        Assert.DoesNotContain("correct-horse", stored, StringComparison.Ordinal);
    }

    [Fact]
    public async Task Flushes_after_another_process_wrote_to_the_run_still_append_each_redacted()
    {
        await using var m = Make();
        RunHandle run = await m.Cw.Job("deploy").StartAsync();
        run.Log("one");
        await run.FlushAsync();
        // Another handle on the same run (another process) appends in between.
        RunHandle other = await m.Cw.Job("deploy").ResumeAsync(run.Id);
        other.Log("from elsewhere");
        await other.FlushAsync();
        run.Log("token=abc123abc123");
        await run.FlushAsync();
        Assert.Equal("one\nfrom elsewhere\ntoken=[redacted]", (await m.Cw.GetRunAsync(run.Id))!.Output);
        // A long run: the remembered tail stays bounded and the output capped.
        for (int i = 0; i < 6; i++)
        {
            run.Log("line " + i + " " + new string('x', 6000));
            await run.FlushAsync();
        }
        run.Log("secret=tail-secret-value");
        Run? finished = await run.FinishAsync();
        Assert.True(finished!.Output!.Length <= (16 * 1024) + 40);
        Assert.EndsWith("secret=[redacted]", finished.Output, StringComparison.Ordinal);
        Assert.DoesNotContain("tail-secret-value", finished.Output, StringComparison.Ordinal);
    }

    [Fact]
    public async Task A_handle_kept_across_a_forget_declares_the_job_again_on_its_next_run_and_writes_it_once()
    {
        int upserts = 0;
        var store = new Wrapped
        {
            BeforeUpsert = _ =>
            {
                Interlocked.Increment(ref upserts);
                return Task.CompletedTask;
            },
        };
        await using var m = Make(store: store);
        Job job = m.Cw.Job("nightly", new JobOptions { Schedule = "0 2 * * *", Description = "Nightly export" });
        await job.RunAsync((j, ct) => Task.CompletedTask);
        Assert.Equal(1, Volatile.Read(ref upserts));
        await m.Cw.ForgetAsync("nightly");
        Assert.Empty(m.Cw.DefinedJobs);

        await job.RunAsync((j, ct) => Task.CompletedTask);
        Assert.Equal(2, Volatile.Read(ref upserts));
        // Declared again, so checks and sources see it.
        Assert.Equal(["nightly"], m.Cw.DefinedJobs.Select(d => d.Name));
        await job.RunAsync((j, ct) => Task.CompletedTask);
        await job.RunAsync((j, ct) => Task.CompletedTask);
        // Written once, not on every run.
        Assert.Equal(2, Volatile.Read(ref upserts));
        Assert.Equal("Nightly export", (await m.Cw.JobSummaryAsync("nightly"))!.Definition.Description);

        // Forgotten by another process since: this one's check writes it again.
        await store.Inner.DeleteJobAsync("nightly");
        await m.Cw.CheckAsync();
        Assert.NotNull(await store.Inner.GetJobAsync("nightly"));
        Assert.Empty(m.Errors.Entries);
    }

    [Theory]
    [InlineData("check")]
    [InlineData("jobs")]
    public async Task A_forget_that_lands_while_declarations_are_being_written_is_not_undone(string read)
    {
        // The read's loop has picked up both names and is writing the first when the second is
        // forgotten (the dashboard's delete): held there, so the order is the test's, not a race's.
        var entered = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
        var gate = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
        var store = new Wrapped
        {
            BeforeUpsert = async definition =>
            {
                if (definition.Name == "keep")
                {
                    entered.TrySetResult();
                    await gate.Task;
                }
            },
        };
        await using var m = Make(store: store);
        m.Cw.Job("keep", new JobOptions { Schedule = "0 2 * * *" });
        m.Cw.Job("nightly", new JobOptions { Schedule = "0 2 * * *" });
        // Neither is written yet, so the read's own loop is what would write them.
        Task reading = read == "check" ? m.Cw.CheckAsync() : m.Cw.JobsAsync();
        await entered.Task.WaitAsync(TimeSpan.FromSeconds(30));
        await m.Cw.ForgetAsync("nightly");
        gate.SetResult();
        await reading;
        Assert.Equal(["keep"], m.Cw.DefinedJobs.Select(d => d.Name));
        Assert.Null(await store.Inner.GetJobAsync("nightly"));
        Assert.Null(await store.Inner.GetStateAsync("nightly"));
        // Its next run still brings it back.
        await m.Cw.RunAsync("nightly", (j, ct) => Task.CompletedTask);
        Assert.NotNull(await store.Inner.GetJobAsync("nightly"));
        Assert.Equal("0 2 * * *", (await m.Cw.JobSummaryAsync("nightly"))!.Definition.Schedule);
        Assert.Empty(m.Errors.Entries);
    }

    private sealed class Unopened : DbDataSource
    {
        public override string ConnectionString => "";

        protected override DbConnection CreateDbConnection() => throw new NotSupportedException("the store asked the database");
    }
}
