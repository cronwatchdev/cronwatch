<?php

declare(strict_types=1);

namespace Cronwatch\Tests;

use Cronwatch\Evaluate;
use Cronwatch\Evaluation;
use Cronwatch\JobDefinition;
use Cronwatch\JobState;
use Cronwatch\Run;
use Cronwatch\Tests\Support\Clock;
use PHPUnit\Framework\TestCase;

/** The cases of evaluate.test.ts that conformance/ does not already pin. */
final class EvaluateTest extends TestCase
{
    private const T0 = Clock::T0;
    private const HOUR = Clock::HOUR;

    private static function okRun(int $startedAt, array $metrics): Run
    {
        static $n = 0;
        $n++;
        return new Run("r{$n}", 'j', 'ok', $startedAt, $startedAt + 1000, 1000, metrics: $metrics);
    }

    /** @return list<string> */
    private static function types(Evaluation $e): array
    {
        return array_map(fn ($a) => $a->type, $e->alerts);
    }

    public function testStuckStaysOpenThroughLaterStartsFailuresAndTimeoutsAndClosesWithARecoveryOnTheNextSuccess(): void
    {
        $min = Clock::MIN;
        $d = new JobDefinition(['name' => 'j']);
        $run = fn (string $status, int $startedAt): Run => $status === 'timeout'
            ? new Run("s{$startedAt}", 'j', 'timeout', $startedAt)
            : new Run("s{$startedAt}", 'j', $status, $startedAt, $startedAt + 1000, 1000, $status === 'failed' ? 'boom' : null);
        $startNow = fn (JobState $state, int $now): JobState => Evaluate::onRunStart($d, new Run('started', 'j', 'running', $now), $state, $now);
        $stuck = Evaluate::onRunFinish($d, $run('timeout', self::T0 - self::HOUR), Evaluate::emptyState('j'), [], self::T0);
        $this->assertSame(['stuck'], self::types($stuck));
        // The next run starting closes nothing: it may hang as the last one did.
        $started = $startNow($stuck->state, self::T0 + $min);
        $this->assertSame(self::T0, $started->open['stuck']);
        $this->assertSame([], $started->pendingRecovery ?? []);
        // It times out too: stuck is already open, so there is no second alert.
        $again = Evaluate::onRunFinish($d, $run('timeout', self::T0 + $min), $started, [], self::T0 + self::HOUR);
        $this->assertSame([], $again->alerts);
        $this->assertSame(self::T0, $again->state->open['stuck'], 'open since the first time');
        $this->assertSame(2, $again->state->consecutiveFailures);
        // A run that fails opens failed by its own rule and leaves stuck open.
        $failed = Evaluate::onRunFinish($d, $run('failed', self::T0 + self::HOUR), $startNow($again->state, self::T0 + self::HOUR), [], self::T0 + self::HOUR + 1000);
        $this->assertSame(['failed'], self::types($failed));
        $this->assertSame(self::T0, $failed->state->open['stuck']);
        // The first success closes both, with one recovery naming both.
        $ok = Evaluate::onRunFinish($d, $run('ok', self::T0 + 2 * self::HOUR), $startNow($failed->state, self::T0 + 2 * self::HOUR), [], self::T0 + 2 * self::HOUR + 1000);
        $this->assertSame(['recovered'], self::types($ok));
        $after = $ok->alerts[0]->details['after'];
        sort($after);
        $this->assertSame(['failed', 'stuck'], $after);
        $this->assertSame([], $ok->state->open);
        $this->assertSame([], $ok->state->pendingRecovery ?? []);
    }

    public function testFloorsAFloorOrZeroAfterFiveRunsThatAllReportedMore(): void
    {
        $floored = new JobDefinition(['name' => 'j', 'floor' => ['rows' => 10]]);
        $short = Evaluate::onRunFinish($floored, self::okRun(self::T0, ['rows' => 9]), Evaluate::emptyState('j'), [], self::T0 + 1000);
        $this->assertSame(['under_floor'], self::types($short));
        $this->assertSame([['metric' => 'rows', 'value' => 9, 'limit' => 10, 'basis' => 'floor']], $short->alerts[0]->details['breaches']);
        $back = Evaluate::onRunFinish($floored, self::okRun(self::T0 + self::HOUR, ['rows' => 10]), $short->state, [], self::T0 + self::HOUR + 1000);
        $this->assertSame(['recovered'], self::types($back));
        $this->assertNull($back->state->underFloor);

        $bare = new JobDefinition(['name' => 'j']);
        $history = array_map(fn (int $i) => self::okRun(self::T0 - $i * self::HOUR, ['rows' => 100 * $i, 'errors' => 0]), [1, 2, 3, 4, 5]);
        $this->assertSame([], Evaluate::onRunFinish($bare, self::okRun(self::T0, ['rows' => 0]), Evaluate::emptyState('j'), array_slice($history, 1), self::T0)->alerts, 'four runs are not a baseline');
        $this->assertSame([], Evaluate::onRunFinish($bare, self::okRun(self::T0, ['rows' => 1, 'errors' => 0]), Evaluate::emptyState('j'), $history, self::T0)->alerts, 'an always-0 metric never alerts');
        $zero = Evaluate::onRunFinish($bare, self::okRun(self::T0, ['rows' => 0, 'errors' => 0]), Evaluate::emptyState('j'), $history, self::T0);
        $this->assertSame([['metric' => 'rows', 'value' => 0, 'limit' => 100, 'basis' => 'the last 5 runs all reported more than 0, the lowest 100']], $zero->alerts[0]->details['breaches']);
        $this->assertSame(['rows'], $zero->state->underFloor);
        $this->assertSame(['rows'], JobState::fromJson($zero->state->toJson())->underFloor);

        // A job that keeps writing nothing stays open, past the point where its zeros are all the history there is.
        $state = $zero->state;
        $runs = $history;
        for ($i = 1; $i <= 30; $i++) {
            array_unshift($runs, self::okRun(self::T0 + ($i - 1) * self::HOUR, ['rows' => 0, 'errors' => 0]));
            $next = Evaluate::onRunFinish($bare, self::okRun(self::T0 + $i * self::HOUR, ['rows' => 0, 'errors' => 0]), $state, array_slice($runs, 0, 25), self::T0 + $i * self::HOUR);
            $this->assertSame([], $next->alerts);
            $this->assertSame(self::T0, $next->state->open['under_floor']);
            $state = $next->state;
        }
        $recovered = Evaluate::onRunFinish($bare, self::okRun(self::T0 + 31 * self::HOUR, ['rows' => 5, 'errors' => 0]), $state, array_slice($runs, 0, 25), self::T0 + 31 * self::HOUR);
        $this->assertSame(['recovered'], self::types($recovered));
        $this->assertSame(['under_floor'], $recovered->alerts[0]->details['after']);

        // A metric that has reported 0 before is judged as usual for it, and a floor of 0 turns the check off.
        $mixed = [...array_slice($history, 0, 4), self::okRun(self::T0 - 6 * self::HOUR, ['rows' => 0])];
        $this->assertSame([], Evaluate::onRunFinish($bare, self::okRun(self::T0, ['rows' => 0]), Evaluate::emptyState('j'), $mixed, self::T0)->alerts);
        $this->assertSame([], Evaluate::onRunFinish(new JobDefinition(['name' => 'j', 'floor' => ['rows' => 0]]), self::okRun(self::T0, ['rows' => 0]), Evaluate::emptyState('j'), $history, self::T0)->alerts);
    }
}
