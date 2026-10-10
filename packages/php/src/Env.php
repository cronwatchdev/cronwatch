<?php

declare(strict_types=1);

namespace Cronwatch;

/**
 * The environment, read in one place (the SDK reads NODE_ENV). PHP has no
 * single convention, so the first of CRONWATCH_ENV, APP_ENV (Laravel,
 * Symfony, Craft) and WP_ENVIRONMENT_TYPE (WordPress) whose value, trimmed,
 * is not empty is used, lowercased (a value of only spaces counts as
 * unset), from getenv(), $_ENV, or $_SERVER, since frameworks that read a
 * .env file put its values in one of those; failing those, what a framework
 * integration names with setFallback() (the WordPress plugin gives
 * wp_get_environment_type(), which also reads the constant of that name).
 * "development", "dev", and "local" are development (Symfony's "dev" and
 * Laravel's "local" included); "production" and "prod" are production;
 * anything else ("test", "testing", "staging") is neither, so a test run or
 * CI needs CRON_SECRET or a token, as production does.
 *
 * It decides whether the in-memory store warns that it forgets, and whether
 * the dashboard makes a development token when none is configured.
 *
 * @internal
 */
final class Env
{
    private const VARIABLES = ['CRONWATCH_ENV', 'APP_ENV', 'WP_ENVIRONMENT_TYPE'];

    private static ?\Closure $fallback = null;

    public static function read(string $name): ?string
    {
        $value = getenv($name);
        if (is_string($value) && $value !== '') {
            return $value;
        }
        foreach ([$_ENV, $_SERVER] as $source) {
            if (isset($source[$name]) && is_string($source[$name]) && $source[$name] !== '') {
                return $source[$name];
            }
        }
        return null;
    }

    /**
     * The words Laravel's env() reads as something other than text (null,
     * true, false, empty, with or without parentheses, in any case). Laravel
     * hands the config null, a bool, or "" for them while the variable still
     * holds the word, so a secret holding one counts as not set rather than
     * as the password "null".
     */
    private const NOT_A_SECRET = ['null', '(null)', 'true', '(true)', 'false', '(false)', 'empty', '(empty)'];

    /**
     * A secret from the environment (CRONWATCH_TOKEN, CRON_SECRET), from the
     * first of getenv(), $_ENV, and $_SERVER that holds one, trimmed (as
     * JavaScript's trim() trims: a value read from a file often ends in a
     * newline that no header can carry, since HTTP strips the whitespace
     * around a header's value), or null. A value that is empty or only
     * whitespace, or one of the words Laravel's env() reads as null, true,
     * false, or empty, counts as unset, so the dashboard and handlers fail
     * closed.
     */
    public static function secret(string $name): ?string
    {
        $value = getenv($name);
        if (is_string($value) && ($text = self::secretText($value)) !== null) {
            return $text;
        }
        foreach ([$_ENV, $_SERVER] as $source) {
            if (isset($source[$name]) && is_string($source[$name]) && ($text = self::secretText($source[$name])) !== null) {
                return $text;
            }
        }
        return null;
    }

    /**
     * A secret's text, trimmed as secret() trims it, or null when it counts
     * as unset: empty, only whitespace, or one of the words Laravel's env()
     * reads as null, true, false, or empty (see secret()). For a framework
     * integration reading its own config before it falls back to the
     * environment.
     */
    public static function secretText(?string $value): ?string
    {
        $text = $value === null ? '' : Js::trim($value);
        if ($text === '' || in_array(strtolower($text), self::NOT_A_SECRET, true)) {
            return null;
        }
        return $text;
    }

    /**
     * A token or secret given in code: a string, null (the opt-out), false
     * (the deprecated opt-out, until 2.0) or FromEnv::Read. A string is
     * trimmed, as secret() trims, and one that is empty or only whitespace
     * comes back as "", which each option reads as not given. Anything else (true, a number, an array, an object) throws
     * a TypeError naming the option, so it never becomes a password, as a
     * caller without strict_types would otherwise make 5 the password "5".
     */
    public static function secretOption(mixed $value, string $what): string|FromEnv|false|null
    {
        if ($value === null || $value === false || $value === FromEnv::Read) {
            return $value;
        }
        if (!is_string($value)) {
            $type = match (true) {
                is_bool($value) => 'boolean',
                is_int($value), is_float($value) => 'number',
                is_array($value) => 'an array',
                default => 'object',
            };
            throw new \TypeError("{$what} must be a string, or null to opt out, not {$type}");
        }
        return Js::trim($value);
    }

    /** Whether text is empty or only whitespace, as JavaScript's trim() sees it. */
    public static function blank(string $value): bool
    {
        return Js::trim($value) === '';
    }

    /** What names the environment when no variable does (a framework's own setting), or null for nothing. */
    public static function setFallback(?callable $read): void
    {
        self::$fallback = $read === null ? null : \Closure::fromCallable($read);
    }

    /**
     * The environment's name from the values that name it, in order, as every
     * CronWatch library reads it: the first that holds more than whitespace,
     * trimmed and lowercased, with "prod" read as "production" and "dev" and
     * "local" as "development"; "" when none is set. "test" and "testing" are
     * not development: a test run or CI needs CRON_SECRET or a token, as
     * production does.
     *
     * @param list<mixed> $values
     */
    public static function environmentOf(array $values): string
    {
        foreach ($values as $value) {
            $name = is_string($value) ? strtolower(Js::trim($value)) : '';
            if ($name === '') {
                continue;
            }
            return match ($name) {
                'prod' => 'production',
                'dev', 'local' => 'development',
                default => $name,
            };
        }
        return '';
    }

    /**
     * The environment's name (environmentOf) from CRONWATCH_ENV, APP_ENV, and
     * WP_ENVIRONMENT_TYPE, and failing those what the framework names (see
     * setFallback()); "" when nothing does.
     */
    public static function environment(): string
    {
        $values = array_map(self::read(...), self::VARIABLES);
        if (self::$fallback !== null) {
            try {
                $values[] = (self::$fallback)();
            } catch (\Throwable) {
                // A fallback that throws names nothing.
            }
        }
        return self::environmentOf($values);
    }

    public static function isProduction(): bool
    {
        return self::environment() === 'production';
    }

    public static function isDevelopment(): bool
    {
        return self::environment() === 'development';
    }
}
