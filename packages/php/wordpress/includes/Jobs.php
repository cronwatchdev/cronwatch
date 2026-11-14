<?php

declare(strict_types=1);

namespace Cronwatch\WordPress;

\defined('ABSPATH') || exit;

use Cronwatch\Js;

/**
 * WP-Cron events as CronWatch jobs. Pure functions of the cron array, so
 * the process running an event and the process running the check declare
 * the same job the same way.
 *
 * A recurring event is a job named "wp:<hook>" whose schedule is "every
 * <interval>s"; one scheduled with arguments (the same hook can run on
 * several schedules, one per set of arguments) is "wp:<hook>:<key>", the key
 * being the first eight hex digits of WordPress's own key for them,
 * md5(serialize($args)). Single events (wp_schedule_single_event) are one
 * job per hook, without a schedule: they are one-offs, not a cadence. When a
 * hook has a recurring event without arguments and single events too, the
 * recurring definition is the job's.
 */
final class Jobs
{
    public const PREFIX = 'wp:';
    public const TAG = 'wp-cron';
    /** The longest job name the library takes. */
    private const MAX = 120;

    /**
     * The job name for a hook: runs of characters a name cannot hold become
     * "-", and a hook that had to be changed or cut gets "-" and a short
     * hash of itself, so two hooks never share a name.
     *
     * @param array<array-key, mixed> $args
     */
    public static function name(string $hook, array $args = [], bool $recurring = false): string
    {
        $suffix = $recurring && $args !== [] ? ':' . substr(md5(serialize($args)), 0, 8) : '';
        $clean = (string) preg_replace('/[^A-Za-z0-9._:-]+/', '-', $hook);
        $room = self::MAX - strlen(self::PREFIX) - strlen($suffix);
        if ($clean === $hook && strlen($clean) <= $room) {
            return self::PREFIX . $clean . $suffix;
        }
        $hash = '-' . substr(md5($hook), 0, 8);
        return self::PREFIX . substr($clean, 0, $room - strlen($hash)) . $hash . $suffix;
    }

    /**
     * Every event in a cron array (_get_cron_array()) as a job: name => [hook,
     * args, recurrence name or null, interval in seconds or null].
     *
     * @param array<int|string, mixed> $crons
     * @param array<string, array{interval?: int}> $schedules wp_get_schedules()
     * @return array<string, array{hook: string, args: array, recurrence: ?string, interval: ?int}>
     */
    public static function fromCron(array $crons, array $schedules = []): array
    {
        $jobs = [];
        foreach ($crons as $timestamp => $hooks) {
            if (!is_array($hooks) || !is_numeric($timestamp)) {
                continue;
            }
            foreach ($hooks as $hook => $events) {
                if (!is_array($events)) {
                    continue;
                }
                foreach ($events as $event) {
                    $args = is_array($event['args'] ?? null) ? $event['args'] : [];
                    $recurrence = is_string($event['schedule'] ?? null) && $event['schedule'] !== '' ? $event['schedule'] : null;
                    $interval = $recurrence === null ? null : (int) ($event['interval'] ?? $schedules[$recurrence]['interval'] ?? 0);
                    self::add($jobs, (string) $hook, $args, $recurrence, $interval ?: null);
                }
            }
        }
        ksort($jobs, SORT_STRING);
        return $jobs;
    }

    /**
     * Adds one event to a map made by fromCron(): a recurring event wins over
     * single events of the same name.
     *
     * @param array<string, array{hook: string, args: array, recurrence: ?string, interval: ?int}> $jobs
     * @param array<array-key, mixed> $args
     */
    public static function add(array &$jobs, string $hook, array $args, ?string $recurrence, ?int $interval): string
    {
        $recurring = $recurrence !== null && $interval !== null && $interval > 0;
        $name = self::name($hook, $args, $recurring);
        if (!$recurring) {
            $jobs[$name] ??= ['hook' => $hook, 'args' => [], 'recurrence' => null, 'interval' => null];
            return $name;
        }
        $jobs[$name] = ['hook' => $hook, 'args' => $args, 'recurrence' => $recurrence, 'interval' => $interval];
        return $name;
    }

    /**
     * The job's options: a description naming the hook, its recurrence and
     * arguments, the wp-cron tag, and for a recurring event its schedule.
     *
     * @param array{hook: string, args: array, recurrence: ?string, interval: ?int} $job
     * @return array<string, mixed>
     */
    public static function options(array $job): array
    {
        $hook = $job['hook'];
        if ($job['interval'] === null) {
            return ['description' => "WP-Cron hook {$hook}, single events", 'tags' => [self::TAG]];
        }
        $args = $job['args'] === [] ? '' : ' with args ' . self::args($job['args']);
        $every = \Cronwatch\Duration::interval($job['interval']);
        return [
            'description' => "WP-Cron hook {$hook}{$args}, {$job['recurrence']} (every {$every})",
            'tags' => [self::TAG],
            'schedule' => "every {$every}",
        ];
    }

    /**
     * Whether a stored job is one the check may forget: a WP-Cron job whose
     * event is gone and that nothing has touched for the whole retention.
     * Every hook WP-Cron ever ran becomes a job, and a plugin that schedules
     * single events under a hook name of their own each (one per post, per
     * order) would otherwise add a job, and a few queries to every check,
     * for ever.
     *
     * Only when all of this holds: the job carries the wp-cron tag (so it is
     * the plugin's, never one declared in code) and no schedule (a recurring
     * event still in the cron array, or one that only just left it, has
     * one); it was last declared more than the retention ago; its newest run
     * (which pruning keeps whatever its age) started more than the retention
     * ago, or it has none, and is not running; and its state holds nothing
     * open, nothing still to send, no run still to judge, and no silence
     * still to come. The caller leaves out the jobs of events in the cron
     * array and the ones declared in this request.
     */
    public static function stale(\Cronwatch\StoredJob $stored, ?\Cronwatch\JobState $state, ?\Cronwatch\Run $last, int|float $now, int|float $retentionMs): bool
    {
        $definition = $stored->definition;
        $tags = $stored->isReadable() ? $definition->get('tags') : null;
        if (!is_array($tags) || !in_array(self::TAG, $tags, true) || $definition->has('schedule')) {
            return false;
        }
        $before = $now - $retentionMs;
        if (!($retentionMs > 0) || !($stored->updatedAt < $before) || !($stored->createdAt < $before)) {
            return false;
        }
        if ($last !== null && ($last->status === \Cronwatch\RunStatus::RUNNING || !($last->startedAt < $before))) {
            return false;
        }
        if ($state === null) {
            return true;
        }
        return $state->open === []
            && ($state->undelivered ?? []) === []
            && ($state->sending ?? []) === []
            && ($state->pendingRuns ?? []) === []
            && ($state->pendingRecovery ?? []) === []
            && !($state->silencedUntil !== null && $state->silencedUntil > $now);
    }

    /** Arguments as JSON for a description, cut to 200 characters; what JSON cannot write is named by its type. */
    private static function args(array $args): string
    {
        try {
            $text = Js::stringify(self::plain($args));
        } catch (\Throwable) {
            $text = '[' . implode(',', array_map('get_debug_type', $args)) . ']';
        }
        return Js::length16($text) > 200 ? Js::head16($text, 197) . '...' : $text;
    }

    private static function plain(mixed $value): mixed
    {
        if (is_array($value)) {
            return array_map(self::plain(...), $value);
        }
        return is_scalar($value) || $value === null ? $value : get_debug_type($value);
    }
}
