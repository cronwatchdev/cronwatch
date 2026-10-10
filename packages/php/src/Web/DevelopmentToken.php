<?php

declare(strict_types=1);

namespace Cronwatch\Web;

/**
 * The dashboard's development token (Dashboard, token:), made once and kept
 * in a file so that every PHP request (each a process of its own under
 * PHP-FPM) asks for the same one. Only a development server with no token
 * configured reaches it; a host that always gives the dashboard a token, or
 * false, never does, so it may leave this file out (the WordPress plugin's
 * zip does).
 *
 * @internal
 */
final class DevelopmentToken
{
    /**
     * The token, and whether this request made it. `file` is where it is
     * kept (null: a file in a directory of this user's own under the
     * system's temporary directory, named from the working directory and
     * `base`, the dashboard's base path).
     *
     * @return array{string, bool}
     */
    public static function obtain(?string $file, string $base): array
    {
        $token = rtrim(strtr(base64_encode(random_bytes(32)), '+/', '-_'), '=');
        // phpcs:disable WordPress.WP.AlternativeFunctions -- a development server's token file in the system's temporary directory, outside WordPress.
        if ($file === null) {
            // A directory of this user's own, 0700, so another user of a shared
            // temporary directory can neither plant a token nor read this one.
            $dir = self::privateDirectory(sys_get_temp_dir() . '/cronwatch-' . self::userId());
            if ($dir === null) {
                // Not safe to keep one: a token for this request alone, so nothing is served.
                return [$token, true];
            }
            $file = $dir . '/dev-token-' . substr(hash('sha256', (getcwd() ?: '') . '|' . $base), 0, 16);
        }
        $existing = self::readToken($file);
        if ($existing !== null) {
            return [$existing, false];
        }
        // Written whole to a file of its own, then linked into place, which
        // fails when the name exists: a request that comes in between never
        // finds the file empty and makes a token of its own.
        $mask = umask(0077);
        try {
            $draft = is_link($file) ? false : @tempnam(dirname($file), 'dev-token-draft-');
        } finally {
            umask($mask);
        }
        $made = false;
        if ($draft !== false) {
            @chmod($draft, 0600);
            $made = @file_put_contents($draft, $token) === strlen($token) && @link($draft, $file);
            @unlink($draft);
        }
        // phpcs:enable WordPress.WP.AlternativeFunctions
        if (!$made) {
            // Another request made it first, or the directory is not writable: use its token, or this one.
            return ($existing = self::readToken($file)) !== null ? [$existing, false] : [$token, true];
        }
        return [$token, true];
    }

    /** A token file's token, when the file is a plain file of this user's, readable by no one else, holding one. */
    private static function readToken(string $file): ?string
    {
        if (!is_file($file) || is_link($file) || !self::private($file)) {
            return null;
        }
        $text = trim((string) @file_get_contents($file)); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_get_contents_file_get_contents -- a development server's token file, outside WordPress.
        return preg_match('/^[A-Za-z0-9_-]{43}$/D', $text) === 1 ? $text : null;
    }

    /** `dir`, made 0700 when missing; null when it is a link, not this user's, or open to others. */
    private static function privateDirectory(string $dir): ?string
    {
        if (!is_dir($dir) && !is_link($dir)) {
            $mask = umask(0077);
            try {
                // phpcs:disable WordPress.WP.AlternativeFunctions -- a development server's token directory, outside WordPress.
                @mkdir($dir, 0700);
                // phpcs:enable WordPress.WP.AlternativeFunctions
            } finally {
                umask($mask);
            }
        }
        clearstatcache(true, $dir);
        return is_dir($dir) && !is_link($dir) && self::private($dir) ? $dir : null;
    }

    /** Whether a path is this user's and nobody else may read or write it. */
    private static function private(string $path): bool
    {
        $perms = @fileperms($path);
        $owner = @fileowner($path);
        return $perms !== false && ($perms & 0077) === 0 && $owner !== false && $owner === self::userId();
    }

    private static function userId(): int
    {
        return function_exists('posix_geteuid') ? posix_geteuid() : (int) getmyuid();
    }
}
