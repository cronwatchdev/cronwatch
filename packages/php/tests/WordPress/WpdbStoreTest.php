<?php

declare(strict_types=1);

namespace Cronwatch\Tests\WordPress;

use Cronwatch\JobState;
use Cronwatch\Store\Sql;
use Cronwatch\Tests\StoreConformanceTest;
use Cronwatch\WordPress\WpdbStore;
use PHPUnit\Framework\TestCase;

/**
 * WpdbStore's answers to what $wpdb tells it, with a $wpdb a test scripts
 * (fixtures/fake-wpdb.php): tables gone while the option says they were
 * made, a duplicate key told by its number, and a statement WordPress sent
 * twice. WordPressTest runs the store on a real WordPress and database.
 */
final class WpdbStoreTest extends TestCase
{
    private \wpdb $wpdb;

    public static function setUpBeforeClass(): void
    {
        if (!defined('ABSPATH')) {
            define('ABSPATH', sys_get_temp_dir() . '/');
        }
        require_once __DIR__ . '/fixtures/fake-wpdb.php';
        require_once dirname(__DIR__, 2) . '/wordpress/includes/WpdbStore.php';
    }

    protected function setUp(): void
    {
        $GLOBALS['cronwatch_test_options'] = [WpdbStore::OPTION => WpdbStore::DB_VERSION];
        $this->wpdb = new \wpdb();
    }

    protected function tearDown(): void
    {
        unset($GLOBALS['cronwatch_test_options']);
    }

    /** @return list<string> the statements sent that start with `verb` */
    private function sent(string $verb): array
    {
        return array_values(array_filter($this->wpdb->sent, fn (string $q) => str_starts_with(ltrim($q), $verb)));
    }

    /**
     * A site copied without the plugin's tables still has the option that
     * says they were made: the first statement that finds one missing makes
     * them again and is sent again, so the plugin works on from there.
     */
    public function testTablesGoneWhileTheOptionSaysTheyWereMadeAreMadeAgain(): void
    {
        $made = false;
        $this->wpdb->answer = function (string $query) use (&$made): array|int {
            if (str_starts_with(ltrim($query), 'CREATE TABLE')) {
                $made = true;
                return 0;
            }
            if (!$made) {
                return ['error' => "Table 'wp.wp_cronwatch_jobs' doesn't exist", 'errno' => 1146];
            }
            return str_starts_with($query, 'SELECT') ? [] : 1;
        };
        $store = new WpdbStore($this->wpdb);
        $store->init();
        $this->assertSame([], $this->wpdb->sent, 'the option says the tables are there, so init() asks nothing');
        $this->assertSame([], $store->listJobs());
        $this->assertCount(3, $this->sent('CREATE TABLE'), 'the three tables, made again');
        $this->assertCount(2, $this->sent('SELECT'), 'the statement, sent again once they are');
        $this->assertSame(WpdbStore::DB_VERSION, get_option(WpdbStore::OPTION));
        // And a write, the same way, on a store of its own (another request).
        $made = false;
        $this->wpdb->sent = [];
        $again = new WpdbStore($this->wpdb);
        $again->insertRun(StoreConformanceTest::makeRun('r1', 'a', 'running', 1));
        $this->assertCount(3, $this->sent('CREATE TABLE'));
        $this->assertCount(2, $this->sent('INSERT'));
    }

    /** The server writes its messages in its own language: the number says a table is missing, whatever the text. */
    public function testAMissingTableIsToldByItsNumber(): void
    {
        $made = false;
        $this->wpdb->answer = function (string $query) use (&$made): array|int {
            if (str_starts_with(ltrim($query), 'CREATE TABLE')) {
                $made = true;
                return 0;
            }
            return $made ? [] : ['error' => "Tabelle 'wp.wp_cronwatch_jobs' existiert nicht", 'errno' => 1146];
        };
        $this->assertSame([], (new WpdbStore($this->wpdb))->listJobs());
    }

    /** Made again once a request: a server where that does not help is not asked at every statement. */
    public function testTheTablesAreMadeAgainOnlyOnceAndTheErrorThenStands(): void
    {
        $this->wpdb->answer = fn (string $query): array|int => str_starts_with(ltrim($query), 'CREATE TABLE')
            ? 0
            : ['error' => "Table 'wp.wp_cronwatch_jobs' doesn't exist", 'errno' => 1146];
        $store = new WpdbStore($this->wpdb);
        foreach ([1, 2] as $attempt) {
            try {
                $store->listJobs();
                $this->fail('the statement still fails');
            } catch (\RuntimeException $error) {
                $this->assertStringContainsString("doesn't exist", $error->getMessage());
            }
        }
        $this->assertCount(3, $this->sent('CREATE TABLE'), 'once, not once per statement');
    }

    /** A server that cannot make the tables (too old, no CREATE privilege) leaves the statement's own error. */
    public function testWhenTheTablesCannotBeMadeTheStatementsOwnErrorIsThrown(): void
    {
        $this->wpdb->answer = fn (string $query): array => str_starts_with(ltrim($query), 'CREATE TABLE')
            ? ['error' => "CREATE command denied to user 'wp'@'localhost'", 'errno' => 1142]
            : ['error' => "Table 'wp.wp_cronwatch_runs' doesn't exist", 'errno' => 1146];
        try {
            (new WpdbStore($this->wpdb))->getRun('r1');
            $this->fail('the statement fails');
        } catch (\RuntimeException $error) {
            $this->assertStringContainsString("doesn't exist", $error->getMessage());
            $this->assertSame(1146, $error->getCode());
        }
        $this->assertCount(1, $this->sent('CREATE TABLE'));
    }

    /** Any other error makes no tables, and neither does the plugin's own DROP. */
    public function testOtherErrorsMakeNoTables(): void
    {
        $this->wpdb->answer = fn (): array => ['error' => 'You have an error in your SQL syntax', 'errno' => 1064];
        $store = new WpdbStore($this->wpdb);
        try {
            $store->listJobs();
            $this->fail('the statement fails');
        } catch (\RuntimeException $error) {
            $this->assertSame(1064, $error->getCode());
        }
        $this->wpdb->answer = fn (): array => ['error' => "Table 'wp.wp_cronwatch_jobs' doesn't exist", 'errno' => 1146];
        try {
            $store->drop();
            $this->fail('the statement fails');
        } catch (\RuntimeException) {
        }
        $this->assertSame([], $this->sent('CREATE TABLE'));
    }

    /**
     * A first state two processes write at once: the second insert meets a
     * duplicate key, which is "another process wrote first" (false, and the
     * caller reads again), told by MySQL's number, 1062, so a server that
     * writes its messages in another language (lc_messages) is read the same.
     */
    public function testADuplicateKeyIsToldByItsNumberWhateverTheServersLanguage(): void
    {
        $state = new JobState('wp:a', version: 1);
        foreach ([["Doppelter Eintrag 'wp:a' für Schlüssel 'PRIMARY'", 1062], ["Duplicate entry 'wp:a' for key 'PRIMARY'", 1062], ["Duplicate entry 'wp:a' for key 'PRIMARY'", 0]] as [$message, $errno]) {
            $this->wpdb->answer = fn (string $query): array|int => str_starts_with($query, 'INSERT') ? ['error' => $message, 'errno' => $errno] : 0;
            $this->assertFalse((new WpdbStore($this->wpdb))->compareAndSetState($state, 0), "{$message} ({$errno})");
        }
        // Another error is not a lost race: it is thrown.
        $this->wpdb->answer = fn (string $query): array|int => str_starts_with($query, 'INSERT') ? ['error' => 'Lock wait timeout exceeded', 'errno' => 1205] : 0;
        $this->expectException(\RuntimeException::class);
        (new WpdbStore($this->wpdb))->compareAndSetState($state, 0);
    }

    /**
     * $wpdb sends a statement again on a new connection when the server had
     * gone away. When the first send landed before the break, the second
     * finds its own write and answers "not written"; the store reads the
     * state back and, finding the one it wrote, says written, so the job's
     * update is not worked out and applied a second time.
     */
    public function testAConditionalWriteWordPressSentTwiceIsReadAsWritten(): void
    {
        $state = new JobState('wp:a', version: 3);
        $json = Sql::stateParams($state)[1];
        $resend = function (string $query, \wpdb $wpdb) use ($json): array|int {
            if (str_starts_with($query, 'UPDATE')) {
                // The first send landed, the connection broke, and the second send matched nothing.
                $wpdb->reconnect();
                return 0;
            }
            if (str_starts_with($query, 'INSERT')) {
                $wpdb->reconnect();
                return ['error' => "Duplicate entry 'wp:a' for key 'PRIMARY'", 'errno' => 1062];
            }
            return [['state' => $json]];
        };
        $this->wpdb->answer = $resend;
        $store = new WpdbStore($this->wpdb);
        $this->assertTrue($store->compareAndSetState($state, 2), 'an update sent twice');
        $first = new JobState('wp:a', version: 1);
        $firstJson = Sql::stateParams($first)[1];
        $this->wpdb->answer = fn (string $query, \wpdb $wpdb): array|int => str_starts_with($query, 'SELECT') ? [['state' => $firstJson]] : $resend($query, $wpdb);
        $this->assertTrue($store->compareAndSetState($first, 0), 'a first insert sent twice');

        // On one connection throughout, "not written" is another process's write, whatever the state now reads.
        $this->wpdb->answer = fn (string $query): array|int => str_starts_with($query, 'UPDATE') ? 0 : [['state' => $json]];
        $this->assertFalse($store->compareAndSetState($state, 2));
        // Sent twice, but the stored state is another's: not written.
        $other = Sql::stateParams(new JobState('wp:a', version: 3, consecutiveFailures: 2))[1];
        $this->wpdb->answer = function (string $query, \wpdb $wpdb) use ($other): array|int {
            if (str_starts_with($query, 'UPDATE')) {
                $wpdb->reconnect();
                return 0;
            }
            return [['state' => $other]];
        };
        $this->assertFalse($store->compareAndSetState($state, 2));
    }
}
