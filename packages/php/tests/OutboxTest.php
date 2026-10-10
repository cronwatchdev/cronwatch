<?php

declare(strict_types=1);

namespace Cronwatch\Tests;

use Cronwatch\Alert;
use Cronwatch\Alerts\Custom;
use Cronwatch\Cronwatch;
use Cronwatch\Evaluate;
use Cronwatch\SendingAlert;
use Cronwatch\Store\MemoryStore;
use Cronwatch\Store\SqliteStore;
use Cronwatch\Tests\Support\Capture;
use Cronwatch\Tests\Support\Clock;
use Cronwatch\Tests\Support\FlakyStore;
use PHPUnit\Framework\TestCase;

/**
 * outbox.test.ts: an alert is written with the state that opens its
 * condition, so a process that dies before sending it does not lose it. The
 * processes that die are real ones (workers/dies-sending.php), ended by
 * exit() in the triage call or just after a channel took the alert.
 */
final class OutboxTest extends TestCase
{
    private const T0 = Clock::T0;
    private const MIN = Clock::MIN;

    /** @var list<string> */
    private array $files = [];

    protected function tearDown(): void
    {
        foreach ($this->files as $file) {
            foreach ([$file, "{$file}-wal", "{$file}-shm"] as $f) {
                @unlink($f);
            }
        }
    }

    private function temp(string $suffix): string
    {
        $file = sys_get_temp_dir() . '/cronwatch-outbox-' . getmypid() . '-' . bin2hex(random_bytes(4)) . $suffix;
        $this->files[] = $file;
        return $file;
    }

    /**
     * Runs a failing job in a process that dies while sending its alert.
     *
     * @return array{string, string} the SQLite file and what the channel was given before the process died
     */
    private function dies(string $mode): array
    {
        if (!function_exists('proc_open')) {
            $this->markTestSkipped('needs proc_open');
        }
        $file = $this->temp('.db');
        $marker = $this->temp('.txt');
        $process = proc_open([PHP_BINARY, '-d', 'display_errors=0', __DIR__ . '/workers/dies-sending.php', $mode, $file, (string) self::T0, $marker], [1 => ['pipe', 'w'], 2 => ['pipe', 'w']], $pipes);
        $out = stream_get_contents($pipes[1]) . stream_get_contents($pipes[2]);
        fclose($pipes[1]);
        fclose($pipes[2]);
        $this->assertSame(0, proc_close($process), $out);
        return [$file, is_file($marker) ? (string) file_get_contents($marker) : ''];
    }

    public function testTheWriteThatOpensAConditionHoldsItsAlertSoAProcessThatDiesBeforeSendingItDoesNotLoseIt(): void
    {
        // The process dies while its triage call is out: no channel was ever called.
        [$file, $taken] = $this->dies('triage');
        $this->assertSame('', $taken);
        $store = new SqliteStore($file);
        $state = $store->getState('nightly');
        $this->assertSame(self::T0, $state->open['failed']);
        $this->assertSame([['failed', self::T0, self::T0 + Evaluate::SEND_LEASE_MS]], array_map(fn (SendingAlert $e) => [$e->alert->type, $e->alert->at, $e->until], $state->sending));
        $this->assertFalse($state->sending[0]->alert->triageTried, 'triage is made at send time, never stored here');
        $this->assertSame([], $state->undelivered);

        // Another process's checks leave it alone while its sender's lease runs.
        $clock = new Clock();
        $sent = new Capture();
        $server = new Cronwatch(store: $store, now: $clock, alerts: [$sent], cronSecret: null, triage: fn () => 'The disk is full.');
        $clock->advance(self::MIN);
        $server->check();
        $this->assertSame([], $sent->types());

        // Once it has run out, the next check sends it, triaged, once.
        $clock->set(self::T0 + Evaluate::SEND_LEASE_MS + 1);
        $result = $server->check();
        $this->assertSame(['failed'], array_map(fn (Alert $a) => $a->type, $result->alerts));
        $this->assertSame([['failed', self::T0, 'The disk is full.']], array_map(fn (Alert $a) => [$a->type, $a->at, $a->triage], $sent->alerts));
        $after = $store->getState('nightly');
        $this->assertNull($after->sending, 'the key goes once nothing is being sent');
        $this->assertArrayNotHasKey('sending', $after->toJson());
        $this->assertSame([], $after->undelivered);
        $server->check();
        try {
            $server->run('nightly', function (): never {
                throw new \RuntimeException('again');
            });
        } catch (\RuntimeException) {
        }
        $this->assertSame(['failed'], $sent->types(), 'the condition still alerts once');
    }

    public function testAnAlertAChannelTookJustBeforeItsProcessDiedIsSentAgainAfterTheLeaseAtLeastOnce(): void
    {
        [$file, $taken] = $this->dies('accepted');
        $this->assertSame('failed', $taken);
        $clock = new Clock(self::T0 + Evaluate::SEND_LEASE_MS + 1);
        $sent = new Capture();
        $server = new Cronwatch(store: new SqliteStore($file), now: $clock, alerts: [$sent], cronSecret: null);
        $server->check();
        $this->assertSame(['failed'], $sent->types(), 'sent a second time: the one duplicate a crash can cause');
    }

    public function testWhileAnAlertIsBeingSentNoCheckAnywhereSendsItToo(): void
    {
        $clock = new Clock();
        $shared = new MemoryStore();
        $other = new Capture();
        $server = new Cronwatch(store: $shared, now: $clock, alerts: [$other], cronSecret: null);
        $sent = [];
        $worker = null;
        // While the worker's channel holds the alert, a check in another process and one in the worker itself run.
        $held = new Custom('held', function (Alert $alert) use (&$sent, &$worker, $server, $clock): void {
            if ($sent === []) {
                $clock->advance(self::MIN);
                $server->check();
                $worker->check();
            }
            $sent[] = $alert->type;
        });
        $worker = new Cronwatch(store: $shared, now: $clock, alerts: [$held], cronSecret: null);
        try {
            $worker->run('nightly', function (): never {
                throw new \RuntimeException('x');
            });
        } catch (\RuntimeException) {
        }
        $this->assertSame(['failed'], $sent);
        $this->assertSame([], $other->types());
        $state = $shared->getState('nightly');
        $this->assertNull($state->sending);
        $this->assertSame([], $state->undelivered);
        $this->assertSame(self::T0, $state->lastAlertAt, 'the time the run was judged, as before');
        $clock->set(self::T0 + Evaluate::SEND_LEASE_MS + self::MIN);
        $server->check();
        $worker->check();
        $this->assertSame([], $other->types());
        $this->assertSame(['failed'], $sent);
    }

    public function testAnAlertNoChannelTookMovesFromTheOutboxToTheRetryQueueWithItsTriage(): void
    {
        $shared = new MemoryStore();
        $down = new Custom('down', function (): never {
            throw new \RuntimeException('down');
        });
        $cw = new Cronwatch(store: $shared, now: new Clock(), alerts: [$down], cronSecret: null, onError: function (): void {
        }, triage: fn () => 'Look at the disk.');
        try {
            $cw->run('nightly', function (): never {
                throw new \RuntimeException('x');
            });
        } catch (\RuntimeException) {
        }
        $state = $shared->getState('nightly');
        $this->assertNull($state->sending);
        $this->assertSame([['failed', 'Look at the disk.']], array_map(fn (Alert $a) => [$a->type, $a->triage], $state->undelivered));
    }

    public function testAProcessThatQueuesItsAlertsForACheckElsewhereWritesThemWithTheStateThatOpensTheCondition(): void
    {
        $shared = new MemoryStore();
        $counting = new FlakyStore($shared);
        $writes = 0;
        $counting->hooks['compareAndSetState'] = function (\Closure $next, array $args) use (&$writes) {
            $writes++;
            return $next(...$args);
        };
        $recorder = new Cronwatch(store: $counting, now: new Clock(), deliver: 'check', cronSecret: null);
        try {
            $recorder->run('backup', function (): never {
                throw new \RuntimeException('disk full');
            });
        } catch (\RuntimeException) {
        }
        $state = $shared->getState('backup');
        $this->assertSame(['failed'], array_map(fn (Alert $a) => $a->type, $state->undelivered));
        $this->assertNull($state->sending);
        $this->assertNull($state->pendingRuns);
        $this->assertSame(2, $writes, "the start holds the run; then one write: the failure, its alert, and the run's release together");
    }

    public function testAStateWithOddValuesInItsOutboxIsStillRead(): void
    {
        $state = \Cronwatch\JobState::fromJson(\Cronwatch\Js::parse('{"job":"j","open":{},"consecutiveFailures":0,"silencedUntil":null,"lastAlertAt":null,"sending":[null,"x",{"until":"soon"},{"alert":7}]}'));
        $this->assertCount(4, $state->sending);
        $released = Evaluate::releaseSending($state, self::T0);
        $this->assertNull($released['state']->sending);
        $this->assertSame([], $released['state']->undelivered);
    }

    public function testTwoProcessesCheckingAtOnceRetryAQueuedAlertOnceTheOneWhoseClaimLandsSendsIt(): void
    {
        $shared = new MemoryStore();
        $clock = new Clock();
        $down = true;
        $sent = [];
        $other = null;
        // The second process checks while the first is sending, as two checks at once would.
        $channel = new Custom('flaky', function (Alert $alert) use (&$down, &$sent, &$other): void {
            if ($down) {
                throw new \RuntimeException('down');
            }
            $sent[] = $alert->type;
            if ($other !== null) {
                [$check, $other] = [$other, null];
                $check->check();
            }
        });
        $a = new Cronwatch(store: $shared, now: $clock, alerts: [$channel], cronSecret: null, onError: fn () => null);
        $b = new Cronwatch(store: $shared, now: $clock, alerts: [$channel], cronSecret: null, onError: fn () => null);
        try {
            $a->run('nightly', function (): never {
                throw new \RuntimeException('disk full');
            });
        } catch (\RuntimeException) {
        }
        $this->assertSame(['failed'], array_map(fn (Alert $x) => $x->type, $shared->getState('nightly')->undelivered));
        $down = false;
        $clock->advance(self::MIN);
        $other = $b;
        $a->check();
        $this->assertNull($other, 'the second check ran');
        $this->assertSame(['failed'], $sent);
        $state = $shared->getState('nightly');
        $this->assertSame([], $state->undelivered);
        $this->assertNull($state->sending);
    }

    public function testWhileAJobIsSilencedARetryClaimsOnlyRecoveriesTheRestWaitInTheQueue(): void
    {
        $shared = new MemoryStore();
        $clock = new Clock();
        $down = true;
        $sent = [];
        $channel = new Custom('flaky', function (Alert $alert) use (&$down, &$sent): void {
            if ($down) {
                throw new \RuntimeException('down');
            }
            $sent[] = $alert->type;
        });
        $cw = new Cronwatch(store: $shared, now: $clock, alerts: [$channel], cronSecret: null, onError: fn () => null);
        try {
            $cw->run('nightly', function (): never {
                throw new \RuntimeException('disk full');
            });
        } catch (\RuntimeException) {
        }
        $cw->silence('nightly', '1h');
        $down = false;
        $clock->advance(self::MIN);
        $cw->check();
        $this->assertSame([], $sent);
        $this->assertSame(['failed'], array_map(fn (Alert $x) => $x->type, $shared->getState('nightly')->undelivered));
        $cw->unsilence('nightly');
        $cw->check();
        $this->assertSame(['failed'], $sent);
    }

    public function testAFinishedRunWhoseEvaluationWasNeverWrittenIsEvaluatedByALaterCheckOnce(): void
    {
        $shared = new MemoryStore();
        $contested = new FlakyStore($shared);
        // The finish's state write keeps losing, as if the process stopped between the run's write and its evaluation.
        $refuse = false;
        $contested->hooks['compareAndSetState'] = function (\Closure $next, array $args) use (&$refuse): bool {
            return $refuse ? false : $next(...$args);
        };
        $clock = new Clock();
        $sent = new Capture();
        $cw = new Cronwatch(store: $contested, now: $clock, alerts: [$sent], cronSecret: null, onError: fn () => null);
        $handle = $cw->job('nightly')->start();
        $refuse = true;
        $handle->finish(['error' => new \RuntimeException('disk full')]);
        $refuse = false;
        $this->assertSame('failed', $shared->getRun($handle->id)->status);
        $this->assertSame([], $sent->types());
        $this->assertSame([$handle->id], $shared->getState('nightly')->pendingRuns);
        // Left alone at first, in case its finish is still being written elsewhere.
        $clock->advance(self::MIN);
        $cw->check();
        $this->assertSame([], $sent->types());
        $clock->advance(Evaluate::SEND_LEASE_MS);
        $cw->check();
        $this->assertSame(['failed'], $sent->types());
        $this->assertNull($shared->getState('nightly')->pendingRuns);
        $cw->check();
        $this->assertSame(['failed'], $sent->types());
    }

    public function testAPendingRunTheStoreNoLongerHasIsLetGoAndARunStoredUnjudgedOnPurposeIsNotEvaluated(): void
    {
        $shared = new MemoryStore();
        $clock = new Clock();
        $sent = new Capture();
        $cw = new Cronwatch(store: $shared, now: $clock, alerts: [$sent], cronSecret: null);
        $cw->job('nightly');
        $shared->setState(\Cronwatch\JobState::fromJson(['job' => 'nightly', 'open' => [], 'consecutiveFailures' => 0, 'silencedUntil' => null, 'lastAlertAt' => null, 'pendingRuns' => ['gone']]));
        $cw->check();
        $this->assertNull($shared->getState('nightly')->pendingRuns);
        $run = ['id' => 'imported', 'job' => 'nightly', 'status' => 'running', 'startedAt' => $clock(), 'finishedAt' => null, 'durationMs' => null, 'error' => null, 'output' => null, 'metrics' => [], 'trigger' => 'x'];
        $cw->recordRun($run);
        $this->assertSame(['imported'], $shared->getState('nightly')->pendingRuns);
        $cw->recordRun(['status' => 'failed', 'finishedAt' => $clock(), 'durationMs' => 0, 'error' => 'x'] + $run, evaluate: false);
        $this->assertNull($shared->getState('nightly')->pendingRuns);
        $clock->advance(Evaluate::SEND_LEASE_MS + self::MIN);
        $cw->check();
        $this->assertSame([], $sent->types());
    }
}
