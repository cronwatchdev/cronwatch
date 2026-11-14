//! What the review after 0.14.3 found in the Rust port, each with the case
//! that shows it fixed.

mod common;

use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use common::{Boom, Kit, TestStore};
use cronwatch::web::{Request, RoutesOptions};
use cronwatch::{
    BoxError, BoxFuture, Client, Definition, JobOptions, JobState, MemoryStore, Run, RunStatus, StartOptions, Store,
    StoredJob,
};

/// A JSON object of distinct keys, as near `size` bytes as they fit.
fn object_of(size: usize, key: impl Fn(usize) -> String) -> (String, usize) {
    let mut body = String::with_capacity(size + 16);
    body.push('{');
    let mut n = 0;
    loop {
        let entry = format!("\"{}\":0,", key(n));
        if body.len() + entry.len() + 8 > size {
            break;
        }
        body.push_str(&entry);
        n += 1;
    }
    body.push_str("\"z\":0}");
    (body, n + 1)
}

/// A short name that starts with a letter, a different one for each `n`.
fn radix36(mut n: usize) -> String {
    let digits = b"0123456789abcdefghijklmnopqrstuvwxyz";
    let mut out = String::from("k");
    loop {
        out.push(digits[n % 36] as char);
        n /= 36;
        if n == 0 {
            return out;
        }
    }
}

/// Anyone can post to the sign-in route, and its body is read before the
/// token is known. An object's keys were found by scanning every one before
/// it, so a body at the 1 MiB cap with a hundred thousand keys held a worker
/// of the app's runtime for many seconds, whatever order the keys came in.
#[tokio::test]
async fn a_sign_in_body_of_many_keys_is_read_in_time() {
    let k = Kit::new();
    let routes = k.cw.routes(RoutesOptions::new().token("a-long-random-token")).unwrap();
    let cap = cronwatch::web::MAX_BODY;
    let named = object_of(cap, radix36);
    let ascending = object_of(cap, |i| i.to_string());
    let descending = object_of(cap, |i| (4_000_000_000usize - i).to_string());
    assert!(named.1 >= 109_000, "{} keys", named.1);
    for (what, (body, keys)) in [("named", named), ("ascending", ascending), ("descending", descending)] {
        assert!(keys > 60_000 && body.len() <= cap, "{what}: {keys} keys in {} bytes", body.len());
        let started = Instant::now();
        let answer = routes
            .handle(
                Request::new("POST", "/cronwatch/signin")
                    .with_header("host", "app.example.com")
                    .with_header("content-type", "application/json")
                    .with_body(body),
            )
            .await;
        let took = started.elapsed();
        assert_eq!(answer.status, 401, "{what}: no token among the keys");
        // Tens of milliseconds in a debug build; a scan per key takes minutes there.
        assert!(took < Duration::from_secs(5), "{what}: {keys} keys took {took:?}");
    }
}

/// A client spawns its writes on the runtime it was built on. Once that
/// runtime has ended they are dropped unrun, which went unsaid: the job ran,
/// nothing was recorded, and no error reached the handler; and a check left
/// its place taken for good.
#[test]
fn a_client_whose_runtime_has_ended_says_so_once() {
    let runtime = || tokio::runtime::Builder::new_multi_thread().worker_threads(1).enable_all().build().unwrap();
    let errors = Arc::new(Mutex::new(Vec::<(String, String)>::new()));
    let kept = errors.clone();
    let first = runtime();
    let cw = first.block_on(async {
        Client::builder()
            .alerts([])
            .on_error(move |err, where_| kept.lock().unwrap().push((where_.to_string(), err.to_string())))
            .build()
            .unwrap()
    });
    let job = cw.job("orphan", JobOptions::new()).unwrap();
    drop(first);

    runtime().block_on(async {
        // The function still runs, and its own outcome is returned.
        assert!(job.run(|_| async { Err::<(), _>(Boom("down")) }).await.is_err());
        job.run(|_| async { Ok::<_, Boom>(()) }).await.unwrap();
        assert!(cw.store().list_runs("orphan", 10).await.unwrap().is_empty(), "nothing could be recorded");
        let said = errors.lock().unwrap().clone();
        assert_eq!(said.len(), 1, "said once: {said:?}");
        assert_eq!(said[0].0, "recording orphan");
        assert!(said[0].1.contains("runtime this client was built on has shut down"), "{}", said[0].1);

        // A check fails, and each one after it is a check of its own.
        let ended = "the check ended before it could be read";
        assert_eq!(cw.check().await.unwrap_err().to_string(), ended);
        assert_eq!(cw.check().await.unwrap_err().to_string(), ended);
        assert_eq!(errors.lock().unwrap().len(), 1, "still said once");
    });
}

/// The same, when a check is the first thing the client is asked for.
#[test]
fn a_check_on_an_ended_runtime_says_so() {
    let runtime = || tokio::runtime::Builder::new_current_thread().enable_all().build().unwrap();
    let errors = Arc::new(Mutex::new(Vec::<(String, String)>::new()));
    let kept = errors.clone();
    let first = runtime();
    let cw = first.block_on(async {
        Client::builder()
            .on_error(move |err, where_| kept.lock().unwrap().push((where_.to_string(), err.to_string())))
            .build()
            .unwrap()
    });
    drop(first);
    runtime().block_on(async {
        assert!(cw.check().await.is_err());
        assert!(cw.check().await.is_err());
    });
    let said = errors.lock().unwrap().clone();
    assert_eq!(said.len(), 1, "{said:?}");
    assert_eq!(said[0].0, "check");
}

/// Reading a job's newest run copied every run it had, output and all,
/// under the store's lock, and only then kept the one asked for. Reading
/// one run is now far cheaper than reading them all, on any machine.
#[tokio::test]
async fn the_memory_store_copies_only_the_runs_it_answers() {
    const RUNS: usize = 300;
    let store = MemoryStore::new();
    // Far more output than a client keeps for a run, so the copying is what
    // the clock measures.
    let output = "x".repeat(256 * 1024);
    for i in 0..RUNS {
        let mut run = Run::new(format!("r{i}"), "busy", RunStatus::Ok, 1_700_000_000_000 + i as i64 * 60_000);
        run.finished_at = Some(run.started_at + 1000);
        run.duration_ms = Some(1000);
        run.output = Some(output.clone());
        store.insert_run(&run).await.unwrap();
    }
    let time = |limit: usize| {
        let store = &store;
        async move {
            let started = Instant::now();
            for _ in 0..20 {
                assert_eq!(store.list_runs("busy", limit).await.unwrap().len(), limit);
                if limit == 1 {
                    assert_eq!(store.last_run("busy").await.unwrap().unwrap().id, format!("r{}", RUNS - 1));
                }
            }
            started.elapsed()
        }
    };
    // The best of three, so one slow moment on a busy machine decides nothing.
    let mut one = Duration::MAX;
    let mut all = Duration::MAX;
    for _ in 0..3 {
        one = one.min(time(1).await);
        all = all.min(time(RUNS).await);
    }
    assert!(one * 4 < all, "40 reads of one run took {one:?}, 20 reads of all {RUNS} took {all:?}");
    let newest = store.list_runs("busy", 3).await.unwrap();
    let ids: Vec<&str> = newest.iter().map(|r| r.id.as_str()).collect();
    assert_eq!(ids, ["r299", "r298", "r297"]);
}

/// A running row whose start is the lowest a store holds (a foreign or
/// damaged row) is drawn before any check has marked it timed out and
/// pruned it: the lane's arithmetic saturates rather than overflow.
#[tokio::test]
async fn the_dashboard_draws_a_run_started_at_the_lowest_time_before_a_check() {
    let k = Kit::new();
    k.cw.job("old", JobOptions::new().schedule("every 5m"))
        .unwrap()
        .run(|_| async { Ok::<_, Boom>(()) })
        .await
        .unwrap();
    for (id, at) in [("long-ago", i64::MIN + 1), ("lowest", i64::MIN)] {
        let mut run = Run::new(id, "old", RunStatus::Running, at);
        run.trigger = "run".into();
        k.cw.store().insert_run(&run).await.unwrap();
    }
    let routes = k.cw.routes(RoutesOptions::new().no_token()).unwrap();
    for target in ["/cronwatch", "/cronwatch/jobs/old"] {
        let res = routes.handle(Request::new("GET", target).with_header("host", "localhost")).await;
        assert_eq!(res.status, 200, "{target}: {:?}", k.errors.lock().unwrap());
    }
    assert!(k.errors.lock().unwrap().is_empty(), "{:?}", k.errors.lock().unwrap());
}

/// A secret that starts in one flush and ends in the next was redacted a
/// flush at a time, so its second half was stored and shown. Each flush,
/// and the finish, is redacted together with what the handle flushed last.
#[tokio::test]
async fn a_secret_split_across_two_flushes_is_redacted_whole() {
    let k = Kit::new();
    let job = k.cw.job("deploy", JobOptions::new()).unwrap();
    let run = job.start(StartOptions::new()).await.unwrap();
    let stored = || async { k.cw.get_run(run.id()).await.unwrap().unwrap().output };
    run.log("starting");
    run.log("-----BEGIN PRIVATE KEY-----");
    run.log("MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQC7VJTUt9Us8cKj");
    run.flush().await;
    assert_eq!(stored().await.as_deref(), Some("starting\n[redacted]"));
    run.log("MFNRV0X0e1nSecondHalfOfTheKeySecondHalfOfTheKeySecondHalfOfTheKe");
    run.log("-----END PRIVATE KEY-----");
    run.log("api_token:");
    run.flush().await;
    assert_eq!(stored().await.as_deref(), Some("starting\n[redacted]\napi_token:"));
    // A label flushed, then its value: at the next flush, and again at finish.
    run.log("hunter2hunter2");
    run.flush().await;
    assert_eq!(stored().await.as_deref(), Some("starting\n[redacted]\napi_token:\n[redacted]"));
    run.log("password:");
    run.flush().await;
    run.log("correct-horse");
    let finished = run.finish().await.expect("a finished run");
    assert_eq!(finished.output.as_deref(), Some("starting\n[redacted]\napi_token:\n[redacted]\npassword:\n[redacted]"));
    let row = k.cw.get_run(run.id()).await.unwrap().unwrap().to_json();
    for leaked in ["SecondHalf", "hunter2", "correct-horse"] {
        assert!(!row.contains(leaked), "{leaked} in {row}");
    }
}

/// The handle's memory of its last flush is only used while the stored
/// output still ends with it, and stays bounded however long the run.
#[tokio::test]
async fn flushes_after_another_process_wrote_still_append_each_redacted() {
    let k = Kit::new();
    let job = k.cw.job("deploy", JobOptions::new()).unwrap();
    let run = job.start(StartOptions::new()).await.unwrap();
    run.log("one");
    run.flush().await;
    // Another handle on the same run (another process) appends in between.
    let other = job.resume(run.id()).await.unwrap();
    other.log("from elsewhere");
    other.flush().await;
    run.log("token=abc123abc123");
    run.flush().await;
    let stored = k.cw.get_run(run.id()).await.unwrap().unwrap().output;
    assert_eq!(stored.as_deref(), Some("one\nfrom elsewhere\ntoken=[redacted]"));
    // A long run: the remembered tail stays bounded and the output capped.
    for i in 0..6 {
        run.log(format!("line {i} {}", "x".repeat(6000)));
        run.flush().await;
    }
    run.log("secret=tail-secret-value");
    let output = run.finish().await.expect("a finished run").output.expect("output");
    assert!(output.len() <= 16 * 1024 + 40, "{} bytes", output.len());
    assert!(output.ends_with("secret=[redacted]"), "{}", &output[output.len() - 40..]);
    assert!(!output.contains("tail-secret-value"));
}

/// After `forget`, a handle kept from before it wrote the job's row on
/// every run and the job stayed out of the declared set. Its next run now
/// declares the job again, and writes it once.
#[tokio::test]
async fn a_handle_kept_across_a_forget_declares_the_job_again_and_writes_it_once() {
    let k = Kit::new();
    let names = || k.cw.defined_jobs().iter().map(|d| d.name().to_string()).collect::<Vec<_>>();
    let written = || async { k.cw.store().get_job("nightly").await.unwrap().map(|j| j.updated_at) };
    let job = k.cw.job("nightly", JobOptions::new().schedule("0 2 * * *").description("Nightly export")).unwrap();
    job.run(|_| async { Ok::<_, Boom>(()) }).await.unwrap();
    k.cw.forget("nightly").await.unwrap();
    assert!(names().is_empty());

    k.advance(1000);
    job.run(|_| async { Ok::<_, Boom>(()) }).await.unwrap();
    let first = written().await.expect("written again");
    assert_eq!(first, k.now());
    assert_eq!(names(), ["nightly"], "declared again, so checks and sources see it");
    for _ in 0..2 {
        k.advance(1000);
        job.run(|_| async { Ok::<_, Boom>(()) }).await.unwrap();
    }
    assert_eq!(written().await, Some(first), "written once, not on every run");
    assert_eq!(k.summary("nightly").await.unwrap().definition.description(), "Nightly export");

    // Forgotten by another process since: this one's check writes it again.
    k.cw.store().delete_job("nightly").await.unwrap();
    k.check().await;
    assert!(k.cw.store().get_job("nightly").await.unwrap().is_some());

    // A run that spans the forget brings the job back too, at its finish
    // when its start was never written.
    k.cw.forget("nightly").await.unwrap();
    let handle = job.start(StartOptions::new()).await.unwrap();
    assert_eq!(names(), ["nightly"]);
    handle.finish().await.expect("finished");
}

/// A check or a dashboard read that was about to write a job's declaration
/// when the job was forgotten does not bring it back; its next run does.
#[tokio::test]
async fn a_forget_that_lands_while_a_read_is_writing_declarations_is_not_undone() {
    // The summary of one job takes the same path (a write that is not for a
    // run); its only write is the forgotten job's own, which this store's
    // gate holds too late to show.
    for read in ["check", "jobs"] {
        let store = Arc::new(TestStore::default());
        let k = Kit::with(|b| b.store_arc(store.clone()));
        k.cw.job("keep", JobOptions::new().schedule("0 2 * * *")).unwrap();
        k.cw.job("nightly", JobOptions::new().schedule("0 2 * * *")).unwrap();
        // Neither is written yet, so the read's own loop is what would write
        // them. Its first write, of "keep", waits: by then it has picked
        // both names up.
        store.holds_upsert.store(true, Ordering::SeqCst);
        let cw = k.cw.clone();
        let reading = tokio::spawn(async move {
            match read {
                "check" => cw.check().await.map(|_| ()),
                _ => cw.jobs().await.map(|_| ()),
            }
        });
        store.upsert_waiting.notified().await;
        let forgetting = {
            let cw = k.cw.clone();
            tokio::spawn(async move { cw.forget("nightly").await })
        };
        for _ in 0..50 {
            tokio::task::yield_now().await;
        }
        store.upsert_release.notify_one();
        forgetting.await.unwrap().unwrap();
        reading.await.unwrap().unwrap();
        let names: Vec<String> = k.cw.defined_jobs().iter().map(|d| d.name().to_string()).collect();
        assert_eq!(names, ["keep"], "{read}: still forgotten here");
        assert!(store.get_job("nightly").await.unwrap().is_none(), "{read}: not written again");
        assert!(store.get_state("nightly").await.unwrap().is_none(), "{read}: no state left behind");
        // Its next run still brings it back.
        k.cw.run("nightly", None, |_| async { Ok::<_, Boom>(()) }).await.unwrap().unwrap();
        assert!(store.get_job("nightly").await.unwrap().is_some(), "{read}");
    }
}

/// An error handler that panics is the handler's own failure: the check,
/// the jobs after the one that could not be read, and a dashboard read all
/// carry on.
#[tokio::test]
async fn an_error_handler_that_panics_does_not_stop_a_check_or_a_read() {
    let store = Arc::new(TestStore::default());
    let calls = Arc::new(AtomicUsize::new(0));
    let counted = calls.clone();
    let k = Kit::with(|b| {
        b.store_arc(store.clone()).on_error(move |_, _| {
            counted.fetch_add(1, Ordering::SeqCst);
            panic!("the handler panicked");
        })
    });
    for name in ["a", "b"] {
        let job = k.cw.job(name, JobOptions::new().schedule("every 5m")).unwrap();
        job.run(|_| async { Ok::<_, Boom>(()) }).await.unwrap();
    }
    store.breaks(&["prune", "running_runs"]);
    assert!(k.cw.check().await.is_err(), "the store failing as the check starts is the check's error");
    store.mends(&["running_runs"]);
    k.advance(30 * 60_000);
    let result = k.cw.check().await.expect("the check goes through");
    assert_eq!(result.jobs.len(), 2);
    assert_eq!(k.types(), ["missed", "missed"], "both jobs were checked");
    assert!(calls.load(Ordering::SeqCst) >= 1, "the handler was called, for the prune");
    assert_eq!(k.cw.jobs().await.unwrap().len(), 2);
    let routes = k.cw.routes(RoutesOptions::new().no_token()).unwrap();
    let res = routes.handle(Request::new("GET", "/cronwatch").with_header("host", "localhost")).await;
    assert_eq!(res.status, 200);
}

/// A job that hangs on every run used to alert stuck at every timeout,
/// since each new start closed it. A start now closes only missed.
#[tokio::test]
async fn a_job_that_hangs_on_every_run_alerts_stuck_once_and_recovers_on_its_first_success() {
    const MIN: i64 = 60_000;
    let k = Kit::new();
    let job = k.cw.job("hangs", JobOptions::new().schedule("*/5 * * * *").timezone("UTC").timeout("10m")).unwrap();
    // Twelve runs, five minutes apart, none of which ever finishes.
    let mut handles = Vec::new();
    for _ in 0..12 {
        handles.push(job.start(StartOptions::new()).await.unwrap());
        k.advance(5 * MIN);
        k.check().await;
    }
    assert_eq!(k.types(), ["stuck"]);
    assert_eq!(k.summary("hangs").await.unwrap().health, cronwatch::JobHealth::Stuck);
    // A run that fails alerts as failed by its own rule; stuck stays open beside it.
    assert!(job.run(|_| async { Err::<(), _>(Boom("boom")) }).await.is_err());
    assert_eq!(k.types(), ["stuck", "failed"]);
    let open = |summary: cronwatch::JobSummary| {
        let mut open: Vec<String> = summary.open.iter().map(|c| c.as_str().to_string()).collect();
        open.sort_unstable();
        open
    };
    assert_eq!(open(k.summary("hangs").await.unwrap()), ["failed", "stuck"]);
    // A hung run that reports failing late is not counted again and alerts nothing.
    handles[0].fail("late").await;
    assert_eq!(k.types(), ["stuck", "failed"]);
    // The first success closes both with one recovery.
    job.run(|_| async { Ok::<_, Boom>(()) }).await.unwrap();
    assert_eq!(k.types(), ["stuck", "failed", "recovered"]);
    let cronwatch::AlertDetails::Recovered { after, .. } = &k.alert_list()[2].details else {
        panic!("not a recovery");
    };
    let mut after: Vec<&str> = after.iter().map(|c| c.as_str()).collect();
    after.sort_unstable();
    assert_eq!(after, ["failed", "stuck"]);
    assert!(open(k.summary("hangs").await.unwrap()).is_empty());
}

#[tokio::test]
async fn a_stuck_run_that_finishes_ok_late_recovers_though_other_runs_started_meanwhile() {
    let k = Kit::new();
    let job = k.cw.job("slow-queue", JobOptions::new().timeout("10m")).unwrap();
    let first = job.start(StartOptions::new()).await.unwrap();
    k.advance(11 * 60_000);
    k.check().await;
    assert_eq!(k.types(), ["stuck"]);
    let second = job.start(StartOptions::new()).await.unwrap();
    let open: Vec<String> =
        k.summary("slow-queue").await.unwrap().open.iter().map(|c| c.as_str().to_string()).collect();
    assert_eq!(open, ["stuck"], "a start leaves stuck open");
    first.finish().await.expect("finished late");
    assert_eq!(k.types(), ["stuck", "recovered"]);
    second.finish().await.expect("finished");
    assert_eq!(k.types(), ["stuck", "recovered"]);
}

/// A memory store whose first `insert_run` (a run's start) waits until it
/// is let go, as a store that hangs does; with `down`, every insert after
/// it fails, as a store that is down does.
struct SlowStart {
    inner: MemoryStore,
    inserts: AtomicUsize,
    release: tokio::sync::Semaphore,
    down: bool,
}

impl SlowStart {
    fn new(down: bool) -> Arc<SlowStart> {
        Arc::new(SlowStart {
            inner: MemoryStore::new(),
            inserts: AtomicUsize::new(0),
            release: tokio::sync::Semaphore::new(0),
            down,
        })
    }
}

impl Store for SlowStart {
    fn init(&self) -> BoxFuture<'_, Result<(), BoxError>> {
        self.inner.init()
    }
    fn upsert_job<'a>(&'a self, d: &'a Definition, now: i64) -> BoxFuture<'a, Result<(), BoxError>> {
        self.inner.upsert_job(d, now)
    }
    fn get_job<'a>(&'a self, name: &'a str) -> BoxFuture<'a, Result<Option<StoredJob>, BoxError>> {
        self.inner.get_job(name)
    }
    fn list_jobs(&self) -> BoxFuture<'_, Result<Vec<StoredJob>, BoxError>> {
        self.inner.list_jobs()
    }
    fn delete_job<'a>(&'a self, name: &'a str) -> BoxFuture<'a, Result<(), BoxError>> {
        self.inner.delete_job(name)
    }
    fn insert_run<'a>(&'a self, run: &'a Run) -> BoxFuture<'a, Result<(), BoxError>> {
        Box::pin(async move {
            if self.inserts.fetch_add(1, Ordering::SeqCst) == 0 {
                self.release.acquire().await.expect("an open gate").forget();
            } else if self.down {
                return Err("store down".into());
            }
            self.inner.insert_run(run).await
        })
    }
    fn update_run<'a>(&'a self, run: &'a Run) -> BoxFuture<'a, Result<(), BoxError>> {
        self.inner.update_run(run)
    }
    fn get_run<'a>(&'a self, id: &'a str) -> BoxFuture<'a, Result<Option<Run>, BoxError>> {
        self.inner.get_run(id)
    }
    fn list_runs<'a>(&'a self, job: &'a str, limit: usize) -> BoxFuture<'a, Result<Vec<Run>, BoxError>> {
        self.inner.list_runs(job, limit)
    }
    fn running_runs(&self) -> BoxFuture<'_, Result<Vec<Run>, BoxError>> {
        self.inner.running_runs()
    }
    fn get_state<'a>(&'a self, job: &'a str) -> BoxFuture<'a, Result<Option<JobState>, BoxError>> {
        self.inner.get_state(job)
    }
    fn set_state<'a>(&'a self, state: &'a JobState) -> BoxFuture<'a, Result<(), BoxError>> {
        self.inner.set_state(state)
    }
    fn prune(&self, before: i64) -> BoxFuture<'_, Result<u64, BoxError>> {
        self.inner.prune(before)
    }
    fn close(&self) -> BoxFuture<'_, Result<(), BoxError>> {
        self.inner.close()
    }
    fn update_run_if<'a>(&'a self, run: &'a Run, from: &'a [RunStatus]) -> BoxFuture<'a, Result<bool, BoxError>> {
        self.inner.update_run_if(run, from)
    }
    fn compare_and_set_state<'a>(
        &'a self,
        state: &'a JobState,
        expected: i64,
    ) -> BoxFuture<'a, Result<bool, BoxError>> {
        self.inner.compare_and_set_state(state, expected)
    }
    fn delete_run_if<'a>(
        &'a self,
        id: &'a str,
        job: &'a str,
        status: &'a RunStatus,
    ) -> BoxFuture<'a, Result<bool, BoxError>> {
        self.inner.delete_run_if(id, job, status)
    }
}

/// Lets every task that can run get as far as it can, without letting the
/// paused clock move.
async fn settle() {
    for _ in 0..100 {
        tokio::task::yield_now().await;
    }
}

/// `run` waited on the store for as long as its start took to be written,
/// so a store that hung held the job back for good. It now gives the two
/// writes five seconds, runs the job unrecorded, and finishes the run
/// wherever the late write leaves it.
#[tokio::test(start_paused = true)]
async fn a_store_that_hangs_recording_a_start_does_not_hold_the_job_back_and_leaves_no_running_row() {
    let store = SlowStart::new(false);
    let k = Kit::with(|b| b.store_arc(store.clone()));
    let job = k.cw.job("nightly", JobOptions::new()).unwrap();
    let ran = Arc::new(AtomicUsize::new(0));
    let running = {
        let ran = ran.clone();
        tokio::spawn(async move {
            job.run(|j| async move {
                ran.fetch_add(1, Ordering::SeqCst);
                j.log("did the work");
                Ok::<_, Boom>("done")
            })
            .await
        })
    };
    settle().await;
    assert_eq!(ran.load(Ordering::SeqCst), 0, "still waiting on the store");
    tokio::time::advance(Duration::from_millis(4_999)).await;
    settle().await;
    assert_eq!(ran.load(Ordering::SeqCst), 0);
    tokio::time::advance(Duration::from_millis(1)).await;
    settle().await;
    assert_eq!(ran.load(Ordering::SeqCst), 1, "the job ran once the deadline passed");
    let said = || k.errors.lock().unwrap().iter().map(|(w, e)| format!("{w}: {e}")).collect::<Vec<_>>();
    assert_eq!(
        said(),
        ["recording nightly: the store took longer than 5s to record the start of nightly; running it unrecorded"]
    );
    // The late start lands, wherever the finish has got to.
    store.release.add_permits(1);
    assert_eq!(running.await.unwrap().unwrap(), "done");
    settle().await;
    let runs = k.runs("nightly").await;
    assert_eq!(runs.len(), 1);
    assert_eq!(runs[0].status, RunStatus::Ok);
    assert_eq!(runs[0].output.as_deref(), Some("did the work"));
    assert!(store.inner.running_runs().await.unwrap().is_empty());
    assert_eq!(said().len(), 1, "{:?}", said());
    assert!(k.types().is_empty());
}

/// The late start lands while the job is still running: the finish is
/// written over it as over any running row.
#[tokio::test(start_paused = true)]
async fn a_start_write_that_lands_while_the_job_runs_is_finished_over() {
    let store = SlowStart::new(false);
    let k = Kit::with(|b| b.store_arc(store.clone()));
    let job = k.cw.job("nightly", JobOptions::new()).unwrap();
    let gate = Arc::new(tokio::sync::Notify::new());
    let running = {
        let gate = gate.clone();
        tokio::spawn(async move {
            job.run(|_| async move {
                gate.notified().await;
                Err::<(), _>(Boom("boom"))
            })
            .await
        })
    };
    settle().await;
    tokio::time::advance(Duration::from_secs(5)).await;
    settle().await;
    store.release.add_permits(1);
    settle().await;
    let begun = store.inner.running_runs().await.unwrap();
    assert_eq!(begun.len(), 1, "the start landed, as the run began");
    assert_eq!(begun[0].finished_at, None);
    gate.notify_one();
    assert!(running.await.unwrap().is_err());
    settle().await;
    let runs = k.runs("nightly").await;
    assert_eq!(runs.len(), 1);
    assert_eq!(runs[0].status, RunStatus::Failed);
    assert!(store.inner.running_runs().await.unwrap().is_empty());
    assert_eq!(k.types(), ["failed"]);
    assert_eq!(k.errors.lock().unwrap().len(), 1, "only the deadline: {:?}", k.errors.lock().unwrap());
}

#[tokio::test(start_paused = true)]
async fn a_start_write_that_lands_after_the_finish_failed_is_finished_then_not_left_running() {
    let store = SlowStart::new(true);
    let k = Kit::with(|b| b.store_arc(store.clone()));
    let job = k.cw.job("nightly", JobOptions::new()).unwrap();
    let running = tokio::spawn(async move { job.run(|_| async { Err::<(), _>(Boom("boom")) }).await });
    settle().await;
    tokio::time::advance(Duration::from_secs(5)).await;
    let err = running.await.unwrap().unwrap_err();
    assert_eq!(err.to_string(), "boom", "the job's own error is what run returns");
    assert_eq!(k.wheres(), ["recording nightly", "recording nightly"]);
    assert!(k.runs("nightly").await.is_empty());
    // The start write lands at last, after the finish gave up: the run is
    // finished and judged now.
    store.release.add_permits(1);
    settle().await;
    let runs = k.runs("nightly").await;
    assert_eq!(runs.len(), 1);
    assert_eq!(runs[0].status, RunStatus::Failed);
    assert!(runs[0].error.as_deref().unwrap().contains("boom"));
    assert!(store.inner.running_runs().await.unwrap().is_empty());
    assert_eq!(k.types(), ["failed"]);
}

/// A run given back while its start was still being written has no row to
/// take back yet: it is taken back when the write lands.
#[tokio::test(start_paused = true)]
async fn a_start_write_that_lands_after_the_run_was_given_back_is_taken_back() {
    let store = SlowStart::new(false);
    let k = Kit::with(|b| b.store_arc(store.clone()));
    let job = k.cw.job("queued", JobOptions::new()).unwrap();
    let running = tokio::spawn(async move {
        job.run_or_discard(cronwatch::RunOptions::new(), |_: &Boom| true, |_| async { Err::<(), _>(Boom("later")) })
            .await
    });
    settle().await;
    tokio::time::advance(Duration::from_secs(5)).await;
    assert!(running.await.unwrap().is_err());
    store.release.add_permits(1);
    settle().await;
    assert!(k.runs("queued").await.is_empty(), "no row left, running or otherwise");
    assert!(k.types().is_empty());
}

/// A store that fails, as opposed to hangs, is reported as before and the
/// finish writes the whole run.
#[tokio::test]
async fn a_store_that_fails_recording_a_start_is_reported_and_the_finish_writes_the_run() {
    let store = Arc::new(TestStore::default());
    let k = Kit::with(|b| b.store_arc(store.clone()));
    let job = k.cw.job("nightly", JobOptions::new()).unwrap();
    store.breaks(&["insert_run"]);
    let mending = store.clone();
    job.run(|_| async move {
        mending.mends(&[]);
        Ok::<_, Boom>(())
    })
    .await
    .unwrap();
    assert_eq!(k.messages(), ["insert_run failed"]);
    assert_eq!(k.wheres(), ["recording nightly"]);
    let runs = k.runs("nightly").await;
    assert_eq!(runs.len(), 1);
    assert_eq!(runs[0].status, RunStatus::Ok);
}
