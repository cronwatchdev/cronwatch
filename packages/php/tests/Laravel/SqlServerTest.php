<?php

declare(strict_types=1);

namespace Cronwatch\Tests\Laravel;

/**
 * An app whose default connection is one CronWatch has no store for (SQL
 * Server here): installing the package must not stop its migrations.
 */
final class SqlServerTest extends TestCase
{
    protected function defineEnvironment($app): void
    {
        parent::defineEnvironment($app);
        $app['config']->set('cronwatch.store.driver', 'database');
        $app['config']->set('database.connections.sqlsrv', ['driver' => 'sqlsrv', 'host' => 'localhost', 'database' => 'app']);
        $app['config']->set('database.default', 'sqlsrv');
    }

    public function testTheMigrationIsLeftOut(): void
    {
        $paths = $this->app->make('migrator')->paths();
        $this->assertSame([], array_values(array_filter($paths, fn (string $path) => str_contains($path, 'Laravel/migrations'))));
    }
}
