using System;
using System.Collections.Generic;
using System.Threading;
using System.Threading.Tasks;
using Hangfire;
using Hangfire.Storage;

namespace Cronwatch.Hangfire.Tests;

/// <summary>A memory store whose run inserts wait, while held, until released: a store that does not answer.</summary>
internal sealed class Gated : IStore, IUpdateRunIfStore, ICompareAndSetStateStore
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
            await gate.Task.ConfigureAwait(false);
        }
        await _inner.InsertRunAsync(run, cancellationToken).ConfigureAwait(false);
    }

    public Task UpdateRunAsync(Run run, CancellationToken cancellationToken = default) => _inner.UpdateRunAsync(run, cancellationToken);

    public Task<bool> UpdateRunIfAsync(Run run, IReadOnlyList<RunStatus> from, CancellationToken cancellationToken = default) => _inner.UpdateRunIfAsync(run, from, cancellationToken);

    public Task<Run?> GetRunAsync(string id, CancellationToken cancellationToken = default) => _inner.GetRunAsync(id, cancellationToken);

    public Task<IReadOnlyList<Run>> ListRunsAsync(string job, int limit, CancellationToken cancellationToken = default) => _inner.ListRunsAsync(job, limit, cancellationToken);

    public Task<IReadOnlyList<Run>> RunningRunsAsync(CancellationToken cancellationToken = default) => _inner.RunningRunsAsync(cancellationToken);

    public Task<JobState?> GetStateAsync(string job, CancellationToken cancellationToken = default) => _inner.GetStateAsync(job, cancellationToken);

    public Task SetStateAsync(JobState state, CancellationToken cancellationToken = default) => _inner.SetStateAsync(state, cancellationToken);

    public Task<bool> CompareAndSetStateAsync(JobState state, long expected, CancellationToken cancellationToken = default) => _inner.CompareAndSetStateAsync(state, expected, cancellationToken);

    public Task<long> PruneAsync(long before, CancellationToken cancellationToken = default) => _inner.PruneAsync(before, cancellationToken);
}

/// <summary>A Hangfire storage whose connection blocks until <c>blocked</c> is set, then fails.</summary>
internal sealed class HangingStorage(ManualResetEventSlim blocked) : JobStorage
{
    public ManualResetEventSlim Entered { get; } = new();

    public override IMonitoringApi GetMonitoringApi() => throw new NotSupportedException();

    public override IStorageConnection GetConnection()
    {
        Entered.Set();
        blocked.Wait();
        throw new InvalidOperationException("storage down");
    }
}
