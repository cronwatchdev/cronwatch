# Changelog

Every notable change to CronWatch, newest first. All the packages, in every language, share one version, so each release is one section here, with a line per language where it matters. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and from 1.0 the versions follow [Semantic Versioning](https://semver.org) as the [Stability](https://cronwatch.dev/docs/stability/) page describes. The WordPress plugin, the Drupal module and the Craft CMS plugin keep their own changelogs too, for their stores.

## Unreleased

### Fixed

Every language:

- A run that started before a missed fire no longer closes missed when it finishes. A cron job whose 02:00 run was still going at the 03:00 fire sent missed, then "recovered" when the run ended, then missed again for the same fire a minute later. Missed now closes only for a run that catches the schedule up, so it opens once and recovers once.
- An interval job is no longer reported missed the moment a run that outlasted its period ends. When a run finishes later than its start plus the interval, the next run is due a period after the finish. The dashboard's timeline follows the same rule.
- Inside a spring-forward gap, the checks and the timeline agree on one rule: every slot the pattern names still fires, a slot that falls in the gap moves forward by the gap's length (thirty minutes on Lord Howe Island), and one run at the jump covers every slot moved out of it. Before, the checks skipped some of those slots and the timeline others. Fall-back nights are unchanged.
- The dashboard answers only at its base path and below it. A path such as `/cronwatch/../api/jobs` or `/cronwatchapi/jobs` was served as the dashboard's own API after its dot segments were resolved, which on a dashboard with no token of its own let a request past auth that matched on the path. Such paths are now answered 404 before any sign-in.
- A Slack alert's link has `<`, `>`, and `|` percent-encoded, so a job name cannot break the link or add mrkdwn.
- A user's webhook header replaces the default of the same name whatever its case, instead of being sent beside it, and can never replace the signature headers.
- The trigger and metrics in a Claude triage prompt are fenced as job data, as the run's output and error already were.
- Only the first 64 KiB of a refused alert's answer is read, and a body cut off by the deadline keeps what arrived. A successful answer is still read up to 1 MiB.

TypeScript:

- An `onError` that throws no longer stops a job from running, or loses its finish, when the store fails.

Go:

- Claude triage reads the job's earlier runs inside its own 25 second limit. A store that never answered that read held the shared check for good, so later checks, the dashboard's check, and the run that sent the alert all waited.

Python:

- A forked process (gunicorn with `preload_app`, Celery's prefork) resets every client's and store's locks and connections in the child before any thread runs, through `os.register_at_fork`. Before, a thread in the child could take a lock the parent held at the fork, or send queries down the parent's Postgres connection.
- A malformed `Content-Length` on the WSGI dashboard or handler is answered 400 instead of 500, and is not reported to `on_error`.

PHP:

- `PostgresStore` and `MysqlStore` look in the catalog before creating their tables, and run no DDL when everything exists. On Postgres the DDL took a table lock on every request; on MySQL and MariaDB `CREATE TABLE IF NOT EXISTS` committed the app's open transaction when the store shared its connection.
- Old runs are pruned once an hour across processes, as in the other languages, instead of on every check. The last prune time is kept in the state table under a reserved key.
- The library no longer uses `ext-ctype`, which it did not require.
- `SqliteStore` refuses a database file, or its directory, that the process cannot read and write, with a message that every process sharing it must run as the same user.

Rust:

- `start_checking` no longer keeps a client alive: dropping every handle stops the checks, as `stop()` does.

Elixir:

- Stopping the application waits at most 60 seconds for a check in flight, then stops it. A store call that never answered kept the release from shutting down.
- The HTTP client closes its socket when a request fails to send, resolves IPv4 and IPv6 addresses at once, and gives each address a share of the deadline, so one unreachable address no longer uses all of it.
- A custom channel's unexpected answer is described by its shape in the error report, never printed, so a secret it holds is not logged.
- A run timeout longer than about 49 days is cancelled properly when its run ends.

Java:

- The servlet filter and the WebFlux filter decide which requests are the dashboard's from the normalised path, as the routes do.
- The JobRunr and Quartz check jobs fail, so the scheduler shows it, on a node with no live CronWatch watch, instead of succeeding without checking.
- JobRunr runs whose end JobRunr never reports are closed and dropped rather than kept for good.
- More than one JobRunr watch in a JVM each get their checks; a second `watch` no longer replaces the first.

.NET:

- The Hangfire check job fails on a server that never called `UseCronwatch`, instead of succeeding without checking.
- Hangfire's filter waits at most five seconds on the store when a job starts or ends, so a store outage no longer holds each worker for the store's whole timeout.
- `MapCronwatch` keeps a `Cronwatch:Token` from configuration when the app passes its own `RoutesOptions` without a token.

### Changed

Every language:

- A webhook with a secret also sends `X-CronWatch-Timestamp` (Unix seconds) and `X-CronWatch-Timestamp-Signature: sha256=<hex>`, the HMAC-SHA256 of the timestamp, a full stop, and the body, so a receiver can refuse a replayed request. `X-CronWatch-Signature` is unchanged. Each webhook channel takes a clock option for tests.
- A store without the conditional writes (`compareAndSetState` or `updateRunIf`, by each language's names) is reported once to `onError`, since processes sharing it can then judge a run twice or lose each other's updates.
- `lastRun` is optional for a store of your own, since the client never calls it: TypeScript, Python, PHP, and Elixir make it optional, and Rust and .NET give it a default. Go keeps it in `Store` until a major release, and the built-in stores keep it everywhere.

Python:

- Django's `BASE_PATH` must be the prefix the dashboard's URLs are included under; a request outside it is answered 404.

PHP:

- A dashboard token shorter than 24 characters is reported once through `onError`, and still used.
- The WordPress plugin needs WordPress 6.5 or newer, the Drupal and Craft API tokens need at least 24 characters, and the Craft dashboard answers only in the control panel. Each has its own changelog.

.NET:

- `MapCronwatch` requires authorization when the dashboard has no token of its own (`DashboardToken.None`); chain `AllowAnonymous()` to keep it open. `UseCronwatch` with no token logs a warning, once, that it relies on the app's authentication running first. New: `RoutesOptions.WithDefaultToken`, `WebhookOptions.Clock`.

Java:

- New: `Adapters.under(rawPath, base)`, the base path test the servlet and WebFlux filters use, and `WebhookOptions.now(LongSupplier)`.

Rust:

- `Cargo.lock` is committed, so CI and `cargo publish` build the same dependency versions from a commit.

MCP server:

- It refuses to start with a plain `http` URL to a host other than this machine; `--allow-http` or `CRONWATCH_ALLOW_HTTP=1` starts it with a warning.
- Answers are kept to a size an agent can read (500 characters a line, 40,000 an answer, with a note of what was left out), job names are checked before any request, the dashboard's answers are read to at most 5 MB and checked for the fields each tool needs, and anything the dashboard says in an error is fenced as data.
- `run_check` is marked open-world, since it sends alerts.
- `@modelcontextprotocol/sdk` 1.31 or newer (GHSA-6qxp-vccf-f47h, in the SDK's OAuth client, which this server does not use).

## 0.13.1 - 2026-10-11

### Fixed

Java:

- The published POMs' project URL is `https://cronwatch.dev`, as every other package's is. Maven appended each module's name to the parent's, so Maven Central linked to `https://cronwatch.dev/cronwatch`, which does not exist. The SCM clone addresses had the same suffix and are fixed too.

## 0.13.0 - 2026-10-10

### Changed

Every language:

- A recovery for an alert that already went out is sent even while the job is silenced. Silence still holds back new alerts.
- `test` and `testing` no longer count as development, so a test run or CI without `CRON_SECRET` gets a handler that refuses (503) and a dashboard that needs a token, as in production. `development`, `dev`, and `local` still count, as do a framework's own names for them (Laravel's `local`, Symfony's `dev`, `MIX_ENV=dev`, a Spring `dev` profile). Set `CRON_SECRET` or a token in tests, or `CRONWATCH_ENV=development` where you relied on the old reading.
- `CRONWATCH_TOKEN`, `CRON_SECRET`, and tokens and secrets given in code are trimmed (with JavaScript's whitespace set), so a value read from a file with a trailing newline matches.
- A job's state gains `pendingRuns`, the runs started and not yet judged (see Fixed).

Go and Rust:

- A job's HTTP handler runs the job to its end when the caller hangs up (a curl with `--max-time`, a scheduler's attempt deadline), as the SDK's handler does. Before, the job was cancelled and recorded as failed. Rust's `HandlerOptions::cancel_with_request()` keeps the old behaviour.

Ruby:

- A run of a Sidekiq job, or an ActiveJob job on Sidekiq, stopped by `Sidekiq::Shutdown` during a deploy is recorded as failed but no longer judged: it opens no failed alert, does not count toward `failures_before_alert`, and sends no recovery. Sidekiq runs the job again after the restart.
- The dashboard reads the request's origin from its own scheme and `Host` header. The new `trust_proxy:` option reads the first `X-Forwarded-Proto` and `X-Forwarded-Host`, as the SDK's `trustProxy` does. It defaults to true under Rails, which trusts those headers itself, and to false elsewhere, so a plain Rack app behind a proxy now needs `trust_proxy: true` or `origin:`.

Python:

- Alert channels' requests no longer go through `HTTP_PROXY`, `HTTPS_PROXY`, or the system's proxy settings, as the TypeScript SDK's do not.

### Added

- Rust: `cronwatch::web::Tls` (the `tower` feature), a request extension that marks a request as having come over TLS. An app that terminates TLS itself adds it (`.layer(axum::Extension(cronwatch::web::Tls))`), so the dashboard reads its origin as `https`.
- .NET: `ObservedRun.TakeBackAsync(Exception?)`. Java: `ObservedRun.takeBack(Throwable)`.
- MCP: requests time out (30 seconds, 120 for a check), with a `timeoutMs` option.
- Ruby: making the dashboard routes warns when the token is under 32 characters.

### Fixed

Every language:

- When several processes check one store, a queued alert is retried by one of them, not all: a check claims the alerts it retries in the job's state before sending them.
- A finished run whose evaluation was never written (the process stopped between the two writes, or the state write failed) is evaluated by a later check, once its finish is five minutes old, so its alert is no longer lost.
- Dashboard request bodies are capped at 1 MiB in every port (TypeScript and Ruby had no cap): a silence answers 413, and a sign-in body over it reads as no token.
- The `?token=` sign-in can no longer redirect off the site, however many slashes or backslashes lead the path.
- Secret compares take time that depends only on what was sent, so the secret's length cannot be timed.
- Slack and Discord errors cut the webhook URL's token from an answer that echoes it.
- A webhook given an empty-string secret reports on each send that it goes out unsigned (not in Go and Rust, where an empty secret and none are the same value).
- After a dashboard Delete, running or resuming a job by name declares it again as it was written, keeping its schedule and options.

TypeScript:

- The Postgres store destroys a connection whose rollback fails, and throws the work's own error.
- Channels release successful answers.

PHP:

- Laravel: CronWatch failing to build its client (a store driver it has no store for, a setting it cannot read) no longer stops `schedule:run`, queued jobs, or `migrate`. The task or job runs unrecorded and the failure goes to the app's log. The migration is skipped for a connection CronWatch cannot store in. `cronwatch:check` is scheduled without overlapping, and on one server when the cache can lock.
- Symfony: the same failure no longer stops `messenger:consume`; it goes to the logger. A `DATABASE_URL` holding container parameters (Symfony's default `sqlite:///%kernel.project_dir%/var/data.db`) is read with them resolved.
- Drupal: keys, tokens, and the webhook secret are password fields, never shown again; left blank they keep the saved value, and "Remove it" clears it.
- Craft CMS: secrets typed in plain are never shown again, settings `config/cronwatch.php` sets are shown empty and never copied into project config on save, and the webhook secret is kept exactly as typed.
- WordPress: deleting a site from a network drops its CronWatch tables, and if another plugin loads a different version of the library, the plugin stays off with a notice instead of failing.
- A value logged with `$job->log()` that holds itself is written as `String()` writes it, instead of exhausting PHP's memory and ending the job.
- If PCRE gives up on the default redaction (low `pcre.backtrack_limit` or JIT limits), the text is stored as `[redacted]` and the run is still recorded.
- The development token file is written whole before it appears, so two first requests at once no longer announce a sign-in link that never works.
- An alert URL with a backslash before its path is refused, since Node and curl read it as different hosts.
- `Handler` given `$_SERVER` finds the bearer Apache keeps out of it (no `CGIPassAuth`) through `getallheaders()`.
- A web request that sends alerts for many jobs moves its time limit on once, so `max_execution_time` still bounds it.

Python:

- `cronwatch.apscheduler`: an error reported while the client cannot be made is logged, and the listener's thread keeps recording; before, it ended, and `flush()` could wait forever.
- Metrics read from a stored row are only its finite numbers, so finishing a run another writer left with a null or text metric no longer fails its evaluation.
- `cronwatch.celery`: a task id over 180 characters, holding a NUL, or starting with `pgcron:` gives its runs a hashed prefix, so a run whose worker is lost is failed at once instead of reported stuck later.

Go:

- A store call that never answers no longer holds checking for good: the shared check has a 10-minute deadline, each step of recording a run 2 minutes, and `Close` waits at most the check's deadline plus 30 seconds.
- Callers sharing one `Check` each get their own copy of the result.
- A `failuresBeforeAlert` another port stored as a fraction is kept in an alert's `threshold`, and the bridge declares it again as the whole number that alerts on the same failure.
- A long-running process no longer keeps a lock and a sync turn for every job name it has ever seen.

Rust:

- A long-lived client lets go of a job's locks once the job is idle.

Elixir:

- A crash of the memory store's process or of `Cronwatch.Runs` restarts the processes after it with the instance's config in place. Before, the config was erased, the restart could not succeed, and the whole instance went down.
- `Cronwatch.run/3` runs the function unrecorded, reported once through Logger, when the instance is not running (not started, stopping, or restarting). Before, it raised or exited without running the job.
- An isolated run whose task went down with a restart of `Cronwatch.Runs` is recorded as failed instead of being left running.
- An alert's HTTP request is killed when its caller is, so a triage cut short no longer leaves its request and API key alive.
- Documented: a store of your own must bound its own calls, and on SQLite a run started inside a transaction that has already written waits for it; start runs outside the transaction there.

Java:

- With `cronwatch.enabled=false`, the Spring Boot starter turns every part off (the check, `@Scheduled`, Quartz, and the dashboard), even when the app has its own `Cronwatch` bean. Before, such an app failed to start.
- On WebFlux behind a TLS-terminating proxy, with Spring's forwarded-header handling on, the dashboard's forms are no longer refused as cross-site, and the sign-in cookie is `Secure`.
- `close()` stops waiting for a check that cannot end and reports it. On Postgres, MySQL, and MariaDB, `SqlStore`'s statements and the pg_cron source's reads have a 60-second query timeout.
- Under an app name many apps get alike (`app`, `java`, Spring Boot's launcher), with no `CRONWATCH_APP_ID`, a scheduler integration takes the schedule only from jobs it declared itself, and reports the rest once.
- A scheduler integration's sync that timed out is interrupted, and no new one starts while one is still running.
- The starter puts Quartz's check job on one scheduler only, a clustered one first; when no scheduler takes it, the check runs in each instance instead of nowhere.
- JobRunr: an attempt deleted from JobRunr's dashboard while it runs is given back instead of recorded as failed, and an attempt that ends in an `Error` is closed as failed when the job's next attempt opens.
- A run that cannot be given back is recorded as failed, never as a success.

.NET:

- When a Hangfire or Quartz.NET run cannot be given back (the store has no `IDeleteRunIfStore`, or its delete failed), it is recorded as failed with what ended it, not as a success, so a scheduler's shutdown cannot send a false recovery.
- A channel or error handler that disposes the client during a check no longer waits for the check it is part of.
- A channel that throws its own `TimeoutException` is reported as that channel's failure, not as the client's deadline, and no longer stops the alert's delivery from being recorded.
- On MySQL and MariaDB, two processes writing the same state from version 0 at the same moment can no longer both count the write as theirs.

Ruby:

- A triage cut short by the retry budget no longer stops later alerts from being triaged.
- Slack's and Discord's error messages decode the answer as UTF-8, so an error reporter that encodes them as JSON no longer fails.
- A `failure:` callable that raises fails the run instead of leaving it running.

## 0.12.5 - 2026-10-10

### Changed

Every language:

- Alert text, the dashboard, and error and log messages use the serial (Oxford) comma in lists of three or more, and a recovery names what closed as a list ("succeeded after: failed and slow", "after: missed, failed, and over budget"), so a filter that matches alert text word for word may need updating.

.NET:

- `StoreReplay` and `FinishOnce` in `Cronwatch.StoreTesting` are marked `[Obsolete]`: the store kit promises only `StoreContract.RunAsync`, as every other language's does. Both still work, and go in 1.0.

Go:

- `triage.System`, the exported Claude triage prompt, carries the same wording change; the constant's value changed, which Go's API check reports as incompatible, accepted here as the packages are below 1.0.

## 0.12.4 - 2026-10-10

### Changed

Every language:

- The source moved to the CronWatch organisation on GitHub, [github.com/cronwatchdev/cronwatch](https://github.com/cronwatchdev/cronwatch). Every package's repository, issue tracker and changelog links point there, as does the dashboard footer's link to the changelog. The old addresses redirect. The Go import path, `cronwatch.dev/go`, is unchanged.

## 0.12.3 - 2026-10-07

### Added

Every language:

- The dashboard's footer names the library and version serving it, such as `@cronwatch/sdk 0.12.3`, linked to the changelog. Nothing is fetched to check for a newer release.

## 0.12.2 - 2026-10-06

### Changed

Every language:

- On a job's page in the dashboard, Silence opens the choice of how long (an hour, four hours, a day, or a week), and one click silences the job; before, a menu of lengths sat beside the button and choosing one did nothing until Silence was pressed. Forget is now Delete history, in a Delete section at the foot of the page that says what it removes and that it cannot be undone, with Delete and Cancel. The buttons share one height. The routes and the JSON API are unchanged.

## 0.12.1 - 2026-10-03

### Added

PHP:

- The WordPress plugin, the Drupal module and the Craft CMS plugin offer every alert channel in their settings, not only email, Slack and the webhook: Discord, email through Resend, Postmark, SendGrid, Mailgun or Amazon SES, text messages through Twilio, and Sentry, Honeybadger, Datadog, Rollbar, Bugsnag and New Relic. Each sends once its required fields are set, and a channel only partly filled in is named on save. The WordPress plugin's zip now carries those channels. See [WordPress](https://cronwatch.dev/docs/wordpress/#settings), [Drupal](https://cronwatch.dev/docs/drupal/#settings) and [Craft CMS](https://cronwatch.dev/docs/craft/#settings).
- The Craft CMS plugin's settings gain "Send a test alert", as WordPress's and Drupal's have.

## 0.12.0 - 2026-10-01

### Added

Every language:

- A new condition, `under_floor`, for the job that exits cleanly and did nothing. A job's `floor` option sets a minimum per metric (`floor: { rows: 1 }`); a metric without one is under its floor when it reports 0 or less and the five to twenty successful runs before it all reported more than 0. It alerts once as a warning and stays open while the metric stays down, however long that is (the metrics under their floor are kept in the job's state, as `underFloor`), and closes with a recovery. `floor: { name: 0 }` lets a metric reach 0. A floor above the same metric's `budget` is refused. The webhook's alert types gain `under_floor`, with `details` shaped as `over_budget`'s. See [under_floor](https://cronwatch.dev/docs/conditions/#under_floor).

Java:

- `AlertDetails` gains `UnderFloor`, so an exhaustive `switch` over the sealed interface needs a case for it.

### Fixed

Go:

- The pg_cron source keeps a job's `budget` when the job leaves pg_cron's schedule. It read the option in a form it never matched, so the ceilings were dropped and the job fell back to the three-times-the-median baseline.

## 0.11.1 - 2026-10-01

### Added

Every language:

- The dashboard switches between light and dark on Cmd+Shift+D (Ctrl+Shift+D on Windows and Linux) and keeps the choice in the browser's `localStorage`, under `cronwatch-theme`; until then it follows the system's setting. The dashboard shown inside WordPress, Drupal or Craft CMS does not load the dashboard's script, so it keeps following the system there.

### Fixed

.NET:

- The Hangfire and Quartz.NET integrations no longer walk a cron's fire times again when two reads of the scheduler overlap (the read every minute and the check job, say): the read that ended first dropped what the other had just checked. `FireTimeChecks.BeginRead` starts a read, and what is dropped waits until the last read ends.

## 0.11.0 - 2026-10-01

The preparation for 1.0: names settled across the languages, internals marked as internal, and the stored data, the JSON API and the webhook made ready to grow without breaking. Names that changed keep working under their old spelling, deprecated (see [Deprecations](https://cronwatch.dev/docs/deprecations/)); the breaking changes for 0.x users are listed first.

### Breaking changes for 0.x users

Every language:

- Processes sharing a store should all move to this release together: 1.x releases keep each other's stored data, 0.x releases do not.
- The dashboard API's silence and unsilence answer the job's summary, `{ ok: true, job }`, where they answered its stored state, `{ ok: true, state }`. `@cronwatch/mcp` reads either.
- `recordRun` refuses a run id longer than 200 characters, as the normal path already did.
- A blank `CRONWATCH_ENV` or `APP_ENV` (only spaces) counts as unset, and every language reads the environment the same way: `CRONWATCH_ENV`, then `APP_ENV`, then its own variable, trimmed and lowercased, with `prod` as production and `dev`, `local`, `test` and `testing` as development. This decides whether the dashboard makes a token of its own and whether a handler with no secret runs. See [Environment variables](https://cronwatch.dev/docs/environment/).
- Integrations record runs with their names spelled the one way: `active-job` (was `active_job`), `laravel-scheduler` and `laravel-queue` (were `schedule` and `queue`), `symfony-scheduler` and `symfony-messenger` (were `scheduler` and `messenger`), `drupal-cron` and `drupal-queue` (were `cron` and `queue`), `craft-queue` and `craft-command` (were `queue` and `command`), `spring-scheduled` (was `scheduled`) and .NET's `hosting` (was `schedule`). Runs recorded before keep their trigger; a filter on it should look for both spellings until they age out.
- A one-time date in place of a cron expression is refused when the job is declared, with the same message in every language, and a date no month has (`0 0 30 2 *`) is accepted and never fires.
- A dashboard token or cron secret of only whitespace, in `CRONWATCH_TOKEN`, `CRON_SECRET` or given in code, counts as unset, as the Environment variables page says: outside development the dashboard stays locked (503) and a handler answers 503 and reports why, where the blank value was taken as the token. A token or secret given in code that is neither a string nor null (`false`, a number) is refused with an error instead of becoming a password; PHP's deprecated `false` for "off" still works.
- The dashboard's sign-in form posts the token to `<base>/signin` in the request body, so it no longer lands in access logs as `?token=`. Opening a page with `?token=` still signs in, for the development sign-in link.

TypeScript:

- The environment is read from `CRONWATCH_ENV` and `APP_ENV` before `NODE_ENV`, which was the only one read. An app with `APP_ENV=development` set for another reason now counts as development.
- A one-time date, which croner accepted, is refused; a date no month has, which reported its job as unevaluable, never fires.

Ruby:

- The environment is read from `CRONWATCH_ENV` and `APP_ENV` before `Rails.env`, `RAILS_ENV` and `RACK_ENV`.
- The constants the docs do not name are private (`private_constant`), so referring to one raises `NameError`.
- A one-time date is refused; a date no month has, which Fugit refused, never fires.
- `token:` on `routes` and `cron_secret:` on the client take a String or `nil` only, and raise `TypeError` for anything else: `false`, a number or a Symbol was turned into a password (`token: false` signed in with `Bearer false`).

Python:

- Every module declares `__all__`, so `from cronwatch.x import *` brings only the public names. The modules that only implement the client moved to underscored names (`cronwatch._evaluate` and the rest); the old names still work, with a `DeprecationWarning`.
- `local` and `testing` count as development.

PHP:

- `null` turns the cron secret, the dashboard's token and a handler's secret off, as in every other language. In 0.x `null` read `CRON_SECRET` or `CRONWATCH_TOKEN`, and `false` turned them off. Leaving the argument out still reads the environment, as does the new default, `Cronwatch\FromEnv::Read`; `false` still works, deprecated. Code that passed `null` on purpose to mean "read the environment" now turns the secret off. Laravel's and Symfony's settings are unchanged: an unset token or cron secret there is still read from the environment.
- `CRON_SECRET` or `CRONWATCH_TOKEN` holding one of the words Laravel's `env()` reads as null, true, false or empty (`null`, `(null)`, `true`, `(true)`, `false`, `(false)`, `empty` or `(empty)`, in any case) counts as unset. With `CRON_SECRET=null` in a Laravel `.env` file, `Authorization: Bearer null` ran every handler and the dashboard's check, since Laravel handed the config null and the variable kept the word.
- `cronSecret:`, `routes(token:)` and a handler's `secret:` are declared `mixed` and check the value themselves, so a caller without `strict_types` can no longer pass `5` and get the password `"5"`: anything but a string, `null`, `false` or `FromEnv::Read` throws a `TypeError`.

Rust:

- The data types (`Run`, `StoredJob`, `Alert`, `JobState`, `JobSummary` and the rest), `AlertDetails`'s variants and every options struct are `#[non_exhaustive]`, so a later release can add a field without a major. Build them with their constructors (`Run::new`, `StoredJob::new`, `Alert::new`, `AlertDetails::failure` and the others) and options with `new()` and a builder method per field (`SlackOptions::new().webhook_url(url)`), not struct literals.
- `js::parse` returns a `JsonError`; `ParseError` is a deprecated alias of it.

Java:

- These were public without being meant for apps, and are internal: `Json.quote`, `Json.kind`, `Json.copy` and `Json.MAX_DEPTH`; `PgCron.schedule`, `PgCron.jobName`, `PgCron.run`, `PgCron.HOLD_MS` and `PgCronRow`; `Twilio.MAX_SEGMENTS`; and in `dev.cronwatch.storetest` everything but `StoreContract.run`.
- A record that may grow (`Run`, `StoredJob`, `JobState`, `Alert`, `JobSummary`, `CheckResult`) is built with its static `of`; its canonical constructor is no longer promised.

.NET:

- `ICronwatchJob` moved from the `Cronwatch` namespace to `Cronwatch.Hosting`, with no alias: add `using Cronwatch.Hosting;` to a job class.
- The extension methods moved to Microsoft's namespaces: `AddCronwatch` and `AddCronwatchJob` to `Microsoft.Extensions.DependencyInjection`, `RunCronwatchCommandAsync` to `Microsoft.Extensions.Hosting`, and `MapCronwatch`, `UseCronwatch` and `MapCronwatchHandler` to `Microsoft.AspNetCore.Builder`. `services.AddCronwatch(...)` and `app.MapCronwatch(...)` compile as before, and the former classes keep the methods as plain static methods, deprecated. The new class for `AddCronwatch` is `CronwatchHostingServiceCollectionExtensions`, so the former class's name is not ambiguous where both namespaces are imported.
- A `Cronwatch:Token` of only whitespace counts as unset, as `CRONWATCH_TOKEN` does.
- `TwilioOptions.Segments` is an `int?`, not a `double?`.
- A handler function whose lambda names its request's type must say `CronwatchRequest`.

Go and Elixir have no breaking changes beyond those every language shares.

### Added

- `GET /api` on every dashboard says what is answering: `{ ok: true, library, language, version, api: 1 }`. The API only grows within `api: 1`.
- The webhook's body starts with `"schema": 1`, and its JSON Schema is published at [cronwatch.dev/schemas/webhook/1.json](https://cronwatch.dev/schemas/webhook/1.json). A receiver can check the `X-CronWatch-Signature` header with `signature(secret, body)`, new in TypeScript (`@cronwatch/sdk/webhook`), Ruby (`Cronwatch::Alerts::Webhook.signature`), Python (`cronwatch.alerts.webhook.signature`) and PHP (`Webhook::signature`), as Go, Rust, Elixir, Java and .NET already had.
- Ruby: `client.routes(**options)`, the dashboard as a Rack app.
- Rust: constructors and builder methods for every data and options type.
- Rust: `StoredJob::read(name, text, created_at, updated_at)` reads a stored definition as the built-in stores do, for a store of the app's own that holds it as JSON text, and `StoredJob::is_readable` says whether it was a JSON object.
- Java: a static `of` on each record that may grow.
- PHP: a handler's `secret:` takes `Cronwatch\FromEnv::Read`, meaning the client's secret, as the 1.0 notes say to pass; it threw a `TypeError`.
- .NET: `Duration.ToMilliseconds(name)`, a duration read as the client reads one, to check a value before it is used.
- Drupal: Ultimate Cron's jobs are recorded, each on its own rules read as Ultimate Cron reads them (`drupal:<module>` for a module's `hook_cron`, `drupal:job:<id>` for any other, triggers `ultimate-cron` and `ultimate-cron-manual`), and every cron run is still `drupal:cron`. Before, the module left a site running Ultimate Cron alone.
- The [Stability](https://cronwatch.dev/docs/stability/), [Environment variables](https://cronwatch.dev/docs/environment/) and [Deprecations](https://cronwatch.dev/docs/deprecations/) pages.
- CI checks each package's public API: a committed report of it for TypeScript, Python and Elixir (`api.txt`), apidiff for the Go modules and cargo-semver-checks for the Rust crates, beside .NET's `PublicAPI.Unshipped.txt`.

### Changed

- The client's check loop is started with `startChecking(every)` (`start_checking` in Ruby, Python, Rust and Elixir, `StartChecking` in Go and .NET), since a job's `start()` opens a run. The client's `start(every)` still works, deprecated.
- `routes()` on the client is the one way to mount the dashboard. TypeScript's `createRoutes`, Ruby's and Python's `Web.new(client)` and `Web(client)`, and Java's `Routes.of` still work, deprecated.
- TypeScript: the client class is `Cronwatch`, with options `CronwatchOptions`, as every other language spells it; `CronWatch` and `CronWatchOptions` are deprecated aliases.
- Python: the triage class is `Anthropic` (was `AnthropicTriage`), and `Slack` and `Discord` take `webhook_url` by keyword.
- Ruby: `silence` takes the duration as `for:`, as the API and the MCP server do; `run(id)` without a block is `get_run(id)`.
- PHP: `$job->monitor($fn)` is the decorator (was `wrap`). Laravel's settings keys `store.prefix`, `store.create_tables`, `schedule.check` and `schedule.check_cron` are `table_prefix`, `create_tables`, `check.schedule` and `check.frequency`, as Symfony spells them; the old keys are still read, with a deprecation notice.
- Go: `PanicError` is gocron's panic type (was `Panic`), and `robfigcron.Converted` the one converted type. The `Watch` shortcuts are deprecated in favour of `New(cw, o).Option()`, and go in 2.0.
- Rust: axum and reqwest, both below 1.0, are leaving the core crate's API: `Routes::into_router()` and `ReqwestTransport::with_client` are deprecated and go in 1.0. Mount the tower service with `Router::new().nest_service(...)` instead.
- Java: `SqlStore` is in `dev.cronwatch.store`, beside `MemoryStore`, and the bridge is `SchedulerBridge`, as in .NET; the old names are deprecated.
- .NET: `CronwatchRequest` and `CronwatchResponse` (were `WebRequest` and `WebResponse`, whose names `System.Net` also has), `Adapters` (was `WebAdapters`), `SlackChannel.Webhook` and `DiscordChannel.Webhook` (were `Slack.Webhook` and `Discord.Webhook`), and the optional store interfaces named after their methods: `IUpdateRunIfStore`, `ICompareAndSetStateStore`, `IDeleteRunIfStore`.
- Public means documented. Each language marks the rest as internal in its own way (`@api private` in Ruby, underscored modules in Python, `@internal` in PHP, hidden from HexDocs in Elixir, internal or deprecated elsewhere). The scheduler bridges are for integration authors and outside the 1.x promise, and each store test kit promises only the entry point that runs the whole contract.
- `cronwatch-apalis` stays outside the 1.x promise while apalis 1.0 is a release candidate.

### Deprecated

Every deprecated name, with its replacement and the release it goes in, is on the [Deprecations](https://cronwatch.dev/docs/deprecations/) page. Renames of documented API keep working through 1.x and go in 2.0: the client's `start(every)` in every language; the dashboard's other constructors; TypeScript's `CronWatch` and `CronWatchOptions`; Python's `AnthropicTriage` and positional webhook URLs; PHP's `false` for "off" and `wrap`; Laravel's four old keys; Go's `Watch` shortcuts; Rust's `describe_job`; .NET's former type and interface names; Java's `jdbc.SqlStore` and `Bridge`. What was public by accident is deprecated now and goes in 1.0, in every language: `hmacSha256Hex` (`hmac_sha256_hex` in Python) wherever it was, the channels' and pg_cron's helpers and constants, Python's implementation modules' old names, .NET's `Json` helpers, and the store test kits' internals (Elixir's `StoreCase` helpers, .NET's `NewRun` and `ForeignRows`, Rust's `storetest` helpers, Go's `storetest` helpers). Rust's `Routes::into_router` and `ReqwestTransport::with_client` go in 1.0 too, since they hand out types of crates below 1.0.

### Fixed

- Ruby, Python and PHP keep the fields of a job's stored state and definition that they do not know, as the other languages did. A 0.x process in those languages erased what a newer release had written there (the queue of alerts being sent, for one) on its next write.
- Every language holds run ids to 200 characters on every path; `recordRun` let a longer one through, which the MySQL column could not hold.
- TypeScript: a schedule on a date no month has no longer runs out of stack.
- Craft CMS: `GET /cronwatch/api`, with no path after it, reaches the JSON API instead of the site's 404 page.
- One malformed job, run or state row (a hand edit, another writer, a damaged database) affects only its own job, in every language: it no longer stops every check or makes the whole dashboard answer 500. A definition that does not parse or is not an object reads as `{ name }`, and that job is reported and shown as failing; tags that are not a list of strings are left out; unparseable metrics read as `{}`; a time that is not a number reads as 0 (a start) or empty (a finish or duration); a state that does not parse reads as none and is replaced by the next write; and a queued alert that is not an object, a recovery that does not say what it recovers from, and an alert without a numeric time are dropped instead of blocking the job's alerts.
- An `Authorization` header that is not a bearer, such as a proxy's Basic auth, no longer locks the dashboard: the cookie and `?token=` sign in as if no header came.
- pg_cron: forgetting the old name of a renamed job while a run of it is open lets the run go, where every check after reported `job is not declared`.
- Ruby: the channels' HTTP client reads at most 1 MiB of an answer's body, and none of a 2xx answer's, where it read everything that came within the 10 second deadline, so a hostile or broken endpoint could stream gigabytes into the worker.
- Python: Celery in a Django project no longer falls back to a client in worker memory when the `CRONWATCH` settings fail to make one (a `STORE` path with a typo, an unknown key, a store factory that raises): the error is raised, so the check task fails naming it, where every run was kept in memory and lost and the check reported success. Only Django settings that are not configured fall back to `cronwatch.client()`.
- Python: a `DATABASE_URL` (or pg_cron connection string) that libpq cannot read, such as a password with a stray `%`, is reported naming only its host, where libpq's error quoted the password to `on_error` and the log on every check and run.
- Python: a host or forwarded host of more than 4300 digits is not an origin, where Python's limit on reading such a number made the dashboard answer 500 (and, in development, could keep the sign-in line from being printed).
- PHP: the HTTP client used without ext-curl keeps its 10 second deadline and its 1 MiB cap while a peer keeps sending. A chunked answer whose size line never ends grew memory without limit, and endless `100 Continue` answers hung the check for good.
- PHP: a queued alert whose run holds a value of the wrong type (`"startedAt": "x"`, an object error) no longer makes the job's state unreadable, which failed its checks and silence until the row was edited by hand; a failed retry of queued alerts is reported on its own and no longer stops the job's new alerts.
- PHP: a state row that is not JSON at all, which the MySQL and MariaDB stores' text column can hold as SQLite's can, no longer fails every write of that job's state there.
- Go, Rust, Elixir and Java: the same for their MySQL and MariaDB stores: a state row that is not JSON at all counts as version 0, so silencing the job or its next check replaces it, where every write of that job's state failed.
- Drupal and Craft CMS: the dashboard in the admin no longer passes the request's `Authorization` header on, so a `GET` of its `/api/check` with any bearer, from a user who may only view the dashboard, no longer runs the check.
- Drupal and Craft CMS: the dashboard in the admin pages ignores an `Authorization` header, so a user who may only view it can no longer run the check with a GET of `/api/check` and any bearer token; and a check through `GET /cronwatch/api/check` (a platform cron) or the embedded `/api/check` declares the site's jobs first, as a POST to `/check` did, where it left a removed job on its old schedule.
- Symfony: the scheduled jobs' default app tag (`symfony-scheduler:app-...`, without `app_id`) is a hash of the project directory, a deploy tool's release directory read as its root, where it was 48 bits of a fast hash of `APP_SECRET`, shown on the dashboard and in the API, against which a weak secret could be guessed offline. Jobs tagged from the secret are still taken for the app's own. Apps sharing a store, a table prefix and a path need an `app_id` each.
- Craft CMS: a watched queue job that another plugin cancels before it runs is no run at all, where it was left running or recorded failed as interrupted.
- WordPress: the webhook signing secret is saved exactly as typed, where saving it escaped `<`, removed `%XX` and collapsed spaces, so every signature failed at the receiver. One with a control character is refused, and the saved one kept.
- Laravel: `CRONWATCH_TOKEN=null` or `CRON_SECRET=null` in `.env` (or `(null)`, `empty`, `true`), which Laravel's `env()` reads as no value, no longer makes the word itself the dashboard's token or the cron secret. The token and secret are read only as Laravel reads them: a value the config gives is used or, when blank, means none, and only a config value of null reads the variable, through `env()`.
- Go: a queued alert's run keeps the fields a newer release added to it (an `attempt`, say) when the state is written back, as the alert's own fields and its details already did.
- Go: an alert of a type the release does not know leaves out Datadog's `alert_type`, Honeybadger's error class and Discord's embed colour, as the SDK does, where it sent them empty (or a colour of 0).
- Rust: a scheduler integration's worker that declares a job from the stored definition (one another process schedules) keeps the fields it has no option for, an app's `JobOptions::field` or one a newer release added, where it wrote the definition back without them.
- Elixir: `false` stays the way to turn a token or secret off, since `nil` means left out; a token, cron secret or handler secret of any other type was already refused, and the error now says so without printing the value.
- Elixir: a channel's, triage's or the instance's `transport:` options, which can hold a proxy's password, are left out when the struct is inspected (`IO.inspect`, a crash report).
- Elixir: the webhook body of a queued alert that carries a `schema` key of its own (another writer's) has one `schema` key, first, as the SDK writes it, where it had two and its signature differed.
- Elixir: `Cronwatch.Release.check/2` and `mix cronwatch.check` send alerts whatever the configuration's `deliver` says, so job nodes given `deliver: :check` have them sent; with `deliver: :check` they queued every alert and printed "sent". Called into a running instance that delivers at check time, the line says the alerts were queued, with a warning on standard error.
- Java: a check asked for after `close()` (a Spring Boot app stopping while the starter's tick was between its syncs and its check, say) throws, where it ran in the caller against the closed store and on SQLite opened a connection nothing closed again; the starter's `stop()` waits up to 30 seconds for a tick under way. A `close()` from a channel's code, or an error handler called from it, returns at once and finishes once the check has ended, where it waited for the channel's 15-second deadline and the alert was counted failed and sent again.
- Java: in a Quartz cluster of three or more nodes, a recovering firing fails only the run of the node that died, read from the name Quartz gives the recovering trigger, where it also failed a live node's run started in the same minute and dropped that run's real result.
- Java: a blank `cronwatch.cron-secret` in the Spring Boot starter falls back to `CRON_SECRET`, as a blank `cronwatch.web.token` falls back to `CRONWATCH_TOKEN`.
- .NET: a `CheckEvery` that is not a duration (`"1 minute"`) stops the host from starting, where it was only logged and the app ran on with no checks at all.
- .NET: `DisposeAsync` waits for a check under way to end before it disposes the store, as the SDK's `close()` does, where after five seconds the check went on writing to a disposed store.
- .NET: a process that stops just as a run's row is written records the run failed every time; the run was sometimes listed for the process-exit hook only after its row was written, and stayed running.
- .NET: a store that implemented a former store interface's method explicitly (`IStateCasStore.CompareAndSetStateAsync`) compiles again, and the deprecated `WebRequest` made from a `CronwatchRequest` reads its `IsTls`, `Mount` and `DeclaredLength`.

## 0.10.0 and earlier

Releases up to 0.10.0 (2026-09-30) are described in their [GitHub releases](https://github.com/cronwatchdev/cronwatch/releases), and the plugins' in their own changelogs.
