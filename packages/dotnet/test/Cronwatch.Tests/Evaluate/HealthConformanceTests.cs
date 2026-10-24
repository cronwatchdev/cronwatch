using System;
using System.Collections.Generic;
using System.Linq;
using Cronwatch.Internal;
using Xunit;

namespace Cronwatch.Tests;

/// <summary>
/// <c>conformance/health.json</c>: health, summaries, percentiles, state normalization, silence,
/// which queued alerts a retry drops, a run's duration, and a state's version.
/// </summary>
public class HealthConformanceTests
{
    private static List<Run> Runs(object? v) =>
        v is List<object?> list ? list.Where(r => r != null).Select(Run.FromValue).ToList() : [];

    private static JobState State(object? v) => JobState.FromValue(v);

    private static StoredJob Stored(object? v)
    {
        var o = (JsObject)v!;
        return new StoredJob(Fixtures.String(o, "name")!, EvaluateCases.Definition(o.Get("definition")),
            Fixtures.Integer(o, "createdAt"), Fixtures.Integer(o, "updatedAt"));
    }

    private static List<double> Numbers(object? v)
    {
        var output = new List<double>();
        if (v is List<object?> list)
        {
            foreach (var n in list)
            {
                if (JsonText.TryNumber(n, out double d))
                {
                    output.Add(d);
                }
            }
        }
        return output;
    }

    // What a function answers, or "error: <message>" as the fixture writes a throw.
    private static object? OrError(Func<object?> f)
    {
        try
        {
            return f();
        }
        catch (ArgumentException e)
        {
            return "error: " + e.Message;
        }
    }

    [Fact]
    public void Every_section_is_replayed()
    {
        var f = Fixtures.Load("health");
        DurationConformanceTests.Known(f, "jobHealth", "summarize", "percentile", "median", "normalizeState", "muteOpens", "isStuck", "isStranded",
            "unevaluableSummary", "applySilence", "staleAlert", "runDuration", "stateVersion", "failureCount", "silenceEnd", "delivery");
        Known(Fixtures.Object(f, "delivery"), "maxUndelivered", "sendLeaseMs", "maxPendingRuns", "alertKey", "normalizeState",
            "queueUndelivered", "holdAlerts", "releaseSending", "recordSent", "claimUndelivered", "holdRun", "releaseRun",
            "heldTextCap", "heldTextSlack", "heldRun", "heldMessageCap", "heldAlert", "channelKeys", "owedChannels", "afterSend", "withoutOwed");
    }

    /// <summary>Fails when a nested section has a key a replay does not know.</summary>
    internal static void Known(JsObject section, params string[] keys)
    {
        var known = new HashSet<string>(keys, StringComparer.Ordinal);
        foreach (string key in section.Keys)
        {
            Assert.True(known.Contains(key), "the fixture has a section this port does not replay: " + key);
        }
    }

    [Fact]
    public void Every_case_answers_as_the_sdk_answers()
    {
        var f = Fixtures.Load("health");
        var fails = new Fixtures.Failures();
        int cases = 0;
        int i = 0;
        foreach (var c in Fixtures.Objects(f, "jobHealth"))
        {
            cases++;
            var got = OrError(() => Evaluate.JobHealthOf(EvaluateCases.Definition(c.Get("definition")), EvaluateCases.Run(c.Get("lastRun")),
                State(c.Get("state")), Fixtures.Integer(c, "now")).Value);
            fails.Same("jobHealth " + i++, got, c.Get("health"));
        }
        i = 0;
        foreach (var c in Fixtures.Objects(f, "summarize"))
        {
            cases++;
            var got = OrError(() => Evaluate.Summarize(Stored(c.Get("stored")), Runs(c.Get("recent")), State(c.Get("state")),
                Fixtures.OptInteger(c, "nextExpectedAt"), Fixtures.Integer(c, "now")).ToValue());
            fails.Same("summarize " + i++, got, c.Get("summary"));
        }
        foreach (var c in Fixtures.Objects(f, "percentile"))
        {
            cases++;
            double p = Fixtures.Number(c, "p");
            fails.Same("percentile(" + Json.Stringify(c.Get("values")) + ", " + Json.Stringify(p) + ")",
                Stats.Percentile(Numbers(c.Get("values")), p), c.Get("percentile"));
        }
        foreach (var c in Fixtures.Objects(f, "median"))
        {
            cases++;
            fails.Same("median(" + Json.Stringify(c.Get("values")) + ")", Stats.Median(Numbers(c.Get("values"))), c.Get("median"));
        }
        i = 0;
        foreach (var c in Fixtures.Objects(f, "normalizeState"))
        {
            cases++;
            // Any stored JSON value: one that is not an object reads as no state.
            fails.Same("normalizeState " + i++, Evaluate.NormalizeStateValue(c.Get("state"), "j").ToValue(), c.Get("normalized"));
        }
        i = 0;
        foreach (var c in Fixtures.Objects(f, "muteOpens"))
        {
            cases++;
            fails.Same("muteOpens " + i++, Evaluate.MuteOpens(State(c.Get("previous")), State(c.Get("next"))).ToValue(), c.Get("muted"));
        }
        i = 0;
        foreach (var c in Fixtures.Objects(f, "isStuck"))
        {
            cases++;
            var got = OrError(() => Evaluate.IsStuck(EvaluateCases.Definition(c.Get("definition")), EvaluateCases.Run(c.Get("run"))!,
                Fixtures.Integer(c, "now")));
            fails.Same("isStuck " + i++, got, c.Get("stuck"));
        }
        i = 0;
        foreach (var c in Fixtures.Objects(f, "isStranded"))
        {
            cases++;
            fails.Same("isStranded " + i++, Evaluate.IsStranded(EvaluateCases.Run(c.Get("run"))!, Fixtures.Integer(c, "now")), c.Get("stranded"));
        }
        i = 0;
        foreach (var c in Fixtures.Objects(f, "unevaluableSummary"))
        {
            cases++;
            fails.Same("unevaluableSummary " + i++,
                Evaluate.UnevaluableSummary(Stored(c.Get("stored")), Runs(c.Get("recent")), State(c.Get("state")), Fixtures.Integer(c, "now")).ToValue(),
                c.Get("summary"));
        }
        i = 0;
        foreach (var c in Fixtures.Objects(f, "applySilence"))
        {
            cases++;
            var e = Fixtures.Object(c, "evaluation");
            var drafts = Fixtures.List(e, "alerts").Select(EvaluateCases.Draft).ToList();
            var output = Evaluate.ApplySilence(State(c.Get("previous")), new Evaluation(State(e.Get("state")), drafts), Fixtures.Integer(c, "now"));
            fails.Same("applySilence " + i++,
                new JsObject().Set("state", output.State.ToValue()).Set("alerts", output.Alerts.Select(d => (object?)d.ToValue()).ToList()),
                c.Get("result"));
        }
        i = 0;
        foreach (var c in Fixtures.Objects(f, "staleAlert"))
        {
            cases++;
            fails.Same("staleAlert " + i++, Evaluate.StaleAlert(Alert.FromValue(c.Get("alert")), State(c.Get("state"))), c.Get("stale"));
        }
        i = 0;
        foreach (var c in Fixtures.Objects(f, "runDuration"))
        {
            cases++;
            fails.Same("runDuration " + i++, Evaluate.RunDuration(Fixtures.Integer(c, "startedAt"), Fixtures.Integer(c, "finishedAt")), c.Get("durationMs"));
        }
        i = 0;
        foreach (var c in Fixtures.Objects(f, "stateVersion"))
        {
            cases++;
            string text = Fixtures.String(c, "state")!;
            object? parsed = Json.Parse(text);
            long fromValue = Evaluate.StateVersion(parsed is JsObject o ? o.Get("version") : null);
            fails.Same("stateVersion " + i, fromValue, c.Get("version"));
            fails.Same("stateVersion " + i++ + ", read as a state", JobState.FromJson(text).CountedVersion, c.Get("version"));
        }
        // failureCount: a state's failures in a row, read from its JSON text, then a failed run from it.
        const long t0 = 1_767_605_400_000;
        var failedDef = EvaluateCases.Definition(Json.Parse("{\"name\":\"j\",\"failuresBeforeAlert\":3}"));
        var failedRun = Run.FromValue(new JsObject()
            .Set("id", "f").Set("job", "j").Set("status", "failed").Set("startedAt", (double)(t0 - 60_000))
            .Set("finishedAt", (double)(t0 - 59_000)).Set("durationMs", 1000.0).Set("error", "Error: boom")
            .Set("output", null).Set("metrics", new JsObject()).Set("trigger", "run"));
        i = 0;
        foreach (var c in Fixtures.Objects(f, "failureCount"))
        {
            cases++;
            string text = Fixtures.String(c, "state")!;
            var normalized = Evaluate.NormalizeState(JobState.FromJson(text), "j");
            fails.Same("failureCount " + i, normalized.ConsecutiveFailures, c.Get("consecutiveFailures"));
            var output = Evaluate.OnRunFinish(failedDef, failedRun, normalized, [], t0);
            fails.Same("failureCount " + i++ + ", then a failed run",
                new JsObject().Set("state", output.State.ToValue()).Set("alerts", output.Alerts.Select(d => (object?)d.ToValue()).ToList()),
                c.Get("failed"));
        }
        // silenceEnd: a duration as parseDuration reads it, then when the silence ends.
        i = 0;
        foreach (var c in Fixtures.Objects(f, "silenceEnd"))
        {
            cases++;
            double ms = Durations.ParseValue(c.Get("duration"), "silence duration");
            fails.Same("silenceEnd " + i++, Evaluate.SilenceEnd(Fixtures.Integer(c, "now"), ms), c.Get("silencedUntil"));
        }
        cases += Delivery(Fixtures.Object(f, "delivery"), fails);
        Assert.Equal(361, cases);
        fails.Check("health");
    }

    private static List<Alert> Alerts(object? v) =>
        v is List<object?> list ? list.Select(Alert.FromValue).ToList() : [];

    /// <summary>
    /// The fixture's value with each alert written as this port writes one (its keys in the
    /// order <see cref="Alert.ToValue"/> gives), so a state is compared key for key while an
    /// alert is compared field for field.
    /// </summary>
    private static object? AlertsAsWritten(object? v)
    {
        switch (v)
        {
            case JsObject o when o.Has("type") && o.Has("details") && o.Has("title"):
                return Alert.FromValue(o).ToValue();
            case JsObject o:
                var copy = new JsObject();
                foreach (var e in o)
                {
                    copy.Set(e.Key, AlertsAsWritten(e.Value));
                }
                return copy;
            case List<object?> list:
                return list.Select(AlertsAsWritten).ToList();
            default:
                return v;
        }
    }

    private static JsObject Result((JobState State, int Dropped) r) =>
        new JsObject().Set("state", r.State.ToValue()).Set("dropped", r.Dropped);

    /// <summary>The <c>delivery</c> section: the outbox and the retry queue, as each write leaves the state.</summary>
    private static int Delivery(JsObject d, Fixtures.Failures fails)
    {
        Assert.Equal(Evaluate.MaxUndelivered, Fixtures.Integer(d, "maxUndelivered"));
        Assert.Equal(Evaluate.SendLeaseMs, Fixtures.Integer(d, "sendLeaseMs"));
        Assert.Equal(Evaluate.MaxPendingRuns, Fixtures.Integer(d, "maxPendingRuns"));
        int cases = 0;
        int i = 0;
        foreach (var c in Fixtures.Objects(d, "alertKey"))
        {
            cases++;
            var raw = Fixtures.Object(c, "alert");
            string want = Fixtures.String(c, "key")!;
            // An alert's time is a whole millisecond here, so a fractional one is read as the
            // millisecond it falls in (see DESIGN.md), and its key names that millisecond.
            double at = Fixtures.Number(raw, "at");
            if (!Js.IsInteger(at))
            {
                want = want.Replace("|" + Js.FormatNumber(at) + "|", "|" + Js.FormatLong(Js.ToLong(at)) + "|", StringComparison.Ordinal);
            }
            fails.Same("alertKey " + i++, Evaluate.AlertKey(Alert.FromValue(raw)), want);
        }
        i = 0;
        foreach (var c in Fixtures.Objects(d, "normalizeState"))
        {
            cases++;
            fails.Same("delivery normalizeState " + i++, Evaluate.NormalizeState(State(c.Get("state")), "j").ToValue(), AlertsAsWritten(c.Get("normalized")));
        }
        i = 0;
        foreach (var c in Fixtures.Objects(d, "queueUndelivered"))
        {
            cases++;
            fails.Same("queueUndelivered " + i++, Result(Evaluate.QueueUndelivered(State(c.Get("state")), Alerts(c.Get("alerts")))), AlertsAsWritten(c.Get("result")));
        }
        i = 0;
        foreach (var c in Fixtures.Objects(d, "holdAlerts"))
        {
            cases++;
            var got = Evaluate.HoldAlerts(State(c.Get("state")), Alerts(c.Get("alerts")), Fixtures.Integer(c, "until"), c.Get("deferred") is true);
            fails.Same("holdAlerts " + i++, Result(got), AlertsAsWritten(c.Get("result")));
        }
        i = 0;
        foreach (var c in Fixtures.Objects(d, "releaseSending"))
        {
            cases++;
            fails.Same("releaseSending " + i++, Result(Evaluate.ReleaseSending(State(c.Get("state")), Fixtures.Integer(c, "now"))), AlertsAsWritten(c.Get("result")));
        }
        i = 0;
        foreach (var c in Fixtures.Objects(d, "recordSent"))
        {
            cases++;
            bool? reached = c.Has("reached") ? c.Get("reached") is true : null;
            var got = Evaluate.RecordSent(State(c.Get("state")), Alerts(c.Get("delivered")), Alerts(c.Get("failed")), Alerts(c.Get("stale")), Fixtures.Integer(c, "now"), reached);
            fails.Same("recordSent " + i++, Result(got), AlertsAsWritten(c.Get("result")));
        }
        i = 0;
        foreach (var c in Fixtures.Objects(d, "claimUndelivered"))
        {
            cases++;
            var (state, claimed, stale) = Evaluate.ClaimUndelivered(State(c.Get("state")), Fixtures.Integer(c, "now"), Fixtures.Integer(c, "until"));
            var got = new JsObject()
                .Set("state", state.ToValue())
                .Set("claimed", claimed.Select(a => (object?)a.ToValue()).ToList())
                .Set("stale", stale.Select(a => (object?)a.ToValue()).ToList());
            fails.Same("claimUndelivered " + i++, got, AlertsAsWritten(c.Get("result")));
        }
        i = 0;
        foreach (var c in Fixtures.Objects(d, "holdRun"))
        {
            cases++;
            fails.Same("holdRun " + i++, Evaluate.HoldRun(State(c.Get("state")), Fixtures.String(c, "runId")!).ToValue(), AlertsAsWritten(c.Get("result")));
        }
        i = 0;
        foreach (var c in Fixtures.Objects(d, "releaseRun"))
        {
            cases++;
            fails.Same("releaseRun " + i++, Evaluate.ReleaseRun(State(c.Get("state")), Fixtures.String(c, "runId")!).ToValue(), AlertsAsWritten(c.Get("result")));
        }
        return cases + Held(d, fails);
    }

    /// <summary>A text spec: null, a string, a <c>{parts}</c> recipe, or <c>{value}</c> for a value that is not a string.</summary>
    private static object? Spec(object? spec) => spec switch
    {
        null or string => spec,
        JsObject o when o.Has("value") => o.Get("value"),
        _ => Fixtures.Expand(spec),
    };

    /// <summary>A text as the fixture writes a result's: <c>digest()</c> for a string, else <c>{value}</c>.</summary>
    private static object Written(object? v) => v is string s ? Fixtures.Digest(s)! : new JsObject().Set("value", v);

    /// <summary>A run's JSON with its error and output written as the fixture writes them.</summary>
    private static JsObject RunWritten(JsObject run)
    {
        var copy = run.Copy();
        copy.Set("error", Written(run.Get("error")));
        copy.Set("output", Written(run.Get("output")));
        return copy;
    }

    private static List<string> Strings(object? v) => v is List<object?> list ? list.Select(k => (string)k!).ToList() : [];

    /// <summary>The held copies (trimmed runs and messages) and the per-channel bookkeeping.</summary>
    private static int Held(JsObject d, Fixtures.Failures fails)
    {
        Assert.Equal(Evaluate.HeldTextCap, Fixtures.Integer(d, "heldTextCap"));
        Assert.Equal(Evaluate.HeldTextSlack, Fixtures.Integer(d, "heldTextSlack"));
        Assert.Equal(Evaluate.HeldMessageCap, Fixtures.Integer(d, "heldMessageCap"));
        int cases = 0;
        int i = 0;
        foreach (var c in Fixtures.Objects(d, "heldRun"))
        {
            cases++;
            var want = Fixtures.Object(c, "result");
            // The run is the fixture's sample, every other field as the result has it.
            var run = want.Copy().Set("error", Spec(c.Get("error"))).Set("output", Spec(c.Get("output")));
            JsObject held = Evaluate.HeldRunValue(run) ?? run;
            fails.Same("heldRun " + i, RunWritten(held), want);
            // Trimming again changes nothing.
            fails.Same("heldRun again " + i++, Json.Stringify(Evaluate.HeldRunValue(held) ?? held), Json.Stringify(held));
            if (run.Get("error") is null or string && run.Get("output") is null or string)
            {
                // The typed run gives the same.
                fails.Same("HeldRun " + (i - 1), RunWritten(Evaluate.HeldRun(Run.FromValue(run)).ToValue()), want);
            }
        }
        i = 0;
        foreach (var c in Fixtures.Objects(d, "heldAlert"))
        {
            cases++;
            var raw = Fixtures.Object(c, "alert").Copy();
            raw.Set("message", Spec(raw.Get("message")));
            Alert held = Evaluate.HeldAlert(Alert.FromValue(raw));
            JsObject got = held.ToValue();
            got.Set("message", Written(got.Get("message")));
            if (got.Get("run") is JsObject r)
            {
                got.Set("run", RunWritten(r));
            }
            fails.Same("heldAlert " + i, got, Fixtures.Object(c, "result"));
            fails.Same("heldAlert again " + i++, Evaluate.HeldAlert(held).ToJson(), held.ToJson());
        }
        i = 0;
        foreach (var c in Fixtures.Objects(d, "channelKeys"))
        {
            cases++;
            fails.Same("channelKeys " + i++, Evaluate.ChannelKeys(Strings(c.Get("names"))).Select(k => (object?)k).ToList(), c.Get("keys"));
        }
        i = 0;
        foreach (var c in Fixtures.Objects(d, "owedChannels"))
        {
            cases++;
            var got = Evaluate.OwedChannels(Alert.FromValue(c.Get("alert")), Strings(c.Get("configured")));
            fails.Same("owedChannels " + i++, got.Select(k => (object?)k).ToList(), c.Get("owed"));
        }
        i = 0;
        foreach (var c in Fixtures.Objects(d, "afterSend"))
        {
            cases++;
            Alert? got = Evaluate.AfterSend(Alert.FromValue(c.Get("alert")), Strings(c.Get("tried")), Strings(c.Get("failed")));
            fails.Same("afterSend " + i++, got?.ToValue(), c.Get("result") == null ? null : AlertsAsWritten(c.Get("result")));
        }
        i = 0;
        foreach (var c in Fixtures.Objects(d, "withoutOwed"))
        {
            cases++;
            fails.Same("withoutOwed " + i++, Evaluate.WithoutOwed(Alert.FromValue(c.Get("alert"))).ToValue(), AlertsAsWritten(c.Get("result")));
        }
        return cases;
    }
}
