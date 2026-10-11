using System;
using System.Collections.Generic;
using System.Linq;
using System.Text;
using System.Threading.Tasks;
using Cronwatch.Internal;
using Cronwatch.Web;
using Xunit;
using static Cronwatch.Tests.Support;

namespace Cronwatch.Tests;

/// <summary>The SDK's audit fixes as the client and the routes carry them, beside the fixtures that pin the rules.</summary>
public class PortedFixesTests
{
    private static long At(int hour, int minute) => Js.DateUtc(2026, 0, 5, hour, minute, 0, 0);

    private static Run Reported(string id, long startedAt, long finishedAt) => new()
    {
        Id = id,
        Job = "imported",
        Status = RunStatus.Ok,
        StartedAt = startedAt,
        FinishedAt = finishedAt,
        DurationMs = finishedAt - startedAt,
        Trigger = "source",
    };

    [Fact]
    public async Task A_run_a_source_reports_closes_missed_only_when_its_start_covers_the_fire()
    {
        await using var m = Make();
        m.Cw.Job("imported", new JobOptions { Schedule = "0 * * * *", Timezone = "UTC", Grace = "10m" });
        m.Clock.Set(At(10, 0));
        await m.Cw.RunAsync("imported", (j, ct) => Task.CompletedTask);
        m.Clock.Set(At(11, 15));
        await m.Cw.CheckAsync();
        Assert.Equal(["missed"], m.Alerts.Types());

        // Started at 10:58, before the 11:00 fire and outside its minute of slack: missed stays open.
        m.Clock.Set(At(11, 28));
        Assert.Empty(await m.Cw.RecordRunAsync(Reported("early", At(10, 58), At(11, 28))));
        Assert.Contains(Condition.Missed, (await m.Store.GetStateAsync("imported"))!.Open.Keys);

        // Started at 10:59, inside the slack: it covers the fire, so missed closes and recovers.
        m.Clock.Set(At(11, 29));
        var alerts = await m.Cw.RecordRunAsync(Reported("covering", At(10, 59), At(11, 29)));
        Assert.Equal(["recovered"], alerts.Select(a => a.Type.Value).ToList());
        Assert.Empty((await m.Store.GetStateAsync("imported"))!.Open);
        Assert.Empty(m.Errors.Entries);
    }

    [Fact]
    public async Task A_store_without_its_conditional_writes_is_reported_once_and_still_works()
    {
        await using var m = Make(store: new Plain(new MemoryStore()));
        await m.Cw.RunAsync("plain", (j, ct) => Task.CompletedTask);
        await m.Cw.RunAsync("plain", (j, ct) => Task.CompletedTask);
        await m.Cw.CheckAsync();
        var entry = Assert.Single(m.Errors.Entries);
        Assert.Equal("store", entry.Where);
        Assert.Equal(
            "the store has no compareAndSetState or updateRunIf, so two processes sharing it can overwrite each other's state or both evaluate the same run; it is safe only while one process at a time uses it",
            entry.Error.Message);
        Assert.Equal(2, (await m.Cw.RunsAsync("plain")).Count);
    }

    [Fact]
    public async Task A_store_without_its_own_last_run_answers_the_newest_of_its_runs()
    {
        var inner = new MemoryStore();
        await using var m = Make(store: inner);
        await m.Cw.RunAsync("newest", (j, ct) => Task.CompletedTask);
        m.Clock.Advance(Min);
        await m.Cw.RunAsync("newest", (j, ct) => Task.FromResult("second"));
        IStore unlasted = new Unlasted(inner);
        Assert.Equal("second", (await unlasted.LastRunAsync("newest"))!.Output);
        Assert.Null(await unlasted.LastRunAsync("none"));
    }

    [Fact]
    public async Task A_failing_store_and_a_throwing_error_handler_still_run_the_job_and_finish_its_handle()
    {
        var store = new Wrapped();
        store.Break("upsertJob", "getJob", "insertRun", "updateRun", "updateRunIf", "getRun", "listRuns", "getState", "setState", "compareAndSetState");
        await using var cw = new CronwatchClient(new CronwatchOptions
        {
            Store = store,
            Clock = Clock(),
            Alerts = [],
            CronSecret = CronSecret.None,
            ProcessExitHook = false,
            OnError = (e, where) => throw new InvalidOperationException("handler down"),
        });
        Assert.Equal("done", await cw.RunAsync("down", (j, ct) => Task.FromResult("done")));
        var thrown = await Assert.ThrowsAsync<ArithmeticException>(() => cw.RunAsync("down", (j, ct) => throw new ArithmeticException("own")));
        Assert.Equal("own", thrown.Message);
        RunHandle handle = await cw.Job("down").StartAsync();
        await handle.FinishAsync();
    }

    private static async Task<(int Status, string Body)> GetAsync(Routes routes, string target)
    {
        var request = new CronwatchRequest("GET", target)
        {
            Headers = [new("host", "app.test"), new("authorization", "Bearer tok")],
        };
        CronwatchResponse answer = await routes.HandleAsync(request);
        return (answer.Status, Encoding.UTF8.GetString(answer.Body.Span));
    }

    [Fact]
    public async Task The_routes_answer_only_under_their_base_with_dot_segments_resolved()
    {
        await using var m = Make();
        m.Cw.Job("nightly", new JobOptions { Schedule = "0 2 * * *" });
        await m.Cw.CheckAsync();
        Routes routes = m.Cw.Routes(new RoutesOptions { Token = "tok", BasePath = "/cronwatch/" });
        foreach (string outside in new[] { "/cronwatch/../api/jobs", "/cronwatchapi/jobs", "/api/jobs", "/cronwatch-x/api/jobs" })
        {
            var (status, body) = await GetAsync(routes, outside);
            Assert.Equal(404, status);
            Assert.Contains("Not found", body, StringComparison.Ordinal);
        }
        foreach (string inside in new[] { "/cronwatch/./api/jobs", "/cronwatch/jobs/../api/jobs", "/cronwatch/api/jobs/" })
        {
            var (status, body) = await GetAsync(routes, inside);
            Assert.Equal(200, status);
            Assert.StartsWith("{", body, StringComparison.Ordinal);
        }
        Assert.Equal(200, (await GetAsync(routes, "/cronwatch")).Status);

        // Mounted at the root, every path is the routes'.
        Routes root = m.Cw.Routes(new RoutesOptions { Token = "tok", BasePath = "/" });
        Assert.Equal(200, (await GetAsync(root, "/api/jobs")).Status);
        Assert.Empty(m.Errors.Entries);
    }

    /// <summary>A store that leaves <see cref="IStore.LastRunAsync"/> to the interface's default.</summary>
    private sealed class Unlasted(IStore inner) : IStore
    {
        public Task InitAsync(System.Threading.CancellationToken cancellationToken = default) => inner.InitAsync(cancellationToken);

        public Task UpsertJobAsync(Definition definition, long now, System.Threading.CancellationToken cancellationToken = default) => inner.UpsertJobAsync(definition, now, cancellationToken);

        public Task<StoredJob?> GetJobAsync(string name, System.Threading.CancellationToken cancellationToken = default) => inner.GetJobAsync(name, cancellationToken);

        public Task<IReadOnlyList<StoredJob>> ListJobsAsync(System.Threading.CancellationToken cancellationToken = default) => inner.ListJobsAsync(cancellationToken);

        public Task DeleteJobAsync(string name, System.Threading.CancellationToken cancellationToken = default) => inner.DeleteJobAsync(name, cancellationToken);

        public Task InsertRunAsync(Run run, System.Threading.CancellationToken cancellationToken = default) => inner.InsertRunAsync(run, cancellationToken);

        public Task UpdateRunAsync(Run run, System.Threading.CancellationToken cancellationToken = default) => inner.UpdateRunAsync(run, cancellationToken);

        public Task<Run?> GetRunAsync(string id, System.Threading.CancellationToken cancellationToken = default) => inner.GetRunAsync(id, cancellationToken);

        public Task<IReadOnlyList<Run>> ListRunsAsync(string job, int limit, System.Threading.CancellationToken cancellationToken = default) => inner.ListRunsAsync(job, limit, cancellationToken);

        public Task<IReadOnlyList<Run>> RunningRunsAsync(System.Threading.CancellationToken cancellationToken = default) => inner.RunningRunsAsync(cancellationToken);

        public Task<JobState?> GetStateAsync(string job, System.Threading.CancellationToken cancellationToken = default) => inner.GetStateAsync(job, cancellationToken);

        public Task SetStateAsync(JobState state, System.Threading.CancellationToken cancellationToken = default) => inner.SetStateAsync(state, cancellationToken);

        public Task<long> PruneAsync(long before, System.Threading.CancellationToken cancellationToken = default) => inner.PruneAsync(before, cancellationToken);
    }
}
