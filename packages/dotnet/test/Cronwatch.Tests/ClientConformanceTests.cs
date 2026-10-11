using System;
using System.Collections.Generic;
using System.Linq;
using System.Threading.Tasks;
using Cronwatch.Internal;
using Xunit;
using static Cronwatch.Tests.Support;

namespace Cronwatch.Tests;

/// <summary>
/// Replays conformance/client.json, the client driven through its public API on a fixed clock:
/// the run ids a run's start, <c>ResumeAsync</c>, and <c>RecordRunAsync</c> take
/// (<c>runIds</c>), and stored data a newer release wrote kept through a check, a silence, an
/// unsilence, a summary, and a run (<c>unknownFields</c>), over the memory store, SQLite, and
/// Postgres when <c>CRONWATCH_TEST_PG</c> is set.
/// </summary>
public class ClientConformanceTests
{
    private static readonly JsObject Fixture = Fixtures.Load("client");

    [Fact]
    public void Every_section_is_replayed() => DurationConformanceTests.Known(Fixture, "runIds", "unknownFields", "environment", "secrets", "outbox");

    /// <summary>
    /// <c>environment</c>: <c>CRONWATCH_ENV</c>, <c>APP_ENV</c>, then the language's own, here
    /// <c>ASPNETCORE_ENVIRONMENT</c>, read as every port reads them, and the app's own environment
    /// standing where the .NET variable does.
    /// </summary>
    [Fact]
    public void The_environment_is_read_as_the_sdk_reads_it()
    {
        var failures = new Fixtures.Failures();
        foreach (JsObject c in Fixtures.Objects(Fixture, "environment"))
        {
            var variables = new Dictionary<string, string?>(StringComparer.Ordinal)
            {
                ["CRONWATCH_ENV"] = Fixtures.String(c, "CRONWATCH_ENV"),
                ["APP_ENV"] = Fixtures.String(c, "APP_ENV"),
                ["ASPNETCORE_ENVIRONMENT"] = Fixtures.String(c, "native"),
            };
            foreach (bool given in new[] { false, true })
            {
                string? fallback = null;
                if (given)
                {
                    fallback = variables["ASPNETCORE_ENVIRONMENT"];
                    variables["ASPNETCORE_ENVIRONMENT"] = null;
                }
                string environment = Env.Environment(fallback, name => variables.GetValueOrDefault(name));
                var got = new JsObject()
                    .Set("CRONWATCH_ENV", c.Get("CRONWATCH_ENV")).Set("APP_ENV", c.Get("APP_ENV")).Set("native", c.Get("native"))
                    .Set("environment", environment).Set("development", environment == "development").Set("production", environment == "production");
                failures.Same("environment " + Json.Stringify(c) + (given ? ", the app's own" : ""), got, c);
            }
        }
        failures.Check("client environment");
        Assert.True(failures.Compared > 0);
    }

    /// <summary><c>secrets</c>: a token or secret trimmed as JavaScript trims, blank as unset.</summary>
    [Fact]
    public void Secrets_are_trimmed_as_the_sdk_trims_them()
    {
        var failures = new Fixtures.Failures();
        foreach (JsObject c in Fixtures.Objects(Fixture, "secrets"))
        {
            string value = Fixtures.String(c, "value")!;
            failures.Same("secret " + Json.Stringify(value), new JsObject().Set("value", value).Set("secret", Js.Secret(value)), c);
        }
        failures.Check("client secrets");
        Assert.True(failures.Compared > 0);
    }

    [Fact]
    public async Task Run_ids_are_held_as_the_sdk_holds_them()
    {
        var failures = new Fixtures.Failures();
        var cases = Fixtures.Objects(Fixture, "runIds");
        foreach (string method in new[] { "start", "resume", "recordRun" })
        {
            await using var m = Make(clock: Clock(T0));
            Job job = m.Cw.Job("j");
            foreach (JsObject c in cases.Where(c => Fixtures.String(c, "method") == method))
            {
                string id = Fixtures.String(c, "id")!;
                var got = new JsObject().Set("method", method).Set("id", id);
                try
                {
                    switch (method)
                    {
                        case "start":
                            await (await job.StartAsync(new StartOptions { Id = id })).FinishAsync();
                            break;
                        case "resume":
                            await job.ResumeAsync(id);
                            break;
                        default:
                            await m.Cw.RecordRunAsync(new Run
                            {
                                Id = id,
                                Job = "j",
                                Status = RunStatus.Ok,
                                StartedAt = T0 - 1000,
                                FinishedAt = T0,
                                DurationMs = 1000,
                                Trigger = "run",
                            });
                            break;
                    }
                    got.Set("ok", true);
                }
                catch (CronwatchException e)
                {
                    got.Set("error", e.Message);
                }
                failures.Same("runIds " + method + " " + JsonText.Quote(id), got, c);
            }
            Assert.Empty(m.Errors.Entries);
        }
        failures.Check("client");
        Assert.Equal(36, failures.Compared);
    }

    [Fact]
    public Task Unknown_fields_are_kept_over_the_memory_store() => ReplayUnknownFieldsAsync(new MemoryStore());

    [Fact]
    public async Task Unknown_fields_are_kept_over_sqlite()
    {
        using var dir = new TempDir();
        await using var store = SqlStore.Sqlite(StoreTests.Sqlite(dir.File("cw.db")));
        await ReplayUnknownFieldsAsync(store);
    }

    [Fact]
    public async Task Unknown_fields_are_kept_over_postgres()
    {
        string url = Databases.Require(Databases.PgVariable);
        await using var source = Databases.Postgres(url);
        string prefix = Databases.Prefix("unknown");
        try
        {
            await ReplayUnknownFieldsAsync(SqlStore.Postgres(source).WithPrefix(prefix));
        }
        finally
        {
            await Databases.ExecAsync(source, "DROP TABLE IF EXISTS " + prefix + "jobs, " + prefix + "runs, " + prefix + "state");
        }
    }

    private static async Task ReplayUnknownFieldsAsync(IStore store)
    {
        JsObject f = Fixtures.Object(Fixture, "unknownFields");
        JsObject seed = Fixtures.Object(f, "seed");
        await store.InitAsync();
        await store.UpsertJobAsync(Definition.Of(Fixtures.Object(seed, "definition")), Fixtures.Integer(seed, "createdAt"));
        await store.SetStateAsync(JobState.FromValue(seed.Get("state")));
        foreach (object? run in Fixtures.List(seed, "runs"))
        {
            await store.InsertRunAsync(Run.FromValue(run));
        }

        await using var m = Make(store: store, clock: Clock(T0));
        var failures = new Fixtures.Failures();
        int sent = 0;
        foreach (JsObject step in Fixtures.Objects(f, "steps"))
        {
            string op = Fixtures.String(step, "op")!;
            switch (op)
            {
                case "check":
                    m.Clock.Set(Fixtures.Integer(step, "at"));
                    await m.Cw.CheckAsync();
                    break;
                case "silence":
                    m.Clock.Set(Fixtures.Integer(step, "at"));
                    await m.Cw.SilenceAsync("keep", Fixtures.String(step, "for")!);
                    break;
                case "unsilence":
                    m.Clock.Set(Fixtures.Integer(step, "at"));
                    await m.Cw.UnsilenceAsync("keep");
                    break;
                case "summary":
                    m.Clock.Set(Fixtures.Integer(step, "at"));
                    JobSummary summary = (await m.Cw.JobSummaryAsync("keep"))!;
                    failures.Same(op + " summary", Canonical(summary.ToValue(), sortOpen: true), Canonical(step.Get("summary"), sortOpen: true));
                    break;
                case "declareAndRun":
                    var declared = Fixtures.Object(step, "declared");
                    Assert.Equal("{\"timeout\":\"5m\",\"tags\":[\"a\"]}", declared.ToJson());
                    Job job = m.Cw.Job("keep", new JobOptions { Timeout = "5m", Tags = ["a"] });
                    m.Clock.Set(Fixtures.Integer(step, "startedAt"));
                    RunHandle handle = await job.StartAsync(new StartOptions { Id = Fixtures.String(step, "id")! });
                    m.Clock.Set(Fixtures.Integer(step, "finishedAt"));
                    await handle.FinishAsync(Fixtures.String(step, "output"));
                    break;
                default:
                    failures.Fail("unknownFields: an op this replay does not know: " + op);
                    continue;
            }
            JsObject expect = Fixtures.Object(step, "expect");
            StoredJob? stored = await store.GetJobAsync("keep");
            JobState? state = await store.GetStateAsync("keep");
            var runs = await store.ListRunsAsync("keep", 10);
            var alerts = m.Alerts.List().Skip(sent).Select(a => (object?)a.ToValue()).ToList();
            sent += alerts.Count;
            object? job1 = stored == null ? null : new JsObject()
                .Set("name", stored.Name)
                .Set("definition", stored.Definition.ToObject())
                .Set("createdAt", stored.CreatedAt)
                .Set("updatedAt", stored.UpdatedAt);
            failures.Same(op + " job", Canonical(job1), Canonical(expect.Get("job")));
            failures.Same(op + " state", Canonical(state?.ToValue()), Canonical(expect.Get("state")));
            failures.Same(op + " runs", Canonical(runs.Select(r => (object?)r.ToValue()).ToList()), Canonical(expect.Get("runs")));
            failures.Same(op + " alerts", Canonical(alerts), Canonical(expect.Get("alerts")));
            failures.Same(op + " errors", m.Errors.Entries.Select(e => (object?)(e.Where + ": " + e.Error.Message)).ToList(), expect.Get("errors"));
        }
        failures.Check("client");
        Assert.Equal(26, failures.Compared);
    }

    /// <summary>
    /// The value with every object's keys sorted, so two are compared as JSON values whatever
    /// order a store keeps; with <paramref name="sortOpen"/> a summary's <c>open</c> is sorted
    /// too, compared as a set.
    /// </summary>
    private static object? Canonical(object? v, bool sortOpen = false)
    {
        switch (v)
        {
            case JsObject o:
                var output = new JsObject();
                foreach (string key in o.Keys.OrderBy(k => k, StringComparer.Ordinal))
                {
                    object? value = Canonical(o.Get(key));
                    if (sortOpen && key == "open" && value is List<object?> list)
                    {
                        value = list.OrderBy(x => x as string, StringComparer.Ordinal).ToList();
                    }
                    output.Set(key, value);
                }
                return output;
            case List<object?> list:
                return list.Select(x => Canonical(x)).ToList();
            default:
                return v;
        }
    }
    /// <summary>A channel of the outbox replay: records each alert it is given, and refuses it while its key is down.</summary>
    private sealed class Fake(string name, string key, ISet<string> down, List<JsObject> seen) : IChannel
    {
        public string Name => name;

        public Task SendAsync(Alert alert, ChannelContext context, System.Threading.CancellationToken cancellationToken)
        {
            bool accepted = !down.Contains(key);
            Assert.Null(alert.Owed);
            lock (seen)
            {
                seen.Add(new JsObject()
                    .Set("channel", key)
                    .Set("type", alert.Type.Value)
                    .Set("run", alert.Run?.Id)
                    .Set("accepted", accepted)
                    .Set("errorLength", alert.Run?.Error?.Length)
                    .Set("outputLength", alert.Run?.Output?.Length));
            }
            return accepted ? Task.CompletedTask : Task.FromException(new InvalidOperationException(key + " is down"));
        }
    }

    /// <summary>A held alert as the replay compares it: its type, its run's id, and its <c>owed</c> when present.</summary>
    private static JsObject Held(Alert alert)
    {
        var o = new JsObject().Set("type", alert.Type.Value).Set("run", alert.Run?.Id);
        if (alert.ToValue().Get("owed") is { } owed)
        {
            o.Set("owed", owed);
        }
        return o;
    }

    /// <summary>
    /// <c>outbox</c>: a real client on the memory store with fake channels, some of them down,
    /// through failures, checks, a channel set changed between clients, and an entry an older
    /// release held: which channels each send reaches, what the state holds after, and what was
    /// reported. Channels sent to at once report in no fixed order here, so the reports are
    /// compared sorted.
    /// </summary>
    [Fact]
    public async Task The_outbox_retries_each_alert_to_the_channels_that_still_owe_it()
    {
        var failures = new Fixtures.Failures();
        var scenarios = Fixtures.Objects(Fixture, "outbox");
        foreach (JsObject scenario in scenarios)
        {
            string name = Fixtures.String(scenario, "name")!;
            var store = new MemoryStore();
            var clock = Clock();
            var down = new HashSet<string>(StringComparer.Ordinal);
            var errors = new List<string>();
            var seens = new List<List<JsObject>>();
            CronwatchClient Client(List<string> names)
            {
                List<string> keys = Evaluate.ChannelKeys(names);
                seens.Clear();
                var channels = new List<IChannel>();
                for (int k = 0; k < names.Count; k++)
                {
                    var seen = new List<JsObject>();
                    seens.Add(seen);
                    channels.Add(new Fake(names[k], keys[k], down, seen));
                }
                return new CronwatchClient(new CronwatchOptions
                {
                    Store = store,
                    Clock = clock,
                    Alerts = channels,
                    CronSecret = CronSecret.None,
                    ProcessExitHook = false,
                    OnWarning = _ => { },
                    OnError = (e, where) =>
                    {
                        lock (errors)
                        {
                            errors.Add(where);
                        }
                    },
                });
            }
            var cw = Client(Fixtures.List(scenario, "channels").Select(c => (string)c!).ToList());
            try
            {
                int step = 0;
                foreach (JsObject s in Fixtures.Objects(scenario, "steps"))
                {
                    string op = Fixtures.String(s, "op")!;
                    switch (op)
                    {
                        case "fail":
                        case "ok":
                            {
                                long at = Fixtures.Integer(s, "at");
                                clock.Set(at);
                                RunHandle handle = await cw.Job("nightly").StartAsync(new StartOptions { Id = Fixtures.String(s, "id") });
                                clock.Set(at + 1000);
                                if (op == "ok")
                                {
                                    await handle.FinishAsync("done");
                                }
                                else
                                {
                                    await handle.FailAsync(s.Has("error") ? Fixtures.Expand(s.Get("error")) : "boom");
                                }
                                break;
                            }
                        case "check":
                            clock.Set(Fixtures.Integer(s, "at"));
                            await cw.CheckAsync();
                            break;
                        case "down":
                            down.UnionWith(Fixtures.List(s, "channels").Select(c => (string)c!));
                            break;
                        case "up":
                            down.ExceptWith(Fixtures.List(s, "channels").Select(c => (string)c!));
                            break;
                        case "configure":
                            await cw.DisposeAsync();
                            cw = Client(Fixtures.List(s, "channels").Select(c => (string)c!).ToList());
                            break;
                        case "legacy":
                            {
                                JobState stored = (await store.GetStateAsync("nightly"))!;
                                var plain = stored.Undelivered!.Select(Evaluate.WithoutOwed).ToList();
                                await store.SetStateAsync(stored with { Undelivered = ValueList<Alert>.Of(plain) });
                                break;
                            }
                        default:
                            failures.Fail(name + ": an op this replay does not know: " + op);
                            break;
                    }
                    JobState? state = await store.GetStateAsync("nightly");
                    List<string> reported;
                    lock (errors)
                    {
                        reported = [.. errors];
                        errors.Clear();
                    }
                    var expect = Fixtures.Object(s, "expect");
                    var got = new JsObject()
                        .Set("sent", seens.SelectMany(seen => seen).Select(o => (object?)o).ToList())
                        .Set("undelivered", (state?.Undelivered ?? ValueList<Alert>.Empty).Select(a => (object?)Held(a)).ToList())
                        .Set("sending", (state?.Sending ?? ValueList<SendingAlert>.Empty).Where(e => e.Alert != null).Select(e => (object?)Held(e.Alert!)).ToList())
                        .Set("lastAlertAt", state?.LastAlertAt)
                        .Set("errors", reported.Order(StringComparer.Ordinal).Select(w => (object?)w).ToList());
                    var want = expect.Copy().Set("errors", Fixtures.List(expect, "errors").Select(w => (string)w!).Order(StringComparer.Ordinal).Select(w => (object?)w).ToList());
                    failures.Same(name + ": step " + step++ + " (" + op + ")", got, want);
                    foreach (var seen in seens)
                    {
                        seen.Clear();
                    }
                }
            }
            finally
            {
                await cw.DisposeAsync();
            }
        }
        failures.Check("client outbox");
        Assert.Equal(8, scenarios.Count);
    }
}
