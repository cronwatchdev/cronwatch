<?php

declare(strict_types=1);

namespace Cronwatch\Tests;

use Cronwatch\Alert;
use Cronwatch\Alerts\ChannelContext;
use Cronwatch\Alerts\Custom;
use Cronwatch\Cronwatch;
use Cronwatch\Job\JobContext;
use Cronwatch\Js;
use Cronwatch\TriageContext;
use Cronwatch\Tests\Support\Clients;
use Cronwatch\Tests\Support\Clock;
use PHPUnit\Framework\TestCase;

/** client.test.ts, but for the handler, which comes with the web phase. */
final class ClientTest extends TestCase
{
    use Clients;

    private const MIN = Clock::MIN;
    private const HOUR = Clock::HOUR;
    private const T0 = Clock::T0;

    public function testRunRecordsOutputMetricsAndDurationAndReturnsTheResult(): void
    {
        $cw = $this->make();
        $job = $cw->job('report', ['schedule' => '0 2 * * *']);
        $result = $job->run(function (JobContext $j) {
            $j->log('hello', ['n' => 1]);
            $j->metric('rows', 42);
            $this->clock->advance(1500);
            return 'done';
        });
        $this->assertSame('done', $result);
        $run = $cw->runs('report')[0];
        $this->assertSame('ok', $run->status);
        $this->assertSame(1500, $run->durationMs);
        $this->assertSame('hello {"n":1}', $run->output);
        $this->assertSame(['rows' => 42], $run->metrics);
        $summary = $cw->jobSummary('report');
        $this->assertSame('healthy', $summary->health);
        $this->assertSame(Js::dateUtc(2026, 0, 6, 2, 0), $summary->nextExpectedAt);
    }

    public function testMonitorAndCurrentRecordRunsToo(): void
    {
        $cw = $this->make();
        $job = $cw->job('wrapped');
        $task = $job->monitor(function (int $a, int $b): int {
            Cronwatch::current()->log("adding {$a} and {$b}");
            return $a + $b;
        });
        $this->assertSame(5, $task(2, 3));
        $this->assertNull(Cronwatch::current(), 'nothing is current outside a run');
        $this->assertSame('adding 2 and 3', $cw->runs('wrapped')[0]->output);
        $this->assertInstanceOf(\RuntimeException::class, $this->failing(fn () => $job->monitor(self::thrower())()));
        $this->assertSame(['ok'], array_slice(array_map(fn ($r) => $r->status, $cw->runs('wrapped')), 1));
    }

    public function testWrapIsMonitorUnderItsDeprecatedName(): void
    {
        $cw = $this->make();
        $task = $cw->job('old')->wrap(fn (int $a) => $a * 2, 'legacy');
        $this->assertSame(8, $task(4));
        $run = $cw->runs('old')[0];
        $this->assertSame(['ok', 'legacy'], [$run->status, $run->trigger]);
    }

    public function testAThrowingJobIsRecordedAsFailedAlertsAndRethrows(): void
    {
        $cw = $this->make();
        $job = $cw->job('nightly');
        $error = $this->failing(fn () => $job->run(self::thrower('db down')));
        $this->assertSame('db down', $error->getMessage());
        $run = $cw->runs('nightly')[0];
        $this->assertSame('failed', $run->status);
        $this->assertStringStartsWith('RuntimeException: db down', $run->error);
        $this->assertSame(['failed'], $this->capture->types());
        $this->assertStringContainsString('db down', $this->capture->alerts[0]->message);
        $this->assertSame('failing', $cw->jobSummary('nightly')->health);
    }

    public function testAnErrorOrExitOutsideExceptionIsRecordedToo(): void
    {
        $cw = $this->make();
        $error = $this->failing(fn () => $cw->run('typed', fn () => strlen([])));
        $this->assertInstanceOf(\TypeError::class, $error);
        $this->assertStringStartsWith('TypeError: strlen()', $cw->runs('typed')[0]->error);
    }

    public function testExpectTurnsAQuietSuccessIntoAFailure(): void
    {
        $cw = $this->make();
        $job = $cw->job('export', ['expect' => 'wrote']);
        $job->run(fn (JobContext $j) => $j->log('wrote 12 files'));
        $this->assertSame([], $this->capture->types());
        $this->clock->advance(self::HOUR);
        $job->run(fn (JobContext $j) => $j->log('nothing to do'));
        $run = $cw->runs('export')[0];
        $this->assertSame('failed', $run->status);
        $this->assertStringContainsString('did not contain "wrote"', $run->error);
        $this->assertSame(['failed'], $this->capture->types());
        // A returned string counts as output too.
        $job->run(fn () => 'wrote 3 files');
        $this->assertSame(['failed', 'recovered'], $this->capture->types());
    }

    public function testARedactionPcreGivesUpOnIsWithheldAndTheRunIsStillRecorded(): void
    {
        $cw = $this->make();
        $job = $cw->job('export');
        $jit = ini_get('pcre.jit');
        $limit = ini_get('pcre.backtrack_limit');
        try {
            $result = $job->run(function (JobContext $j) {
                $j->log('password=hunter2 ' . str_repeat('a', 200));
                // A host whose PCRE gives up on the redaction patterns.
                ini_set('pcre.jit', '0');
                ini_set('pcre.backtrack_limit', '1');
                return 'done';
            });
        } finally {
            ini_set('pcre.jit', (string) $jit);
            ini_set('pcre.backtrack_limit', (string) $limit);
        }
        $this->assertSame('done', $result);
        $run = $cw->runs('export')[0];
        $this->assertSame('ok', $run->status);
        $this->assertSame('[redacted]', $run->output);
    }

    public function testRunDefinesOnFirstUseAndValidatesNamesAndSchedules(): void
    {
        $cw = $this->make();
        $cw->run('adhoc', fn () => 1, ['schedule' => 'every 5m']);
        $this->assertCount(1, $cw->jobs());
        foreach ([
            'job name' => fn () => $cw->job('bad name!'),
            'not a cron expression' => fn () => $cw->job('x', ['schedule' => 'nope']),
            'grace' => fn () => $cw->job('x', ['grace' => 'soon']),
            'unknown option shedule' => fn () => $cw->job('x', ['shedule' => '@hourly']),
        ] as $expected => $declare) {
            try {
                $declare();
                $this->fail("expected an error mentioning {$expected}");
            } catch (\InvalidArgumentException $error) {
                $this->assertStringContainsString($expected, $error->getMessage());
            }
        }
    }

    public function testCheckFindsAMissedRunOnceAndALaterRunRecovers(): void
    {
        $cw = $this->make();
        $job = $cw->job('sync', ['schedule' => 'every 1h', 'grace' => '10m']);
        $cw->check(); // registers at T0
        $this->clock->advance(30 * self::MIN);
        $this->assertSame([], $cw->check()->alerts);
        $this->clock->set(self::T0 + 70 * self::MIN + 1);
        $r = $cw->check();
        $this->assertSame(['missed'], array_map(fn (Alert $a) => $a->type, $r->alerts));
        $this->assertSame('late', $r->jobs[0]->health);
        $this->assertSame([], $cw->check()->alerts, 'no repeat');
        $job->run(fn () => null);
        $this->assertSame(['missed', 'recovered'], $this->capture->types());
        $this->assertSame('healthy', $cw->jobSummary('sync')->health);
    }

    public function testAJobDeclaredAgainWithoutItsScheduleClosesMissedWithARecoveryOnce(): void
    {
        $cw = $this->make();
        $cw->job('sync', ['schedule' => 'every 1h', 'grace' => '10m']);
        $cw->check();
        $this->clock->set(self::T0 + 70 * self::MIN + 1);
        $this->assertSame(['missed'], array_map(fn (Alert $a) => $a->type, $cw->check()->alerts));
        $job = $cw->job('sync');
        $this->clock->advance(self::MIN);
        $r = $cw->check();
        $this->assertSame(['recovered'], array_map(fn (Alert $a) => $a->type, $r->alerts));
        $alert = $r->alerts[0];
        $this->assertSame('sync is no longer scheduled', $alert->title);
        $this->assertSame('Missed since 2026-01-05 10:40:00 UTC (1m ago). It has no schedule now, so nothing is due; the missed alert is closed.', $alert->message);
        $this->assertSame(['after' => ['missed'], 'reason' => 'unscheduled', 'since' => self::T0 + 70 * self::MIN + 1], $alert->details);
        $this->assertSame('never_ran', $r->jobs[0]->health);
        $this->assertSame([], $cw->check()->alerts, 'no repeat');
        $job->run(fn () => null);
        $this->assertSame(['missed', 'recovered'], $this->capture->types(), 'the next run owes nothing');
    }

    public function testAScheduleRemovedWhileSilencedClosesMissedWithItsRecoveryWhichTheSilenceDoesNotHoldBack(): void
    {
        $cw = $this->make();
        $cw->job('sync', ['schedule' => 'every 1h', 'grace' => '10m']);
        $cw->check();
        $this->clock->set(self::T0 + 70 * self::MIN + 1);
        $cw->check();
        $cw->silence('sync', '1h');
        $cw->job('sync');
        $this->clock->advance(self::MIN);
        $this->assertSame(['recovered'], array_map(fn (Alert $a) => $a->type, $cw->check()->alerts));
        $this->assertSame([], $cw->jobSummary('sync')->open);
        $this->clock->advance(2 * self::HOUR);
        $this->assertSame([], $cw->check()->alerts);
        $this->assertSame(['missed', 'recovered'], $this->capture->types());
    }

    public function testARecoveryForAnAlertSentBeforeTheSilenceIsSentDuringItNewProblemsAreNot(): void
    {
        $cw = $this->make();
        $job = $cw->job('flaky');
        $this->failing(fn () => $job->run(self::thrower('x')));
        $cw->silence('flaky', '1h');
        $job->run(fn () => 'ok');
        $this->assertSame(['failed', 'recovered'], $this->capture->types());
        $this->failing(fn () => $job->run(self::thrower('y')));
        $this->assertSame(['failed', 'recovered'], $this->capture->types());
    }

    public function testAfterAForgetRunAndResumeRunByNameDeclareTheJobAgainAsItWasWrittenNotBare(): void
    {
        $cw = $this->make();
        $options = ['schedule' => '0 2 * * *', 'description' => 'Nightly export'];
        $cw->job('nightly', $options);
        $cw->run('nightly', fn () => null);
        $handle = $cw->job('nightly', $options)->start();
        $cw->forget('nightly');
        $this->assertNull($cw->jobSummary('nightly'));
        $cw->run('nightly', fn () => null);
        $summary = $cw->jobSummary('nightly');
        $this->assertSame('0 2 * * *', $summary->definition->get('schedule'));
        $this->assertSame('Nightly export', $summary->definition->get('description'));
        $cw->forget('nightly');
        $resumed = $cw->resumeRun('nightly', $handle->id);
        $this->assertSame($handle->id, $resumed->id);
        $this->clock->advance(self::HOUR);
        $cw->check();
        $this->assertSame('0 2 * * *', $cw->jobSummary('nightly')->definition->get('schedule'));
        $this->assertNotContains('recovered', $this->capture->types(), "no 'no longer scheduled' recovery");
    }

    public function testAHandleKeptAcrossAForgetDeclaresTheJobAgainOnItsNextRunAndWritesItOnce(): void
    {
        $upserts = 0;
        $base = new \Cronwatch\Store\MemoryStore();
        $store = new \Cronwatch\Tests\Support\DelegatingStore($base);
        $store->hooks['upsertJob'] = function (\Closure $next, array $args) use (&$upserts) {
            $upserts++;
            return $next(...$args);
        };
        $cw = $this->make(['store' => $store]);
        $job = $cw->job('nightly', ['schedule' => '0 2 * * *', 'description' => 'Nightly export']);
        $job->run(fn () => null);
        $this->assertSame(1, $upserts);
        $cw->forget('nightly');
        $this->assertSame([], $cw->definedJobs());

        $job->run(fn () => null);
        $this->assertSame(2, $upserts);
        $this->assertSame(['nightly'], array_map(fn ($d) => $d->get('name'), $cw->definedJobs()), 'declared again, so checks and sources see it');
        $job->run(fn () => null);
        $job->run(fn () => null);
        $this->assertSame(2, $upserts, 'written once, not on every run');
        $this->assertSame('Nightly export', $cw->jobSummary('nightly')->definition->get('description'));

        // Forgotten by another process since: this one's check writes it again.
        $base->deleteJob('nightly');
        $cw->check();
        $this->assertNotNull($base->getJob('nightly'));
    }

    /**
     * PHP runs one thing at a time, so a forget lands in the middle of a
     * check's or a read's writes only when something they call makes it: here
     * the store, as the first declaration is written (or, for jobSummary(),
     * as the store is made ready).
     */
    public function testAForgetThatLandsWhileACheckIsWritingDeclarationsIsNotUndoneByThatCheck(): void
    {
        foreach (['check', 'jobs', 'jobSummary'] as $read) {
            $base = new \Cronwatch\Store\MemoryStore();
            $store = new \Cronwatch\Tests\Support\DelegatingStore($base);
            $cw = $this->make(['store' => $store]);
            $forgot = false;
            $forget = function (\Closure $next, array $args) use (&$forgot, &$cw) {
                $result = $next(...$args);
                if (!$forgot) {
                    $forgot = true;
                    $cw->forget('nightly');
                }
                return $result;
            };
            // Neither is written yet, so the read's own loop is what would write them.
            $store->hooks[$read === 'jobSummary' ? 'init' : 'upsertJob'] = $forget;
            $cw->job('keep', ['schedule' => '0 2 * * *']);
            $cw->job('nightly', ['schedule' => '0 2 * * *']);
            match ($read) {
                'check' => $cw->check(),
                'jobs' => $cw->jobs(),
                default => $cw->jobSummary('nightly'),
            };
            $this->assertTrue($forgot, $read);
            $this->assertSame(['keep'], array_map(fn ($d) => $d->get('name'), $cw->definedJobs()), "{$read}: still forgotten here");
            $this->assertNull($base->getJob('nightly'), "{$read}: not written again");
            $this->assertNull($base->getState('nightly'), "{$read}: no state left behind");
            // Its next run still brings it back.
            $cw->run('nightly', fn () => null);
            $this->assertNotNull($base->getJob('nightly'), $read);
        }
    }

    public function testASilenceEndsOnAWholeMillisecondHeldAtTheLargestSafeInteger(): void
    {
        $cw = $this->make();
        $cw->job('quiet');
        $this->assertSame(9007199254740991, $cw->silence('quiet', '99999999999999999999w')->silencedUntil);
        $this->assertSame(9007199254740991, $cw->silence('quiet', 1e300)->silencedUntil);
        $this->assertSame(self::T0 + 1, $cw->silence('quiet', 1.5)->silencedUntil);
    }

    public function testCheckMarksARunThatNeverFinishedAsStuck(): void
    {
        // A process killed mid-run leaves its run running; another process's check finds it.
        $cw = $this->make();
        $job = $cw->job('long', ['timeout' => '5m']);
        $cw->store->init();
        $cw->store->insertRun(new \Cronwatch\Run('hung', 'long', 'running', self::T0));
        $this->assertSame('running', $cw->runs('long')[0]->status);
        $this->clock->advance(4 * self::MIN);
        $this->assertSame([], $cw->check()->alerts);
        $this->clock->advance(2 * self::MIN);
        $r = $cw->check();
        $this->assertSame(['stuck'], array_map(fn (Alert $a) => $a->type, $r->alerts));
        $this->assertSame('timeout', $cw->runs('long')[0]->status);
        $this->assertSame('stuck', $r->jobs[0]->health);
        $this->assertStringContainsString('never reported finishing', $this->capture->alerts[0]->message);
        unset($job);
    }

    public function testAJobThatHangsOnEveryRunAlertsStuckOnceAndRecoversOnItsFirstSuccess(): void
    {
        $cw = $this->make();
        $job = $cw->job('hangs', ['schedule' => '*/5 * * * *', 'timezone' => 'UTC', 'timeout' => '10m']);
        // Twelve runs, five minutes apart, none of which ever finishes.
        $handles = [];
        for ($i = 0; $i < 12; $i++) {
            $handles[] = $job->start();
            $this->clock->advance(5 * self::MIN);
            $cw->check();
        }
        $this->assertSame(['stuck'], $this->capture->types());
        $this->assertSame('stuck', $cw->jobSummary('hangs')->health);
        // A run that fails alerts as failed by its own rule; stuck stays open beside it.
        $this->failing(fn () => $job->run(self::thrower('boom')));
        $this->assertSame(['stuck', 'failed'], $this->capture->types());
        $open = $cw->jobSummary('hangs')->open;
        sort($open);
        $this->assertSame(['failed', 'stuck'], $open);
        // A hung run that reports failing late is not counted again and alerts nothing.
        $handles[0]->fail(new \RuntimeException('late'));
        $this->assertSame(['stuck', 'failed'], $this->capture->types());
        // The first success closes both with one recovery.
        $job->run(fn () => null);
        $this->assertSame(['stuck', 'failed', 'recovered'], $this->capture->types());
        $after = $this->capture->alerts[2]->details['after'];
        sort($after);
        $this->assertSame(['failed', 'stuck'], $after);
        $this->assertSame([], $cw->jobSummary('hangs')->open);
    }

    public function testAStuckRunThatFinishesOkLateClosesStuckWithARecoveryThoughOtherRunsStartedMeanwhile(): void
    {
        $cw = $this->make();
        $job = $cw->job('slow-queue', ['timeout' => '10m']);
        $first = $job->start();
        $this->clock->advance(11 * self::MIN);
        $cw->check();
        $this->assertSame(['stuck'], $this->capture->types());
        $second = $job->start();
        $this->assertSame(['stuck'], $cw->jobSummary('slow-queue')->open, 'a start leaves stuck open');
        $first->finish();
        $this->assertSame(['stuck', 'recovered'], $this->capture->types());
        $second->finish();
        $this->assertSame(['stuck', 'recovered'], $this->capture->types());
    }

    /**
     * The SDK gives the two writes before a job five seconds and then runs
     * the job unrecorded. PHP cannot abandon a store call under way, so a
     * store that hangs and then answers holds the job back for as long; the
     * run is recorded as usual, and the wait is reported, not hidden. (An
     * unreachable server fails within the deadline instead: the stores'
     * connect timeout.)
     */
    public function testAStoreThatHangsRecordingARunsStartIsReportedOnceItAnswers(): void
    {
        $wall = 0;
        $store = new \Cronwatch\Tests\Support\FlakyStore(new \Cronwatch\Store\MemoryStore());
        $slow = 0;
        $store->hooks['insertRun'] = function (\Closure $next, array $args) use (&$wall, &$slow) {
            $wall += $slow;
            return $next(...$args);
        };
        $cw = $this->make(['store' => $store]);
        (new \ReflectionProperty(Cronwatch::class, 'wall'))->setValue($cw, function () use (&$wall): int {
            return $wall;
        });
        $job = $cw->job('nightly');
        $slow = Cronwatch::START_DEADLINE_MS;
        $this->assertSame('done', $job->run(fn () => 'done'));
        $this->assertSame([], $this->messages(), 'five seconds exactly is inside the deadline');
        $slow = 7_400;
        $this->assertSame('done', $job->run(function (JobContext $j) {
            $j->log('did the work');
            return 'done';
        }));
        $this->assertSame(['recording nightly'], $this->wheres());
        $this->assertSame(['the store took 7s to record the start of nightly, longer than 5s; PHP cannot abandon a store call under way, so the job waited for it'], $this->messages());
        // The run was recorded all the same, and nothing is left running.
        $runs = $cw->runs('nightly');
        $this->assertSame(['ok', 'did the work'], [$runs[0]->status, $runs[0]->output]);
        $this->assertSame([], $store->runningRuns());
        $this->assertSame([], $this->capture->types());
        // A store that fails, as opposed to hangs, is reported as before, once.
        $this->errors = [];
        $store->hooks['insertRun'] = function () use (&$wall): never {
            $wall += 9_000;
            throw new \RuntimeException('store down');
        };
        $job->run(fn () => 'done');
        $this->assertSame(['store down', 'store down'], $this->messages(), 'the start, and the finish that inserts in its place');
    }

    public function testSlowAndOverBudgetAlertsComeFromTheJobsOwnBaseline(): void
    {
        $cw = $this->make();
        $job = $cw->job('agent', ['budget' => ['cost' => 1]]);
        for ($i = 0; $i < 5; $i++) {
            $job->run(function (JobContext $j) {
                $this->clock->advance(1000);
                $j->metrics(['tokens' => 1000, 'cost' => 0.5]);
            });
            $this->clock->advance(self::HOUR);
        }
        $this->assertSame([], $this->capture->types());
        $job->run(function (JobContext $j) {
            $this->clock->advance(15_000);
            $j->metrics(['tokens' => 1000, 'cost' => 0.5]);
        });
        $this->assertSame(['slow'], $this->capture->types());
        $this->clock->advance(self::HOUR);
        $job->run(function (JobContext $j) {
            $this->clock->advance(1000);
            $j->metrics(['tokens' => 5000, 'cost' => 1.2]);
        });
        $this->assertSame(['slow', 'over_budget'], $this->capture->types());
        $last = $this->capture->alerts[1];
        $this->assertStringContainsString('cost: 1.2, limit 1 (budget)', $last->message);
        $this->assertStringContainsString('tokens: 5,000, limit 3,000 (three times the usual 1,000)', $last->message);
        $this->clock->advance(self::HOUR);
        $job->run(function (JobContext $j) {
            $this->clock->advance(1000);
            $j->metrics(['tokens' => 1000, 'cost' => 0.5]);
        });
        $this->assertSame(['slow', 'over_budget', 'recovered'], $this->capture->types());
    }

    public function testAnUnderFloorAlertNamesTheMetricAndWhatItWasJudgedAgainst(): void
    {
        $cw = $this->make();
        $job = $cw->job('import', ['floor' => ['files' => 1]]);
        for ($i = 0; $i < 5; $i++) {
            $job->run(function (JobContext $j) use ($i) {
                $this->clock->advance(1000);
                $j->metrics(['rows' => 4812 + $i, 'files' => 2]);
            });
            $this->clock->advance(self::HOUR);
        }
        $empty = function (JobContext $j) {
            $this->clock->advance(1000);
            $j->metrics(['rows' => 0, 'files' => 0]);
        };
        $job->run($empty);
        $this->assertSame(['under_floor'], $this->capture->types());
        $alert = $this->capture->alerts[0];
        $this->assertSame('import fell short', $alert->title);
        $this->assertStringContainsString('rows: 0 (the last 5 runs all reported more than 0, the lowest 4,812)', $alert->message);
        $this->assertStringContainsString('files: 0, below the floor of 1.', $alert->message);
        $this->clock->advance(self::HOUR);
        $job->run($empty);
        $this->assertSame(['under_floor'], $this->capture->types());
        $this->clock->advance(self::HOUR);
        $job->run(function (JobContext $j) {
            $this->clock->advance(1000);
            $j->metrics(['rows' => 10, 'files' => 1]);
        });
        $this->assertSame(['under_floor', 'recovered'], $this->capture->types());
    }

    public function testSilenceSwallowsAlertsAndNothingOpensUnderneathUnsilenceAlertsAgain(): void
    {
        $cw = $this->make();
        $job = $cw->job('flaky');
        $cw->silence('flaky', '1h');
        $this->failing(fn () => $job->run(self::thrower('x')));
        $this->assertSame([], $this->capture->types());
        $this->assertSame('silenced', $cw->jobSummary('flaky')->health);
        $cw->unsilence('flaky');
        $this->failing(fn () => $job->run(self::thrower('y')));
        $this->assertSame(['failed'], $this->capture->types());
    }

    public function testTriageOutputIsAttachedToFailureAlertsAndNeverBlocksThem(): void
    {
        $cw = $this->make(['triage' => fn (TriageContext $ctx) => "Probably {$ctx->alert->job}'s database."]);
        $this->failing(fn () => $cw->run('t', self::thrower()));
        $this->assertSame("Probably t's database.", $this->capture->alerts[0]->triage);
        $cw2 = $this->make(['triage' => function (): never {
            throw new \RuntimeException('api down');
        }]);
        $this->failing(fn () => $cw2->run('t', self::thrower()));
        $this->assertSame(['failed'], $this->capture->types());
        $this->assertNull($this->capture->alerts[0]->triage);
        $this->assertTrue($this->capture->alerts[0]->triageTried, 'tried, and gave nothing');
        $this->assertSame(['triage for t'], $this->wheres());
    }

    public function testForgetRemovesTheJobAndItsRuns(): void
    {
        $cw = $this->make();
        $cw->run('gone', fn () => null);
        $this->assertCount(1, $cw->jobs());
        $cw->forget('gone');
        $this->assertCount(0, $cw->jobs());
        $this->assertNull($cw->jobSummary('gone'));
    }

    public function testAFailingAlertChannelDoesNotBreakTheRun(): void
    {
        $cw = $this->make(['alerts' => [new Custom('broken', function (): never {
            throw new \RuntimeException('no network');
        })]]);
        $error = $this->failing(fn () => $cw->run('x', self::thrower('job')));
        $this->assertSame('job', $error->getMessage());
        $this->assertSame(['alert channel broken'], $this->wheres());
    }

    public function testPlainCallablesAndCustomChannelsTakeAlerts(): void
    {
        $got = [];
        $cw = $this->make(['alerts' => [
            function (Alert $alert) use (&$got): void {
                $got[] = "plain {$alert->type}";
            },
            new Custom('with-context', function (Alert $alert, ChannelContext $context) use (&$got): void {
                $got[] = "context {$alert->type}";
                $context->onError(new \RuntimeException('one recipient refused'));
            }),
        ]]);
        $this->failing(fn () => $cw->run('c', self::thrower()));
        $this->assertSame(['plain failed', 'context failed'], $got);
        $this->assertSame(['alert channel with-context'], $this->wheres());
        $this->assertSame([], $cw->store->getState('c')->undelivered, 'delivered');
    }
}
