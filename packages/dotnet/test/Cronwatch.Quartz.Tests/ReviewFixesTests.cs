using System;
using System.Collections.Generic;
using System.Globalization;
using System.Linq;
using System.Reflection;
using System.Threading;
using System.Threading.Tasks;
using Quartz;
using Xunit;
using static Cronwatch.Quartz.Tests.QuartzSupport;

namespace Cronwatch.Quartz.Tests;

/// <summary>
/// The cases of the 0.14 review's findings in the Quartz integration: a store that does not
/// answer, a close held up by a slow channel, a recovery beside another node's live run, and a
/// firing that comes while the scheduler is still being attached.
/// </summary>
public class ReviewFixesTests
{
    private static readonly TimeSpan Long = TimeSpan.FromSeconds(30);

    /// <summary>A proxy of <typeparamref name="T"/> that answers what <see cref="Answer"/> takes and hands the rest to <see cref="Inner"/>.</summary>
    public class Stub<T> : DispatchProxy
        where T : class
    {
        public T? Inner { get; set; }

        public Func<MethodInfo, object?[]?, (bool Taken, object? Value)> Answer { get; set; } = (_, _) => (false, null);

        protected override object? Invoke(MethodInfo? targetMethod, object?[]? args)
        {
            ArgumentNullException.ThrowIfNull(targetMethod);
            var (taken, value) = Answer(targetMethod, args);
            if (taken)
            {
                return value;
            }
            if (Inner == null)
            {
                throw new NotSupportedException(targetMethod.Name);
            }
            return targetMethod.Invoke(Inner, args);
        }

        public static T Of(T? inner, Func<MethodInfo, object?[]?, (bool Taken, object? Value)> answer)
        {
            T proxy = Create<T, Stub<T>>();
            var stub = (Stub<T>)(object)proxy;
            stub.Inner = inner;
            stub.Answer = answer;
            return proxy;
        }
    }

    /// <summary>A firing's context as Quartz hands one to a listener, for a job of <paramref name="name"/>.</summary>
    private static IJobExecutionContext Firing(IScheduler scheduler, string name, string fire, long? recoveringFrom = null)
    {
        IJobDetail detail = JobBuilder.Create<NoopJob>().WithIdentity(name).Build();
        var data = new JobDataMap();
        if (recoveringFrom is long firedAt)
        {
            data[SchedulerConstants.FailedJobOriginalTriggerFireTime] = firedAt.ToString(CultureInfo.InvariantCulture);
        }
        return Stub<IJobExecutionContext>.Of(null, (method, _) => method.Name switch
        {
            "get_JobDetail" => (true, detail),
            "get_Scheduler" => (true, scheduler),
            "get_FireInstanceId" => (true, fire),
            "get_RefireCount" => (true, 0),
            "get_Recovering" => (true, recoveringFrom != null),
            "get_MergedJobDataMap" => (true, data),
            _ => (false, null),
        });
    }

    /// <summary>A store whose run inserts wait while it is held.</summary>
    private sealed class Gated : IStore, IUpdateRunIfStore, ICompareAndSetStateStore
    {
        private readonly MemoryStore _inner = new();
        private volatile TaskCompletionSource? _gate;

        public void Hold() => _gate = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);

        public void Release() => _gate?.TrySetResult();

        public Task InitAsync(CancellationToken cancellationToken = default) => _inner.InitAsync(cancellationToken);

        public Task UpsertJobAsync(Definition definition, long now, CancellationToken cancellationToken = default) => _inner.UpsertJobAsync(definition, now, cancellationToken);

        public Task<StoredJob?> GetJobAsync(string name, CancellationToken cancellationToken = default) => _inner.GetJobAsync(name, cancellationToken);

        public Task<IReadOnlyList<StoredJob>> ListJobsAsync(CancellationToken cancellationToken = default) => _inner.ListJobsAsync(cancellationToken);

        public Task DeleteJobAsync(string name, CancellationToken cancellationToken = default) => _inner.DeleteJobAsync(name, cancellationToken);

        public async Task InsertRunAsync(Run run, CancellationToken cancellationToken = default)
        {
            if (_gate is { } gate)
            {
                await gate.Task;
            }
            await _inner.InsertRunAsync(run, cancellationToken);
        }

        public Task UpdateRunAsync(Run run, CancellationToken cancellationToken = default) => _inner.UpdateRunAsync(run, cancellationToken);

        public Task<bool> UpdateRunIfAsync(Run run, IReadOnlyList<RunStatus> from, CancellationToken cancellationToken = default) => _inner.UpdateRunIfAsync(run, from, cancellationToken);

        public Task<Run?> GetRunAsync(string id, CancellationToken cancellationToken = default) => _inner.GetRunAsync(id, cancellationToken);

        public Task<IReadOnlyList<Run>> ListRunsAsync(string job, int limit, CancellationToken cancellationToken = default) => _inner.ListRunsAsync(job, limit, cancellationToken);

        public Task<Run?> LastRunAsync(string job, CancellationToken cancellationToken = default) => _inner.LastRunAsync(job, cancellationToken);

        public Task<IReadOnlyList<Run>> RunningRunsAsync(CancellationToken cancellationToken = default) => _inner.RunningRunsAsync(cancellationToken);

        public Task<JobState?> GetStateAsync(string job, CancellationToken cancellationToken = default) => _inner.GetStateAsync(job, cancellationToken);

        public Task SetStateAsync(JobState state, CancellationToken cancellationToken = default) => _inner.SetStateAsync(state, cancellationToken);

        public Task<bool> CompareAndSetStateAsync(JobState state, long expected, CancellationToken cancellationToken = default) => _inner.CompareAndSetStateAsync(state, expected, cancellationToken);

        public Task<long> PruneAsync(long before, CancellationToken cancellationToken = default) => _inner.PruneAsync(before, cancellationToken);
    }

    /// <summary>A channel that takes no alert until it is released.</summary>
    private sealed class Slow : IChannel
    {
        private readonly TaskCompletionSource _gate = new(TaskCreationOptions.RunContinuationsAsynchronously);

        public string Name => "slow";

        public void Release() => _gate.TrySetResult();

        public Task SendAsync(Alert alert, ChannelContext context, CancellationToken cancellationToken) => _gate.Task;
    }

    [Fact]
    public async Task A_store_that_does_not_answer_holds_a_job_for_one_bounded_wait_and_the_run_is_recorded_once_it_does()
    {
        var store = new Gated();
        await using var m = Client(store);
        IScheduler scheduler = await BuildAsync();
        try
        {
            await using CronwatchQuartz q = await CronwatchQuartz.WatchAsync(m.Cw, scheduler, new CronwatchQuartzOptions { App = "billing" });
            q.StoreWait = TimeSpan.FromMilliseconds(200);
            int ran = 0;
            store.Hold();
            await scheduler.Start(default);
            await scheduler.ScheduleJob(
                Job("held", _ =>
                {
                    Interlocked.Increment(ref ran);
                    return default;
                }),
                Once("held"),
                default,
                default);
            // The job ran past the store that had not taken its run.
            await Eventually("the job to run", () => Volatile.Read(ref ran) == 1);
            await Eventually("the report", () => !m.Errors.IsEmpty);
            Assert.StartsWith(
                "quartz: cronwatch: the store did not answer within 200ms while opening a run of held, so the job went on",
                Assert.Single(m.Errors),
                StringComparison.Ordinal);
            Assert.Empty(await Runs(m.Cw, "held"));
            store.Release();
            await Eventually("the run recorded", async () => (await Runs(m.Cw, "held")).Any(r => r.Status == RunStatus.Ok));
            await q.RecordedAsync();
            Assert.Single(m.Errors);
            Assert.Single(await Runs(m.Cw, "held"));
        }
        finally
        {
            store.Release();
            await scheduler.Shutdown(true, default);
        }
    }

    [Fact]
    public async Task A_close_held_up_by_a_slow_channel_holds_quartz_for_one_bounded_wait_and_reports_nothing()
    {
        var slow = new Slow();
        var errors = new System.Collections.Concurrent.ConcurrentQueue<string>();
        var store = new MemoryStore();
        await using var cw = new CronwatchClient(new CronwatchOptions
        {
            Store = store,
            Alerts = [slow],
            CronSecret = CronSecret.None,
            ProcessExitHook = false,
            OnError = (e, where) => errors.Enqueue(where + ": " + e.Message),
            OnWarning = _ => { },
        });
        IScheduler scheduler = await BuildAsync();
        try
        {
            await using CronwatchQuartz q = await CronwatchQuartz.WatchAsync(cw, scheduler, new CronwatchQuartzOptions { App = "billing" });
            q.StoreWait = TimeSpan.FromMilliseconds(200);
            IJobExecutionContext firing = Firing(scheduler, "fails", "fire-1");
            await q.JobListener.JobToBeExecuted(firing, default);
            // Quartz is told the job was executed; the alert is still on its way to the channel.
            await q.JobListener.JobWasExecuted(firing, new JobExecutionException("it failed"), default).AsTask().WaitAsync(Long);
            Assert.Equal(RunStatus.Failed, Assert.Single(await Runs(cw, "fails")).Status);
            Assert.Empty(errors);
            slow.Release();
            await q.RecordedAsync();
            Assert.Empty(errors);
        }
        finally
        {
            slow.Release();
            await scheduler.Shutdown(true, default);
        }
    }

    [Fact]
    public async Task A_recovered_firing_leaves_another_nodes_later_run_alone()
    {
        await using var m = Client();
        IScheduler scheduler = await BuildAsync();
        try
        {
            await using CronwatchQuartz q = await CronwatchQuartz.WatchAsync(m.Cw, scheduler, new CronwatchQuartzOptions { App = "billing" });
            // A job every thirty seconds whose runs overlap: node a stopped during the one it fired
            // at T, and node c is running the one fired at T + 30s.
            long firedAt = DateTimeOffset.UtcNow.ToUnixTimeMilliseconds() - 40_000;
            m.Cw.Job("long");
            await m.Cw.SyncJobAsync("long");
            const string Stopped = "quartz:billing:node-a:a1:0";
            const string Live = "quartz:billing:node-c:c7:0";
            await m.Store.InsertRunAsync(Run.Running(Stopped, "long", firedAt + 15, CronwatchQuartz.Trigger));
            await m.Store.InsertRunAsync(Run.Running(Live, "long", firedAt + 30_000, CronwatchQuartz.Trigger));

            IJobExecutionContext recovering = Firing(scheduler, "long", "fire-9", firedAt);
            await q.JobListener.JobToBeExecuted(recovering, default);
            Run? stopped = await m.Store.GetRunAsync(Stopped);
            Assert.Equal(RunStatus.Failed, stopped!.Status);
            Assert.Equal(CronwatchQuartz.Recovered, stopped.Error);
            Assert.Equal(RunStatus.Running, (await m.Store.GetRunAsync(Live))!.Status);
            await q.JobListener.JobWasExecuted(recovering, null, default);
            Assert.Empty(m.Errors);
        }
        finally
        {
            await scheduler.Shutdown(true, default);
        }
    }

    [Fact]
    public void The_run_a_recovery_finishes_is_the_nearest_start_and_only_while_it_runs()
    {
        const string Prefix = "quartz:billing:";
        const string Mine = "quartz:billing:node-b:";
        Run At(string id, long startedAt, bool running = true) =>
            Run.Running(id, "long", startedAt, "quartz") with { Status = running ? RunStatus.Running : RunStatus.Ok };
        Run stopped = At(Prefix + "node-a:1:0", 1_000_020);
        Run later = At(Prefix + "node-c:2:0", 1_030_000);
        Assert.Equal(stopped.Id, CronwatchQuartz.RecoveredRun([later, stopped], Prefix, Mine, 1_000_000)?.Id);
        // The nearest has finished: there is nothing to recover, and the later run is not taken for it.
        Assert.Null(CronwatchQuartz.RecoveredRun([later, At(stopped.Id, 1_000_020, running: false)], Prefix, Mine, 1_000_000));
        // This instance's own runs, another app's, and runs past the minute are never taken.
        Assert.Null(CronwatchQuartz.RecoveredRun(
            [At(Mine + "3:0", 1_000_000), At("quartz:other:node-a:4:0", 1_000_000), At(Prefix + "node-a:5:0", 1_060_001)],
            Prefix,
            Mine,
            1_000_000));
    }

    [Fact]
    public async Task A_firing_that_comes_while_the_scheduler_is_being_attached_waits_for_its_instance_part()
    {
        await using var m = Client();
        IScheduler real = await BuildAsync();
        try
        {
            var entered = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
            var gate = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
            async Task<SchedulerMetadata> HeldAsync(CancellationToken ct)
            {
                entered.TrySetResult();
                await gate.Task;
                return await real.GetMetadata(ct);
            }
            // The scheduler itself, but for its metadata, which it is slow to give.
            IScheduler slow = Stub<IScheduler>.Of(real, (method, args) =>
            {
                if (method.Name != nameof(IScheduler.GetMetadata))
                {
                    return (false, null);
                }
                Task<SchedulerMetadata> held = HeldAsync((CancellationToken)args![0]!);
                return (true, method.ReturnType == typeof(Task<SchedulerMetadata>) ? held : new ValueTask<SchedulerMetadata>(held));
            });
            var q = new CronwatchQuartz(m.Cw, new CronwatchQuartzOptions { App = "billing" }, null);
            await using (q)
            {
                Task first = q.AttachAsync(slow, default);
                await entered.Task.WaitAsync(Long);
                // A firing on another thread, attaching as the first is.
                Task second = q.AttachAsync(slow, default);
                await Task.Delay(200);
                Assert.False(second.IsCompleted, "the second caller went on before the instance part was known");
                gate.SetResult();
                await first.WaitAsync(Long);
                await second.WaitAsync(Long);
                Assert.StartsWith(
                    "quartz:billing:" + real.SchedulerInstanceId + ".",
                    q.RunId(Firing(real, "any", "fire-1")),
                    StringComparison.Ordinal);
            }
        }
        finally
        {
            await real.Shutdown(true, default);
        }
    }
}
