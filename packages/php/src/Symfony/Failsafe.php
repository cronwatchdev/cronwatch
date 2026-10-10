<?php

declare(strict_types=1);

namespace Cronwatch\Symfony;

use Psr\Log\LoggerInterface;

/**
 * Keeps CronWatch's own failures out of the worker it watches: a listener
 * runs through run(), and anything it throws (a client that cannot be
 * built, from a DATABASE_URL CronWatch has no store for or a setting it
 * cannot read, among others) is logged and goes no further, so the message
 * is handled unrecorded instead of stopping messenger:consume.
 *
 * The log line is the one the client's own onError writes. It never asks
 * for the client, whose building may be what failed.
 *
 * @internal
 */
final class Failsafe
{
    public static function run(?LoggerInterface $logger, string $where, callable $fn): void
    {
        try {
            $fn();
        } catch (\Throwable $error) {
            self::report($logger, $error, $where);
        }
    }

    public static function report(?LoggerInterface $logger, \Throwable $error, string $where): void
    {
        $line = "[cronwatch] {$where}: " . get_class($error) . ': ' . $error->getMessage();
        try {
            if ($logger !== null) {
                $logger->error($line, ['exception' => $error]);
                return;
            }
        } catch (\Throwable) {
        }
        error_log($line);
    }
}
