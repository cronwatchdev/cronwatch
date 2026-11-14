<?php

declare(strict_types=1);

/*
 * What WpdbStore asks of WordPress, for WpdbStoreTest: a $wpdb whose answers
 * a test scripts (the real one is driven by WordPressTest), and the three
 * option functions over an array. Loaded only where WordPress is not.
 */

if (!defined('ARRAY_A')) {
    define('ARRAY_A', 'ARRAY_A');
}

if (!class_exists('wpdb', false)) {
    // phpcs:ignore -- WordPress's own class name.
    class wpdb
    {
        public string $prefix = 'wp_';
        public string $last_error = '';
        /** The connection, as WordPress holds it: an object with the server's last error number. */
        public object $dbh;
        /** @var list<string> every statement sent */
        public array $sent = [];
        /** @var \Closure(string, self): (array|int|array{error: string, errno: int}) what each statement answers */
        public \Closure $answer;
        public string $server = '8.4.3';

        public function __construct()
        {
            $this->dbh = new \stdClass();
            $this->dbh->errno = 0;
            $this->answer = fn (): int => 0;
        }

        /** A new connection in place of the old one, as WordPress makes when the server had gone away. */
        public function reconnect(): void
        {
            $this->dbh = new \stdClass();
            $this->dbh->errno = 0;
        }

        public function suppress_errors(bool $suppress = true): bool
        {
            return false;
        }

        public function db_server_info(): string
        {
            return $this->server;
        }

        public function prepare(string $query, mixed ...$args): string
        {
            $i = 0;
            return (string) preg_replace_callback('/%[sdi]/', function (array $m) use (&$i, $args): string {
                $value = $args[$i++] ?? '';
                return match ($m[0]) {
                    '%d' => (string) (int) $value,
                    '%i' => '`' . $value . '`',
                    default => "'" . addslashes((string) $value) . "'",
                };
            }, $query);
        }

        public function query(string $query): int|false
        {
            $answer = $this->run($query);
            return is_int($answer) ? $answer : false;
        }

        public function get_results(string $query, string $output = 'OBJECT'): ?array
        {
            $answer = $this->run($query);
            return is_array($answer) ? $answer : null;
        }

        private function run(string $query): array|int|null
        {
            $this->sent[] = $query;
            $this->last_error = '';
            $this->dbh->errno = 0;
            $answer = ($this->answer)($query, $this);
            if (is_array($answer) && isset($answer['error'])) {
                $this->last_error = (string) $answer['error'];
                $this->dbh->errno = (int) ($answer['errno'] ?? 0);
                return null;
            }
            return $answer;
        }
    }
}

if (!function_exists('get_option')) {
    function get_option(string $name, mixed $default = false): mixed
    {
        return $GLOBALS['cronwatch_test_options'][$name] ?? $default;
    }

    function update_option(string $name, mixed $value, mixed $autoload = null): bool
    {
        $GLOBALS['cronwatch_test_options'][$name] = $value;
        return true;
    }

    function delete_option(string $name): bool
    {
        unset($GLOBALS['cronwatch_test_options'][$name]);
        return true;
    }
}
