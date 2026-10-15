<?php

declare(strict_types=1);

namespace Cronwatch;

/**
 * Pure decisions about a job's health (evaluate.ts). Each function takes the
 * current state and returns the new state plus the alerts that should go
 * out. Nothing here touches a store or a network, which is what makes it
 * testable.
 *
 * @internal
 */
final class Evaluate
{
    public const DEFAULT_GRACE_MS = 10 * 60_000;
    public const DEFAULT_TIMEOUT_MS = 60 * 60_000;
    /** Runs faster than this are never called slow, whatever the baseline says. */
    public const SLOW_FLOOR_MS = 10_000;
    /** How many earlier runs a baseline needs before it is trusted. */
    public const BASELINE_MIN_RUNS = 5;
    /** How many successful runs a baseline looks at, and how many runs a summary covers. */
    public const BASELINE_WINDOW = 20;
    /** The longest duration written: the largest integer JavaScript holds exactly, which every port and store reads back unchanged. */
    public const MAX_DURATION_MS = Js::MAX_SAFE_INTEGER;

    /**
     * How long a run took, from `startedAt` to `finishedAt` (runDuration): 0
     * when it started later, and never more than MAX_DURATION_MS. A foreign
     * row's start near a 64-bit limit must not make a duration no store can
     * write; PHP's int turns to a float past the limit, which the cap brings back.
     */
    public static function runDuration(int|float $startedAt, int|float $finishedAt): int|float
    {
        $ms = $finishedAt - $startedAt;
        if (!($ms > 0)) {
            return 0;
        }
        return $ms >= self::MAX_DURATION_MS ? self::MAX_DURATION_MS : $ms;
    }

    /**
     * When a silence of `ms` from `now` ends (silenceEnd): a whole
     * millisecond, never past MAX_DURATION_MS (2^53 - 1), however long the
     * silence asked for. Every port sharing the store reads it back
     * unchanged, where a larger number could wrap to a time long past and
     * send alerts during the silence.
     */
    public static function silenceEnd(int|float $now, int|float $ms): int|float
    {
        $whole = $ms >= self::MAX_DURATION_MS ? self::MAX_DURATION_MS : (int) floor($ms);
        $end = $now + $whole;
        return $end >= self::MAX_DURATION_MS ? self::MAX_DURATION_MS : $end;
    }

    /**
     * The version a stored state counts as for compareAndSetState
     * (stateVersion): a whole number from 0 to MAX_DURATION_MS (2^53 - 1),
     * else 0, as when it is absent. The SQL stores read it the same way, so a
     * foreign row's 1.5, "x", or -1 is written over by the next update instead
     * of refusing every compare-and-set of its job for good.
     */
    public static function stateVersion(?JobState $state): int
    {
        $version = $state?->version;
        if (is_int($version)) {
            return $version >= 0 && $version <= self::MAX_DURATION_MS ? $version : 0;
        }
        if (is_float($version) && is_finite($version) && floor($version) === $version && $version >= 0 && $version <= self::MAX_DURATION_MS) {
            return (int) $version;
        }
        return 0;
    }

    /**
     * The failures in a row a stored state counts as (failureCount): its
     * consecutiveFailures when that is a whole number, held at
     * MAX_DURATION_MS (2^53 - 1), and 0 when it is negative or not a whole
     * number. A foreign row's count at a 64-bit limit stays at the top
     * instead of turning into a float past it, and a 1.5 or -1 counts as none.
     */
    public static function failureCount(?JobState $state): int
    {
        $count = $state?->consecutiveFailures;
        if (is_int($count)) {
            return $count > 0 ? min($count, self::MAX_DURATION_MS) : 0;
        }
        if (is_float($count) && is_finite($count) && floor($count) === $count && $count > 0) {
            return $count >= self::MAX_DURATION_MS ? self::MAX_DURATION_MS : (int) $count;
        }
        return 0;
    }

    public static function emptyState(string $job): JobState
    {
        return new JobState($job, [], 0, null, null, [], []);
    }

    /**
     * A stored state with every field present, or a fresh one. State written
     * by an older version lacks the newer fields. `sending` is the exception:
     * it is there only while it holds an alert (see holdAlerts).
     *
     * Read leniently, since a foreign, hand-edited, or damaged row must affect
     * only its own job, and the next write puts it right: a state that is not
     * an object (a decoded 5, "x", [], or null) reads as none; the fields
     * inside are read as JobState::fromJson() reads them.
     */
    public static function normalizeState(mixed $state, string $job): JobState
    {
        if (!$state instanceof JobState) {
            $state = JobState::fromStored($state);
        }
        if ($state === null) {
            return self::emptyState($job);
        }
        // A copy, so the fields this release does not know come along.
        $next = clone $state;
        $next->consecutiveFailures = self::failureCount($state);
        $next->pendingRecovery = $state->pendingRecovery ?? [];
        $next->undelivered = $state->undelivered ?? [];
        $next->sending = $state->sending === null || $state->sending === [] ? null : array_values($state->sending);
        $underFloor = array_values(array_filter($state->underFloor ?? [], 'is_string'));
        $next->underFloor = $underFloor === [] ? null : $underFloor;
        $pendingRuns = array_values(array_filter($state->pendingRuns ?? [], 'is_string'));
        $next->pendingRuns = $pendingRuns === [] ? null : $pendingRuns;
        return $next;
    }

    // ------------------------------------------------------------ delivery

    /** Alerts kept per job for retry, and per job being sent; past it the oldest go. */
    public const MAX_UNDELIVERED = 20;

    /**
     * How long an alert in `sending` is left to the process sending it.
     * Longer than any send takes: at most three alerts go out together, each
     * with 25 seconds of triage and 15 of channels.
     */
    public const SEND_LEASE_MS = 5 * 60_000;

    /** Identifies an alert across retries, and in `sending`. */
    public static function alertKey(Alert $alert): string
    {
        return $alert->type . '|' . Js::number($alert->at) . '|' . ($alert->run?->id ?? '');
    }

    /** How much of a held alert's run error and output is kept: 2048 UTF-16 units of each. */
    public const HELD_TEXT_CAP = 2048;

    /**
     * How far past its cap a held text may run and still be kept whole:
     * longer than any marker, so a text trimmed once (its kept part and its
     * marker) is never trimmed again, and the count in its marker stays true.
     */
    public const HELD_TEXT_SLACK = 64;

    /** How much of a held alert's message is kept: its first 4096 UTF-16 units. */
    public const HELD_MESSAGE_CAP = 4096;

    /**
     * A run as an alert held in a job's state keeps it (`sending` and
     * `undelivered`), so the state stays small however much a run logged.
     * Its error keeps its first HELD_TEXT_CAP UTF-16 units and its output its
     * last, each cut so as not to split a surrogate pair, with a line saying
     * how many units were left out: "<head>\n[N more characters trimmed]" and
     * "[N earlier characters trimmed]\n<tail>". A text of at most
     * HELD_TEXT_CAP + HELD_TEXT_SLACK units, or that is not a string (a
     * foreign row's), is kept as it is, and so is every other field. Takes a
     * Run, or a run's JSON as a state holds it, and gives back the same kind.
     * The first send of an alert uses the alert as composed; only a retry
     * reads the held copy.
     *
     * @template T of Run|array|\stdClass
     * @param T $run
     * @return T
     */
    public static function heldRun(Run|array|\stdClass $run): Run|array|\stdClass
    {
        $fields = $run instanceof Run ? ['error' => $run->error, 'output' => $run->output] : Js::fields($run);
        $error = $fields['error'] ?? null;
        $output = $fields['output'] ?? null;
        $trimmedError = is_string($error) && Js::length16($error) > self::HELD_TEXT_CAP + self::HELD_TEXT_SLACK ? self::keepHead($error, self::HELD_TEXT_CAP) : $error;
        $trimmedOutput = is_string($output) && Js::length16($output) > self::HELD_TEXT_CAP + self::HELD_TEXT_SLACK ? self::keepTail($output) : $output;
        if ($trimmedError === $error && $trimmedOutput === $output) {
            return $run;
        }
        if ($run instanceof Run) {
            $copy = clone $run;
            $copy->error = $trimmedError;
            $copy->output = $trimmedOutput;
            return $copy;
        }
        if ($trimmedError !== $error) {
            $fields['error'] = $trimmedError;
        }
        if ($trimmedOutput !== $output) {
            $fields['output'] = $trimmedOutput;
        }
        return $run instanceof \stdClass ? (object) $fields : $fields;
    }

    /**
     * An alert as it is held in state: its run trimmed (heldRun()), and its
     * message, when longer than HELD_MESSAGE_CAP + HELD_TEXT_SLACK units, cut
     * to its first HELD_MESSAGE_CAP like a run's error ("<head>\n[N more
     * characters trimmed]"). Everything else, the title and `owed` included,
     * as it is. The first send uses the alert as composed.
     */
    public static function heldAlert(Alert $alert): Alert
    {
        $json = $alert->stateJson();
        $fields = $json instanceof \stdClass ? get_object_vars($json) : $json;
        $run = $fields['run'] ?? null;
        $isObject = $run instanceof \stdClass || (is_array($run) && $run !== [] && !array_is_list($run));
        $trimmedRun = $isObject ? self::heldRun($run) : $run;
        $message = $fields['message'] ?? null;
        $trimmedMessage = is_string($message) && Js::length16($message) > self::HELD_MESSAGE_CAP + self::HELD_TEXT_SLACK ? self::keepHead($message, self::HELD_MESSAGE_CAP) : $message;
        if ($trimmedRun === $run && $trimmedMessage === $message) {
            return $alert;
        }
        return $alert->withJson(function (array $f) use ($trimmedRun, $trimmedMessage): array {
            $f['run'] = $trimmedRun;
            $f['message'] = $trimmedMessage;
            return $f;
        });
    }

    /** The first `cap` UTF-16 units of `text` (one fewer when a surrogate pair would be split) and a line counting the rest. */
    private static function keepHead(string $text, int $cap): string
    {
        $head = self::wholeHead($text, $cap);
        return $head . "\n[" . (Js::length16($text) - Js::length16($head)) . ' more characters trimmed]';
    }

    /** The last HELD_TEXT_CAP UTF-16 units of `text` (one fewer when a surrogate pair would be split) after a line counting the rest. */
    private static function keepTail(string $text): string
    {
        $tail = self::wholeTail($text, self::HELD_TEXT_CAP);
        return '[' . (Js::length16($text) - Js::length16($tail)) . " earlier characters trimmed]\n" . $tail;
    }

    /** The longest start of `text` of at most `units` UTF-16 units that splits no character. */
    private static function wholeHead(string $text, int $units): string
    {
        if (Js::isAscii($text)) {
            return substr($text, 0, $units);
        }
        $n = strlen($text);
        $i = 0;
        $u = 0;
        while ($i < $n) {
            $lead = ord($text[$i]);
            $width = $lead < 0x80 ? 1 : ($lead < 0xE0 ? 2 : ($lead < 0xF0 ? 3 : 4));
            $size = $width === 4 ? 2 : 1;
            if ($u + $size > $units) {
                break;
            }
            $u += $size;
            $i += $width;
        }
        return substr($text, 0, $i);
    }

    /** The longest end of `text` of at most `units` UTF-16 units that splits no character. */
    private static function wholeTail(string $text, int $units): string
    {
        if (Js::isAscii($text)) {
            return strlen($text) <= $units ? $text : substr($text, -$units);
        }
        $i = strlen($text);
        $u = 0;
        while ($i > 0) {
            $start = $i - 1;
            while ($start > 0 && (ord($text[$start]) & 0xC0) === 0x80) {
                $start--;
            }
            $size = ord($text[$start]) >= 0xF0 ? 2 : 1;
            if ($u + $size > $units) {
                break;
            }
            $u += $size;
            $i = $start;
        }
        return substr($text, $i);
    }

    /**
     * Each configured channel's key, in the order configured: its name, with
     * "#2", "#3", and so on for the second and later channels of the same
     * name, so two webhooks are "webhook" and "webhook#2". A key already
     * given to an earlier channel (a channel named "webhook#2" itself, say)
     * is skipped for the next number up, so every key is different. A held
     * alert names the channels that still owe it by these keys (`owed`).
     *
     * @param list<string> $names
     * @return list<string>
     */
    public static function channelKeys(array $names): array
    {
        $seen = [];
        $taken = [];
        $keys = [];
        foreach ($names as $name) {
            $n = ($seen[$name] ?? 0) + 1;
            $seen[$name] = $n;
            $key = $n === 1 ? $name : "{$name}#{$n}";
            while (isset($taken[$key])) {
                $key = $name . '#' . ++$n;
            }
            $taken[$key] = true;
            $keys[] = $key;
        }
        return $keys;
    }

    /**
     * The channels a held alert is still owed by, of those configured now
     * (`configured`, channelKeys() order): those in its `owed` that are
     * still configured, or every configured one when it has no `owed` list
     * (an alert held by a release before per-channel delivery, or one never
     * sent). An entry of `owed` that is not a string is ignored. None left
     * means the alert is done.
     *
     * @param list<string> $configured
     * @return list<string>
     */
    public static function owedChannels(Alert $alert, array $configured): array
    {
        $owed = self::owedOf($alert);
        if ($owed === null) {
            return $configured;
        }
        return array_values(array_filter($configured, fn (string $key) => in_array($key, $owed, true)));
    }

    /**
     * An alert after a send to the channels in `tried`, of which those in
     * `failed` refused it: null when none is left owing it, else the alert
     * with `owed` set to those that refused, in configured order.
     *
     * @param list<string> $tried
     * @param list<string> $failed
     */
    public static function afterSend(Alert $alert, array $tried, array $failed): ?Alert
    {
        $owed = array_values(array_filter($tried, fn (string $key) => in_array($key, $failed, true)));
        if ($owed === []) {
            return null;
        }
        return $alert->withJson(function (array $f) use ($owed): array {
            $f['owed'] = $owed;
            return $f;
        });
    }

    /** An alert without its `owed` list, as a channel is given it. */
    public static function withoutOwed(Alert $alert): Alert
    {
        $json = $alert->stateJson();
        if (!array_key_exists('owed', $json instanceof \stdClass ? get_object_vars($json) : $json)) {
            return $alert;
        }
        return $alert->withJson(function (array $f): array {
            unset($f['owed']);
            return $f;
        });
    }

    /**
     * A held alert's `owed` list, or null when it has none (or what it holds
     * is not a list, as a damaged row's may be).
     *
     * @return list<mixed>|null
     */
    private static function owedOf(Alert $alert): ?array
    {
        $json = $alert->stateJson();
        $owed = ($json instanceof \stdClass ? get_object_vars($json) : $json)['owed'] ?? null;
        return is_array($owed) && array_is_list($owed) ? $owed : null;
    }

    /**
     * `alerts` added to the undelivered queue, each held as heldAlert()
     * keeps it, with its `owed` list as given: one with the same key as a
     * queued alert replaces it where it stands, the rest go at the end, and
     * only the newest MAX_UNDELIVERED stay. `dropped` counts those let go.
     *
     * @param list<Alert> $alerts
     * @return array{state: JobState, dropped: int}
     */
    public static function queueUndelivered(JobState $state, array $alerts): array
    {
        $next = self::cloneState($state);
        $byKey = [];
        foreach ($alerts as $alert) {
            $byKey[self::alertKey($alert)] = self::heldAlert($alert);
        }
        $queue = [];
        $known = [];
        foreach ($next->undelivered ?? [] as $alert) {
            $key = self::alertKey($alert);
            $queue[] = $byKey[$key] ?? $alert;
            $known[$key] = true;
        }
        foreach ($byKey as $key => $alert) {
            if (!isset($known[(string) $key])) {
                $queue[] = $alert;
            }
        }
        [$next->undelivered, $dropped] = self::newest($queue);
        return ['state' => $next, 'dropped' => $dropped];
    }

    /**
     * The outbox. Alerts just composed are written with the state that opens
     * their condition, before any is sent, so a process that stops part way
     * does not lose them: into `sending`, each with its lease ending at
     * `until`, when this process sends them, or (deliver: "check") straight
     * into the undelivered queue for a check elsewhere. Either way the copy
     * held has its run trimmed (heldAlert()) and no `owed` list, so every
     * configured channel owes it; the first send uses the alerts as composed.
     * `dropped` counts alerts let go past MAX_UNDELIVERED.
     *
     * @param list<Alert> $alerts
     * @return array{state: JobState, dropped: int}
     */
    public static function holdAlerts(JobState $state, array $alerts, int|float $until, bool $deferred): array
    {
        if ($alerts === []) {
            return ['state' => $state, 'dropped' => 0];
        }
        if ($deferred) {
            return self::queueUndelivered($state, $alerts);
        }
        $next = self::cloneState($state);
        $held = [...($next->sending ?? []), ...array_map(fn (Alert $alert) => new SendingAlert($until, self::heldAlert($alert)), $alerts)];
        [$next->sending, $dropped] = self::newest($held);
        return ['state' => $next, 'dropped' => $dropped];
    }

    /**
     * Alerts in `sending` whose lease ran out by `now`: the process sending
     * them stopped before it recorded how the send went. They go to the
     * undelivered queue, where the retry sends them (with triage, which is
     * never stored with them here) or drops them as stale, owed by the
     * channels its `owed` names. An entry without
     * an alert is dropped; one without a numeric `until` counts as run out.
     *
     * @return array{state: JobState, dropped: int}
     */
    public static function releaseSending(JobState $state, int|float $now): array
    {
        $held = [];
        $lapsed = [];
        foreach ($state->sending ?? [] as $entry) {
            if (Js::isNumber($entry->until) && $entry->until > $now) {
                $held[] = $entry;
            } else {
                $lapsed[] = $entry;
            }
        }
        if ($lapsed === []) {
            return ['state' => $state, 'dropped' => 0];
        }
        $next = clone $state;
        $next->sending = $held === [] ? null : $held;
        $alerts = [];
        foreach ($lapsed as $entry) {
            if ($entry->alert !== null) {
                $alerts[] = $entry->alert;
            }
        }
        return self::queueUndelivered($next, $alerts);
    }

    /**
     * How a send went. Delivered alerts (no channel owes them any more) and
     * stale ones leave the queue; failed ones (some channel still owes them,
     * named by their `owed`) replace their queued copy, so a triage made on
     * this attempt and the shrunken `owed` are kept, or join the queue.
     * Every one of them leaves `sending`. lastAlertAt moves to `now` when
     * `reached` (some channel accepted an alert on this attempt; by default,
     * when any was delivered). `dropped` in the result counts alerts let go
     * past MAX_UNDELIVERED.
     *
     * @param list<Alert> $delivered
     * @param list<Alert> $failed
     * @param list<Alert> $stale
     * @return array{state: JobState, dropped: int}
     */
    public static function recordSent(JobState $state, array $delivered, array $failed, array $stale, int|float $now, ?bool $reached = null): array
    {
        $next = self::cloneState($state);
        $done = [];
        foreach ([...$delivered, ...$stale] as $alert) {
            $done[self::alertKey($alert)] = true;
        }
        $next->undelivered = array_values(array_filter($next->undelivered ?? [], fn (Alert $alert) => !isset($done[self::alertKey($alert)])));
        $sent = $done;
        foreach ($failed as $alert) {
            $sent[self::alertKey($alert)] = true;
        }
        $held = array_values(array_filter($next->sending ?? [], fn (SendingAlert $entry) => $entry->alert === null || !isset($sent[self::alertKey($entry->alert)])));
        $next->sending = $held === [] ? null : $held;
        if ($reached ?? $delivered !== []) {
            $next->lastAlertAt = $now;
        }
        return self::queueUndelivered($next, $failed);
    }

    /**
     * A check's claim on the undelivered queue, written before it sends, so
     * two processes checking at once never retry the same alert: the queued
     * alerts that still describe the job move into `sending`, oldest first,
     * each with its lease ending at `until`, as many as `sending` has room
     * for under MAX_UNDELIVERED (the rest wait for the next check); stale ones
     * (staleAlert) leave the queue. While the job is silenced only recoveries
     * are claimed. The process whose write lands sends `claimed`.
     *
     * @return array{state: JobState, claimed: list<Alert>, stale: list<Alert>}
     */
    public static function claimUndelivered(JobState $state, int|float $now, int|float $until): array
    {
        $next = self::cloneState($state);
        $stale = [];
        $fresh = [];
        foreach ($next->undelivered ?? [] as $alert) {
            if (self::staleAlert($alert, $next)) {
                $stale[] = $alert;
            } else {
                $fresh[] = $alert;
            }
        }
        $silenced = self::isSilenced($next, $now);
        $room = max(0, self::MAX_UNDELIVERED - count($next->sending ?? []));
        $claimable = array_values(array_filter($fresh, fn (Alert $alert) => !$silenced || $alert->type === AlertType::RECOVERED));
        $claimed = array_slice($claimable, 0, $room);
        if ($stale === [] && $claimed === []) {
            return ['state' => $state, 'claimed' => [], 'stale' => []];
        }
        $next->undelivered = array_values(array_filter($fresh, fn (Alert $alert) => !in_array($alert, $claimed, true)));
        if ($claimed !== []) {
            $next->sending = [...($next->sending ?? []), ...array_map(fn (Alert $alert) => new SendingAlert($until, $alert), $claimed)];
        }
        return ['state' => $next, 'claimed' => $claimed, 'stale' => $stale];
    }

    /** Runs held in `pendingRuns` per job; past it the oldest go. */
    public const MAX_PENDING_RUNS = 100;

    /**
     * A run just started, held in `pendingRuns` until its finish is
     * evaluated (releaseRun), so a check can evaluate it if the process that
     * finished it stops before writing the evaluation. Written with
     * onRunStart's state. Only the newest MAX_PENDING_RUNS are kept.
     */
    public static function holdRun(JobState $state, string $runId): JobState
    {
        $next = self::cloneState($state);
        $held = array_values(array_filter($next->pendingRuns ?? [], fn (string $id) => $id !== $runId));
        $held[] = $runId;
        $next->pendingRuns = array_slice($held, -self::MAX_PENDING_RUNS);
        return $next;
    }

    /** A run taken out of `pendingRuns`, as its evaluation is written or once it is gone from the store. */
    public static function releaseRun(JobState $state, string $runId): JobState
    {
        $held = $state->pendingRuns ?? [];
        if (!in_array($runId, $held, true)) {
            return $state;
        }
        $next = self::cloneState($state);
        $left = array_values(array_filter($held, fn (string $id) => $id !== $runId));
        $next->pendingRuns = $left === [] ? null : $left;
        return $next;
    }

    /**
     * The newest MAX_UNDELIVERED of `list`, and how many went.
     *
     * @template T
     * @param list<T> $list
     * @return array{list<T>, int}
     */
    private static function newest(array $list): array
    {
        return [array_slice($list, -self::MAX_UNDELIVERED), max(0, count($list) - self::MAX_UNDELIVERED)];
    }

    private static function cloneState(JobState $state): JobState
    {
        return self::normalizeState($state, $state->job);
    }

    private static function openCondition(JobState $state, string $condition, int|float $now): bool
    {
        if (array_key_exists($condition, $state->open)) {
            return false;
        }
        $state->open[$condition] = $now;
        return true;
    }

    /**
     * Every open condition has alerted, so closing one owes a recovered
     * message. It is remembered until a successful run leaves nothing open
     * and sends it.
     */
    private static function closeCondition(JobState $state, string $condition): bool
    {
        if (!array_key_exists($condition, $state->open)) {
            return false;
        }
        unset($state->open[$condition]);
        $state->pendingRecovery ??= [];
        if (!in_array($condition, $state->pendingRecovery, true)) {
            $state->pendingRecovery[] = $condition;
        }
        return true;
    }

    /** @return list<string> */
    public static function openConditions(JobState $state): array
    {
        return array_map('strval', array_keys($state->open));
    }

    public static function graceMs(JobDefinition $def): int|float
    {
        return $def->get('grace') === null ? self::DEFAULT_GRACE_MS : Duration::parse($def->get('grace'), 'grace');
    }

    public static function timeoutMs(JobDefinition $def): int|float
    {
        return $def->get('timeout') === null ? self::DEFAULT_TIMEOUT_MS : Duration::parse($def->get('timeout'), 'timeout');
    }

    /**
     * Slow threshold for a successful run, or null when there is nothing to compare against yet.
     *
     * @param list<Run> $history
     * @return array{thresholdMs: int|float, basis: string}|null
     */
    public static function slowThreshold(JobDefinition $def, array $history): ?array
    {
        if ($def->get('maxDuration') !== null) {
            return ['thresholdMs' => Duration::parse($def->get('maxDuration'), 'maxDuration'), 'basis' => 'maxDuration'];
        }
        $durations = [];
        foreach ($history as $run) {
            if ($run->status === RunStatus::OK && $run->durationMs !== null) {
                $durations[] = $run->durationMs;
                if (count($durations) === self::BASELINE_WINDOW) {
                    break;
                }
            }
        }
        if (count($durations) < self::BASELINE_MIN_RUNS) {
            return null;
        }
        $p95 = Stats::percentile($durations, 95);
        return [
            'thresholdMs' => max(2 * $p95, self::SLOW_FLOOR_MS),
            'basis' => 'twice the p95 of the last ' . count($durations) . ' runs (' . Duration::format($p95) . ')',
        ];
    }

    /**
     * @param list<Run> $history
     * @return list<array{metric: string, value: int|float, limit: int|float, basis: string}>
     */
    public static function budgetBreaches(JobDefinition $def, Run $run, array $history): array
    {
        $breaches = [];
        $budget = is_array($def->get('budget')) ? $def->get('budget') : [];
        foreach (Js::objectKeys($run->metrics) as $metric) {
            $value = $run->metrics[$metric];
            if (array_key_exists($metric, $budget)) {
                $ceiling = $budget[$metric];
                if ($value > $ceiling) {
                    $breaches[] = ['metric' => $metric, 'value' => $value, 'limit' => $ceiling, 'basis' => 'budget'];
                }
                continue;
            }
            $past = [];
            foreach ($history as $r) {
                if ($r->status === RunStatus::OK && Js::isNumber($r->metrics[$metric] ?? null)) {
                    $past[] = $r->metrics[$metric];
                    if (count($past) === self::BASELINE_WINDOW) {
                        break;
                    }
                }
            }
            if (count($past) < self::BASELINE_MIN_RUNS) {
                continue;
            }
            $usual = Stats::median($past);
            if ($usual > 0 && $value > 3 * $usual) {
                $breaches[] = ['metric' => $metric, 'value' => $value, 'limit' => 3 * $usual, 'basis' => 'three times the usual ' . self::formatNumber($usual)];
            }
        }
        return $breaches;
    }

    /**
     * The metrics of a successful run that fell below their floor. A metric
     * with a floor breaches when it reports less than that floor. One without
     * breaches when it reports 0 or less and either it did so on the job's last
     * successful run too (`previous`, the metrics under their floor then), or
     * the earlier successful runs that reported it (at least five, the newest
     * twenty) all reported more than 0. So a job that keeps writing nothing stays
     * under its floor however long it goes on, and a metric that is always 0
     * never alerts.
     *
     * @param list<Run> $history
     * @param list<string> $previous
     * @return list<array{metric: string, value: int|float, limit: int|float, basis: string}>
     */
    public static function floorBreaches(JobDefinition $def, Run $run, array $history, array $previous = []): array
    {
        $breaches = [];
        $floors = is_array($def->get('floor')) ? $def->get('floor') : [];
        foreach (Js::objectKeys($run->metrics) as $metric) {
            $value = $run->metrics[$metric];
            if (array_key_exists($metric, $floors)) {
                $floor = $floors[$metric];
                if ($value < $floor) {
                    $breaches[] = ['metric' => $metric, 'value' => $value, 'limit' => $floor, 'basis' => 'floor'];
                }
                continue;
            }
            if ($value > 0) {
                continue;
            }
            if (in_array($metric, $previous, true)) {
                $breaches[] = ['metric' => $metric, 'value' => $value, 'limit' => 0, 'basis' => '0 or less on the run before too'];
                continue;
            }
            $past = [];
            foreach ($history as $r) {
                if ($r->status === RunStatus::OK && Js::isNumber($r->metrics[$metric] ?? null)) {
                    $past[] = $r->metrics[$metric];
                    if (count($past) === self::BASELINE_WINDOW) {
                        break;
                    }
                }
            }
            if (count($past) < self::BASELINE_MIN_RUNS || array_filter($past, fn ($v) => !($v > 0)) !== []) {
                continue;
            }
            $lowest = min($past);
            $breaches[] = ['metric' => $metric, 'value' => $value, 'limit' => $lowest, 'basis' => 'the last ' . count($past) . ' runs all reported more than 0, the lowest ' . self::formatNumber($lowest)];
        }
        return $breaches;
    }

    /**
     * Whether `history` (newest first) holds a full baseline window of successful runs.
     *
     * @param list<Run> $history
     */
    public static function hasFullBaseline(array $history): bool
    {
        return count(array_filter($history, fn (Run $r) => $r->status === RunStatus::OK)) >= self::BASELINE_WINDOW;
    }

    /**
     * toLocaleString("en-US"): digit groups, and a fraction rounded half up
     * to at most four places. Worked on the shortest decimal digits, as ICU does.
     */
    public static function formatNumber(int|float $n): string
    {
        if (is_float($n) && !is_finite($n)) {
            return ($n < 0 ? '-' : '') . (is_nan($n) ? 'NaN' : "\u{221E}");
        }
        $negative = $n < 0;
        if (is_int($n)) {
            $whole = (string) abs($n);
            $fraction = '';
        } elseif ($n == 0.0) {
            $whole = '0';
            $fraction = '';
        } else {
            [$digits, $point] = Js::decimal(abs((float) $n));
            if ($point >= strlen($digits)) {
                $whole = $digits . str_repeat('0', $point - strlen($digits));
                $fraction = '';
            } elseif ($point > 0) {
                $whole = substr($digits, 0, $point);
                $fraction = substr($digits, $point);
            } else {
                $whole = '0';
                $fraction = str_repeat('0', -$point) . $digits;
            }
            if (strlen($fraction) > 4) {
                $up = (int) $fraction[4] >= 5;
                $fraction = substr($fraction, 0, 4);
                if ($up) {
                    $rounded = str_pad(self::addOne($whole . $fraction), strlen($whole) + 4, '0', STR_PAD_LEFT);
                    $whole = substr($rounded, 0, -4);
                    $fraction = substr($rounded, -4);
                }
            }
            $fraction = rtrim($fraction, '0');
        }
        $groups = [];
        while (strlen($whole) > 3) {
            array_unshift($groups, substr($whole, -3));
            $whole = substr($whole, 0, -3);
        }
        array_unshift($groups, $whole);
        $text = implode(',', $groups);
        if ($fraction !== '') {
            $text .= ".{$fraction}";
        }
        return $negative ? "-{$text}" : $text;
    }

    /** A string of decimal digits plus one, however long. */
    private static function addOne(string $digits): string
    {
        $i = strlen($digits) - 1;
        while ($i >= 0 && $digits[$i] === '9') {
            $digits[$i] = '0';
            $i--;
        }
        if ($i < 0) {
            return '1' . $digits;
        }
        $digits[$i] = (string) ((int) $digits[$i] + 1);
        return ltrim($digits, '0') === '' ? '0' : $digits;
    }

    /** When a run finished, or null while it runs (a run marked timeout has finished). */
    public static function finishedAtOf(Run $run): int|float|null
    {
        return $run->status !== RunStatus::RUNNING && (is_int($run->finishedAt) || is_float($run->finishedAt)) ? $run->finishedAt : null;
    }

    /**
     * Whether `run` brings the schedule up to date at `now`, so missed may
     * close for it: judged with it as the job's last run, as onCheck judges,
     * the run the schedule wants next is not yet past its deadline. A run
     * that started before the fire missed opened for does not cover that
     * fire, however late it finishes, and neither does a run a source reports
     * only once it is over whose start was before it. Every run starting now
     * covers it. A job with no schedule, or one that no longer parses, has
     * nothing due, so any run does.
     */
    public static function catchesUp(JobDefinition $def, Run $run, int|float $now): bool
    {
        $schedule = $def->get('schedule');
        if ($schedule === null || $schedule === '' || $schedule === false || $schedule === 0) {
            return true;
        }
        try {
            $timezone = $def->get('timezone');
            $parsed = Schedule::parse((string) $schedule, is_string($timezone) && $timezone !== '' ? $timezone : null);
            $exp = Schedule::expectation($parsed, $run->startedAt, $run->startedAt, self::graceMs($def), self::finishedAtOf($run));
            return $exp === null || $now <= $exp->deadline;
        } catch (\Throwable) {
            return true;
        }
    }

    /**
     * Called when a run starts, or when a run a source reports is recorded.
     * Missed and stuck are about the absence of a run, so a run starting
     * closes them without an alert; the recovered message waits for a
     * successful finish. Missed closes only when the run catches the
     * schedule up (catchesUp), which a run starting now always does.
     */
    public static function onRunStart(JobDefinition $def, Run $run, JobState $state, int|float $now): JobState
    {
        $next = self::cloneState($state);
        if (self::catchesUp($def, $run, $now)) {
            self::closeCondition($next, Condition::MISSED);
        }
        self::closeCondition($next, Condition::STUCK);
        return $next;
    }

    /**
     * Called when a run finishes with status ok, failed, or timeout. `history`
     * is the job's earlier runs, newest first, not including this one. Missed
     * closes only when the run catches the schedule up (catchesUp): a run
     * that started before the fire missed opened for leaves it open.
     *
     * @param list<Run> $history
     */
    public static function onRunFinish(JobDefinition $def, Run $run, JobState $state, array $history, int|float $now): Evaluation
    {
        $next = self::cloneState($state);
        $alerts = [];

        if ($run->status === RunStatus::OK) {
            $next->consecutiveFailures = 0;
            if (self::catchesUp($def, $run, $now)) {
                self::closeCondition($next, Condition::MISSED);
            }
            self::closeCondition($next, Condition::STUCK);
            self::closeCondition($next, Condition::FAILED);

            $slow = self::slowThreshold($def, $history);
            if ($slow !== null && $run->durationMs !== null && $run->durationMs > $slow['thresholdMs']) {
                if (self::openCondition($next, Condition::SLOW, $now)) {
                    $alerts[] = new AlertDraft(AlertType::SLOW, $run, ['durationMs' => $run->durationMs, 'thresholdMs' => $slow['thresholdMs'], 'basis' => $slow['basis']]);
                }
            } else {
                self::closeCondition($next, Condition::SLOW);
            }

            $breaches = self::budgetBreaches($def, $run, $history);
            if ($breaches !== []) {
                if (self::openCondition($next, Condition::OVER_BUDGET, $now)) {
                    $alerts[] = new AlertDraft(AlertType::OVER_BUDGET, $run, ['breaches' => $breaches]);
                }
            } else {
                self::closeCondition($next, Condition::OVER_BUDGET);
            }

            $short = self::floorBreaches($def, $run, $history, $next->underFloor ?? []);
            if ($short !== []) {
                $next->underFloor = array_column($short, 'metric');
                if (self::openCondition($next, Condition::UNDER_FLOOR, $now)) {
                    $alerts[] = new AlertDraft(AlertType::UNDER_FLOOR, $run, ['breaches' => $short]);
                }
            } else {
                $next->underFloor = null;
                self::closeCondition($next, Condition::UNDER_FLOOR);
            }

            $pending = $next->pendingRecovery ?? [];
            if ($pending !== [] && self::openConditions($next) === []) {
                $alerts[] = new AlertDraft(AlertType::RECOVERED, $run, ['after' => array_values($pending)]);
                $next->pendingRecovery = [];
            }
            return new Evaluation($next, $alerts);
        }

        // failed or timeout
        // Held at the top: a count at the limit neither turns into a float nor passes 2^53 - 1.
        $next->consecutiveFailures = min(self::failureCount($next) + 1, self::MAX_DURATION_MS);
        if (self::catchesUp($def, $run, $now)) {
            self::closeCondition($next, Condition::MISSED);
        }
        $threshold = max(1, $def->get('failuresBeforeAlert') ?? 1);
        $condition = $run->status === RunStatus::TIMEOUT ? Condition::STUCK : Condition::FAILED;
        if ($next->consecutiveFailures >= $threshold) {
            if (self::openCondition($next, $condition, $now)) {
                $alerts[] = new AlertDraft($condition, $run, ['consecutiveFailures' => $next->consecutiveFailures, 'threshold' => $threshold]);
            }
        }
        return new Evaluation($next, $alerts);
    }

    /**
     * Called by check(). Decides whether the schedule has been missed: the
     * run the schedule wants next (see Schedule::expectation()) has not
     * started and its grace has run out. `lastRun` is the most recent run of
     * any status. A job with no schedule is never missed, and one whose
     * schedule was removed while missed was open gets a recovered alert
     * (reason "unscheduled") for missed alone.
     */
    public static function onCheck(JobDefinition $def, StoredJob $stored, ?Run $lastRun, JobState $state, int|float $now): CheckEvaluation
    {
        $next = self::cloneState($state);
        $alerts = [];
        $schedule = $def->get('schedule');
        if ($schedule === null || $schedule === '' || $schedule === false || $schedule === 0) {
            if (array_key_exists(Condition::MISSED, $next->open)) {
                // The schedule went away while missed was open (the job was
                // declared again without one, or a source retired it), so
                // nothing is due any more. Missed closes now with a recovery of
                // its own; other open conditions keep their own rules. Missed is
                // taken out of the pending recovery too, so the next successful
                // run does not name it again.
                $since = $next->open[Condition::MISSED];
                unset($next->open[Condition::MISSED]);
                $next->pendingRecovery = array_values(array_filter($next->pendingRecovery ?? [], fn (string $c) => $c !== Condition::MISSED));
                $alerts[] = new AlertDraft(AlertType::RECOVERED, $lastRun, ['after' => [Condition::MISSED], 'reason' => 'unscheduled', 'since' => $since]);
            }
            return new CheckEvaluation($next, $alerts, null, null);
        }

        $timezone = $def->get('timezone');
        $parsed = Schedule::parse((string) $schedule, is_string($timezone) && $timezone !== '' ? $timezone : null);
        $grace = self::graceMs($def);
        $lastRunAt = $lastRun?->startedAt;
        $exp = Schedule::expectation($parsed, $lastRunAt, $stored->createdAt, $grace, $lastRun === null ? null : self::finishedAtOf($lastRun));
        $nextExpectedAt = $parsed->isInterval() ? $exp?->dueAt : Schedule::nextFire($parsed, $now, null);
        if ($exp === null) {
            return new CheckEvaluation($next, $alerts, $nextExpectedAt, null);
        }

        // An interval's next run is due a period after the last one started, or
        // after it finished when it outlasted the period (intervalDue). If that
        // run is still going, the job is busy, not late; stuck covers one that never ends.
        if ($parsed->isInterval() && $lastRun !== null && $lastRun->status === RunStatus::RUNNING) {
            return new CheckEvaluation($next, $alerts, $nextExpectedAt, $exp->dueAt);
        }

        if ($now > $exp->deadline) {
            if (self::openCondition($next, Condition::MISSED, $now)) {
                $alerts[] = new AlertDraft(AlertType::MISSED, $lastRun, ['dueAt' => $exp->dueAt, 'deadline' => $exp->deadline, 'graceMs' => $grace, 'lastRunAt' => $lastRunAt]);
            }
        } else {
            // A run has started since it opened, or the grace was widened.
            self::closeCondition($next, Condition::MISSED);
        }
        return new CheckEvaluation($next, $alerts, $nextExpectedAt, $exp->dueAt);
    }

    /** Whether a running run has gone on longer than the job's timeout. */
    public static function isStuck(JobDefinition $def, Run $run, int|float $now): bool
    {
        // A completed import stages a running row while its pending marker is written.
        return $run->status === RunStatus::RUNNING && $run->finishedAt === null && $now - $run->startedAt > self::timeoutMs($def);
    }

    /**
     * While a job is silenced nothing new is recorded as an incident:
     * conditions may close (so a job that recovered during the silence shows
     * as healthy) but none may open, so the first problem after the silence
     * ends alerts normally.
     */
    public static function muteOpens(JobState $previous, JobState $next): JobState
    {
        $muted = self::cloneState($next);
        foreach (self::openConditions($muted) as $condition) {
            if (!array_key_exists($condition, $previous->open)) {
                unset($muted->open[$condition]);
            }
        }
        return $muted;
    }

    public static function isSilenced(JobState $state, int|float $now): bool
    {
        return $state->silencedUntil !== null && $state->silencedUntil > $now;
    }

    /**
     * An evaluation as it is saved and sent: while the job was silenced when
     * it began, nothing opens and no new alert is sent, but a recovery is. A
     * condition can only close during a silence if it opened (and alerted)
     * before it, so its recovery closes an incident someone was told about.
     */
    public static function applySilence(JobState $previous, Evaluation $evaluation, int|float $now): Evaluation
    {
        if (!self::isSilenced($previous, $now)) {
            return $evaluation;
        }
        $recoveries = array_values(array_filter($evaluation->alerts, fn (AlertDraft $alert) => $alert->type === AlertType::RECOVERED));
        return new Evaluation(self::muteOpens($previous, $evaluation->state), $recoveries);
    }

    /**
     * Whether an alert waiting to be retried no longer describes the job, so
     * it is dropped rather than sent late. An alert for a condition is stale
     * once that condition has closed, or has closed and opened again (it
     * opened at a time other than the alert's). A recovery is stale when any
     * condition it names is open again; while they all stay closed it is kept.
     * From a foreign or damaged row: an alert whose `at` is not a number, and
     * a recovery whose `details.after` is not a list of strings, are stale.
     */
    public static function staleAlert(Alert $alert, JobState $state): bool
    {
        if ($alert->type === AlertType::RECOVERED) {
            // One whose details say nothing of what it recovers from (a foreign or damaged row's) cannot be judged, and goes.
            $details = $alert->storedField('details');
            $after = $details instanceof \stdClass || is_array($details) ? (Js::fields($details)['after'] ?? null) : null;
            if (!is_array($after) || !array_is_list($after)) {
                return true;
            }
            foreach ($after as $condition) {
                if (!is_string($condition) || array_key_exists($condition, $state->open)) {
                    return true;
                }
            }
            return false;
        }
        // One with no time (a foreign or damaged row's) cannot match an open condition.
        $at = $alert->storedField('at');
        return !Js::isNumber($at) || !array_key_exists($alert->type, $state->open) || $state->open[$alert->type] != $at;
    }

    /** How a job looks at a glance. Silence wins, then stuck, failing, and late. */
    public static function jobHealth(JobDefinition $def, ?Run $lastRun, JobState $state, int|float $now): string
    {
        $open = self::openConditions($state);
        if (self::isSilenced($state, $now)) {
            return JobHealth::SILENCED;
        }
        if (in_array(Condition::STUCK, $open, true) || ($lastRun !== null && self::isStuck($def, $lastRun, $now))) {
            return JobHealth::STUCK;
        }
        if (in_array(Condition::FAILED, $open, true) || ($lastRun !== null && in_array($lastRun->status, [RunStatus::FAILED, RunStatus::TIMEOUT], true))) {
            return JobHealth::FAILING;
        }
        if (in_array(Condition::MISSED, $open, true)) {
            return JobHealth::LATE;
        }
        if ($lastRun === null) {
            return JobHealth::NEVER_RAN;
        }
        return JobHealth::HEALTHY;
    }

    /**
     * A job's summary from its most recent runs (newest first; the first
     * BASELINE_WINDOW are used) and its state. Stats cover runs of any
     * status; the percentiles are over the successful ones among them.
     *
     * @param list<Run> $recent
     */
    public static function summarize(StoredJob $stored, array $recent, JobState $state, int|float|null $nextExpectedAt, int|float $now): JobSummary
    {
        return self::summary($stored, $recent, $state, $nextExpectedAt, fn (?Run $lastRun) => self::jobHealth($stored->definition, $lastRun, $state, $now));
    }

    /**
     * The summary of a job that could not be evaluated, say because its
     * stored schedule no longer parses. It reads nothing from the
     * definition. The job shows as failing (or silenced, while it is), since
     * it needs a look, and nothing is known about when it is next due.
     *
     * @param list<Run> $recent
     */
    public static function unevaluableSummary(StoredJob $stored, array $recent, JobState $state, int|float $now): JobSummary
    {
        return self::summary($stored, $recent, $state, null, fn () => self::isSilenced($state, $now) ? JobHealth::SILENCED : JobHealth::FAILING);
    }

    /**
     * @param list<Run> $recent
     * @param \Closure(?Run): string $health
     */
    private static function summary(StoredJob $stored, array $recent, JobState $state, int|float|null $nextExpectedAt, \Closure $health): JobSummary
    {
        $window = array_slice($recent, 0, self::BASELINE_WINDOW);
        $lastRun = $window[0] ?? null;
        $finished = array_values(array_filter($window, fn (Run $r) => $r->status !== RunStatus::RUNNING));
        $okDurations = [];
        foreach ($window as $r) {
            if ($r->status === RunStatus::OK && $r->durationMs !== null) {
                $okDurations[] = $r->durationMs;
            }
        }
        $ok = count(array_filter($finished, fn (Run $r) => $r->status === RunStatus::OK));
        return new JobSummary(
            name: $stored->name,
            definition: $stored->definition,
            health: $health($lastRun),
            open: self::openConditions($state),
            lastRun: $lastRun,
            nextExpectedAt: $nextExpectedAt,
            consecutiveFailures: $state->consecutiveFailures,
            silencedUntil: $state->silencedUntil,
            stats: [
                'runs' => count($finished),
                'okRate' => $finished === [] ? 1 : $ok / count($finished),
                'p50Ms' => Stats::percentile($okDurations, 50),
                'p95Ms' => Stats::percentile($okDurations, 95),
            ],
        );
    }
}
