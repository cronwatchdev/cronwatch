<?php

declare(strict_types=1);

namespace Cronwatch\Laravel;

use Cronwatch\Cronwatch;
use Illuminate\Contracts\Container\Container;

/**
 * Keeps CronWatch's own failures out of the app it watches: a listener runs
 * through run(), and anything it throws (a client that cannot be built,
 * from a store driver CronWatch has no store for or a setting it cannot
 * read, among others) is reported and goes no further, so the task or the
 * queued job runs unrecorded instead of failing.
 *
 * @internal
 */
final class Failsafe
{
    public static function run(Container $app, string $where, callable $fn): void
    {
        try {
            $fn();
        } catch (\Throwable $error) {
            self::report($app, $error, $where);
        }
    }

    /**
     * To the client's onError when the client was built; otherwise to the
     * app's log, as the client's default onError writes it. It never asks the
     * container for the client, whose building may be what failed.
     */
    public static function report(Container $app, \Throwable $error, string $where): void
    {
        try {
            // resolved() is only true once a build succeeded.
            if ($app->resolved(Cronwatch::class)) {
                $app->make(Cronwatch::class)->onError($error, $where);
                return;
            }
            $app->make('log')->error("[cronwatch] {$where}: " . get_class($error) . ': ' . $error->getMessage(), ['exception' => $error]);
        } catch (\Throwable) {
            error_log("[cronwatch] {$where}: " . get_class($error) . ': ' . $error->getMessage());
        }
    }

}
