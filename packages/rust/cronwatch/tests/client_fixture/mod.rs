//! Replays `conformance/client.json`, the first fixture driven through the
//! client's public API rather than a pure function (the SDK's replay is
//! `packages/sdk/test/unknown-fields.test.ts`): the run ids `start`,
//! `resume`, and `record_run` take (`runIds`), and what a client keeps of
//! stored data a newer release wrote (`unknownFields`). Shared by the core's
//! tests, over the memory store, and `cronwatch-sqlx`'s, over SQLite and the
//! servers, through `#[path]`.
#![allow(dead_code)]

use std::sync::atomic::{AtomicI64, Ordering};
use std::sync::{Arc, Mutex};

use cronwatch::js::{self, Object, Value};
use cronwatch::{
    Alert, Client, JobOptions, JobState, MemoryStore, RecordOptions, Run, StartOptions, Store, StoredJob, channel_fn,
};

/// 2026-01-05 09:30:00 UTC, the fixture's `T0`.
const T0: i64 = 1_767_605_400_000;

/// `conformance/client.json`, from the repository.
pub fn fixture() -> Object {
    let path = concat!(env!("CARGO_MANIFEST_DIR"), "/../../../conformance/client.json");
    let text = std::fs::read_to_string(path).unwrap_or_else(|e| panic!("{path}: {e}"));
    match js::parse(&text) {
        Ok(Value::Object(o)) => o,
        other => panic!("client.json is not an object: {other:?}"),
    }
}

fn field<'a>(o: &'a Object, key: &str) -> &'a Value {
    o.get(key).unwrap_or_else(|| panic!("no {key} in {}", o.to_json()))
}

fn text<'a>(o: &'a Object, key: &str) -> &'a str {
    field(o, key).as_str().unwrap_or_else(|| panic!("{key} is not a string"))
}

fn int(o: &Object, key: &str) -> i64 {
    field(o, key).as_f64().unwrap_or_else(|| panic!("{key} is not a number")) as i64
}

fn objects(v: &Value) -> Vec<&Object> {
    v.as_array().expect("a list").iter().map(|x| x.as_object().expect("an object")).collect()
}

/// A JSON value with every object's keys sorted, written out: two values
/// are the same JSON when these are equal, whatever their key order.
pub fn canonical(v: &Value) -> String {
    fn sort(v: &Value) -> Value {
        match v {
            Value::Object(o) => {
                let mut keys: Vec<&str> = o.keys().collect();
                keys.sort_unstable();
                let mut out = Object::new();
                for k in keys {
                    out.set(k, sort(o.get(k).unwrap()));
                }
                Value::Object(out)
            }
            Value::Array(list) => Value::Array(list.iter().map(sort).collect()),
            other => other.clone(),
        }
    }
    sort(v).to_json()
}

/// `runIds`: for each method, a fresh client over a memory store, job `j`,
/// the clock at `T0`; each id is taken or refused with the SDK's words
/// (`recordRun`'s method name is the same in Rust). Answers how many cases
/// ran.
pub async fn replay_run_ids() -> usize {
    let f = fixture();
    let cases = objects(field(&f, "runIds"));
    let mut wrong = Vec::new();
    for method in ["start", "resume", "recordRun"] {
        let cw = Client::builder()
            .store(MemoryStore::new())
            .alerts([])
            .clock(|| T0)
            .on_error(|_, _| {})
            .build()
            .expect("a client");
        let job = cw.job("j", JobOptions::new()).expect("job j");
        for c in cases.iter().filter(|c| text(c, "method") == method) {
            let id = text(c, "id").to_string();
            let got = match method {
                "start" => match job.start(StartOptions::new().id(id.clone())).await {
                    Ok(handle) => {
                        handle.finish().await;
                        Ok(())
                    }
                    Err(e) => Err(e.to_string()),
                },
                "resume" => job.resume(&id).await.map(|_| ()).map_err(|e| e.to_string()),
                _ => {
                    let run = Run::from_value(&Value::Object(
                        Object::new()
                            .with("id", id.as_str())
                            .with("job", "j")
                            .with("status", "ok")
                            .with("startedAt", T0 - 1000)
                            .with("finishedAt", T0)
                            .with("durationMs", 1000)
                            .with("error", Value::Null)
                            .with("output", Value::Null)
                            .with("metrics", Object::new())
                            .with("trigger", "run"),
                    ))
                    .expect("a run");
                    cw.record_run(run, RecordOptions::new()).await.map(|_| ()).map_err(|e| e.to_string())
                }
            };
            let want = match c.get("error") {
                Some(Value::String(e)) => Err(e.clone()),
                _ => Ok(()),
            };
            if got != want {
                wrong.push(format!("{method}({}): got {got:?}, want {want:?}", js::stringify(field(c, "id"))));
            }
        }
        let _ = cw.close().await;
    }
    assert!(wrong.is_empty(), "client.json runIds: {} cases differ:\n{}", wrong.len(), wrong.join("\n"));
    cases.len()
}

fn stored_job(job: &StoredJob) -> Value {
    Value::Object(
        Object::new()
            .with("name", job.name.as_str())
            .with("definition", job.definition.as_object().clone())
            .with("createdAt", job.created_at)
            .with("updatedAt", job.updated_at),
    )
}

/// `unknownFields`: seeds `store` with what a newer release wrote (a
/// definition and a state with keys this release does not know, a run whose
/// status it does not know, a trigger it does not know, an open condition it
/// does not know), then checks, silences, unsilences, reads the summary, and
/// declares and runs the job, comparing after each step what the store
/// reads back and what was sent as JSON values. Answers how many steps ran.
pub async fn replay_unknown_fields(store: Arc<dyn Store>) -> usize {
    let f = fixture();
    let fx = field(&f, "unknownFields").as_object().expect("unknownFields");
    let seed = field(fx, "seed").as_object().expect("seed");
    store.init().await.expect("init");
    let definition = cronwatch::Definition::from_object(field(seed, "definition").as_object().unwrap().clone());
    store.upsert_job(&definition, int(seed, "createdAt")).await.expect("upsert_job");
    for run in objects(field(seed, "runs")) {
        let run = Run::from_value(&Value::Object(run.clone())).expect("a seeded run");
        store.insert_run(&run).await.expect("insert_run");
    }
    let state = JobState::from_value(field(seed, "state")).expect("the seeded state");
    store.set_state(&state).await.expect("set_state");

    let clock = Arc::new(AtomicI64::new(0));
    let sent: Arc<Mutex<Vec<Alert>>> = Arc::default();
    let errors: Arc<Mutex<Vec<String>>> = Arc::default();
    let (now, sink, kept) = (clock.clone(), sent.clone(), errors.clone());
    let cw = Client::builder()
        .store_arc(store.clone())
        .clock(move || now.load(Ordering::SeqCst))
        .no_cron_secret()
        .alerts([channel_fn("capture", move |alert: Alert| {
            sink.lock().unwrap().push(alert);
            async { Ok(()) }
        })])
        .on_error(move |err, where_| kept.lock().unwrap().push(format!("{where_}: {err}")))
        .build()
        .expect("a client");

    let steps = objects(field(fx, "steps"));
    let mut wrong = Vec::new();
    for step in &steps {
        let op = text(step, "op");
        if step.has("at") {
            clock.store(int(step, "at"), Ordering::SeqCst);
        }
        match op {
            "check" => {
                cw.check().await.expect("check");
            }
            "silence" => {
                cw.silence("keep", text(step, "for")).await.expect("silence");
            }
            "unsilence" => {
                cw.unsilence("keep").await.expect("unsilence");
            }
            "summary" => {
                // `open` follows the stored state's key order, which
                // Postgres's JSONB does not keep: compared as a set.
                let sorted = |v: Value| {
                    let Value::Object(mut o) = v else { panic!("a summary") };
                    let mut open: Vec<Value> = o.get("open").and_then(Value::as_array).cloned().unwrap_or_default();
                    open.sort_by_key(|c| c.as_str().unwrap_or("").to_string());
                    o.set("open", open);
                    canonical(&Value::Object(o))
                };
                let got = cw.job_summary("keep").await.expect("job_summary").expect("a summary").to_value();
                let (got, want) = (sorted(got), sorted(field(step, "summary").clone()));
                if got != want {
                    wrong.push(format!("summary:\n  got  {got}\n  want {want}"));
                }
            }
            "declareAndRun" => {
                let declared = field(step, "declared").as_object().expect("declared");
                let mut options = JobOptions::new();
                for (k, v) in declared.iter() {
                    options = match k {
                        "timeout" => options.timeout(v.as_str().unwrap()),
                        "tags" => options.tags(v.as_array().unwrap().iter().map(|t| t.as_str().unwrap().to_string())),
                        other => panic!("declared option {other} is not replayed"),
                    };
                }
                let job = cw.job("keep", options).expect("declare keep");
                clock.store(int(step, "startedAt"), Ordering::SeqCst);
                let handle = job.start(StartOptions::new().id(text(step, "id"))).await.expect("start");
                clock.store(int(step, "finishedAt"), Ordering::SeqCst);
                handle.finish_with(text(step, "output").to_string()).await;
            }
            other => panic!("unknown step {other}"),
        }
        let got = Value::Object(
            Object::new()
                .with("job", stored_job(&store.get_job("keep").await.unwrap().expect("the job")))
                .with("state", store.get_state("keep").await.unwrap().expect("the state").to_value())
                .with("runs", store.list_runs("keep", 10).await.unwrap().iter().map(Run::to_value).collect::<Vec<_>>())
                .with("alerts", sent.lock().unwrap().drain(..).map(|a| a.to_value()).collect::<Vec<_>>())
                .with("errors", errors.lock().unwrap().drain(..).map(Value::from).collect::<Vec<_>>()),
        );
        let (got, want) = (canonical(&got), canonical(field(step, "expect")));
        if got != want {
            wrong.push(format!("{op}:\n  got  {got}\n  want {want}"));
        }
    }
    let _ = cw.close().await;
    assert!(wrong.is_empty(), "client.json unknownFields: {} steps differ:\n{}", wrong.len(), wrong.join("\n"));
    steps.len()
}

/// The SDK's `channelKeys`: a channel's name, with "#2", "#3", and so on
/// for the later ones of the same name, every key different.
fn channel_keys(names: &[String]) -> Vec<String> {
    let mut seen = std::collections::HashMap::<&str, u64>::new();
    let mut taken = std::collections::HashSet::<String>::new();
    names
        .iter()
        .map(|name| {
            let count = seen.entry(name.as_str()).or_insert(0);
            *count += 1;
            let mut n = *count;
            let mut key = if n == 1 { name.clone() } else { format!("{name}#{n}") };
            while taken.contains(&key) {
                n += 1;
                key = format!("{name}#{n}");
            }
            taken.insert(key.clone());
            key
        })
        .collect()
}

/// A text of the fixture: a string or a `{ parts: [[piece, times], ...] }`
/// recipe, joined.
fn recipe(v: &Value) -> String {
    if let Some(s) = v.as_str() {
        return s.to_string();
    }
    let parts = v.as_object().and_then(|o| o.get("parts")).and_then(Value::as_array).expect("a recipe");
    parts
        .iter()
        .map(|p| {
            let pair = p.as_array().expect("a pair");
            pair[0].as_str().expect("a piece").repeat(pair[1].as_f64().expect("a count") as usize)
        })
        .collect()
}

/// What the outbox replay's channels saw, and what they refuse.
#[derive(Default)]
struct Outbox {
    /// Each send: the channel's position, then what the fixture records.
    sent: Vec<(usize, Object)>,
    down: std::collections::HashSet<String>,
    errors: Vec<String>,
    /// Each run's error as the step failed it, so the replay can read the
    /// length the SDK's would have (see `error_length`).
    failed_with: std::collections::HashMap<String, String>,
}

/// The UTF-16 length of a run's error as the SDK's would be. A Rust run's
/// error is written `Name: message` ("Error: boom" for `fail("boom")`),
/// where the SDK's is the text thrown, so an error sent whole is counted as
/// that text; one trimmed for the outbox is counted as it is (the marker's
/// count changes, its width does not, for the fixture's 5000 units).
fn error_length(error: &str, thrown: Option<&String>) -> usize {
    match thrown {
        Some(t) if error.strip_prefix("Error: ") == Some(t.as_str()) => t.encode_utf16().count(),
        _ => error.encode_utf16().count(),
    }
}

fn outbox_client(store: Arc<MemoryStore>, names: &[String], clock: Arc<AtomicI64>, seen: Arc<Mutex<Outbox>>) -> Client {
    let keys = channel_keys(names);
    let channels: Vec<Arc<dyn cronwatch::Channel>> = names
        .iter()
        .zip(keys)
        .enumerate()
        .map(|(position, (name, key))| {
            let seen = seen.clone();
            channel_fn(name.clone(), move |alert: Alert| {
                let mut s = seen.lock().unwrap();
                let accepted = !s.down.contains(&key);
                let run = alert.run.as_ref();
                let thrown = run.and_then(|r| s.failed_with.get(&r.id));
                let record = Object::new()
                    .with("channel", key.as_str())
                    .with("type", alert.alert_type.as_str())
                    .with("run", run.map_or(Value::Null, |r| Value::from(r.id.as_str())))
                    .with("accepted", accepted)
                    .with(
                        "errorLength",
                        run.and_then(|r| r.error.as_deref()).map_or(Value::Null, |e| error_length(e, thrown).into()),
                    )
                    .with(
                        "outputLength",
                        run.and_then(|r| r.output.as_deref()).map_or(Value::Null, |o| o.encode_utf16().count().into()),
                    );
                s.sent.push((position, record));
                let key = key.clone();
                async move { if accepted { Ok(()) } else { Err(format!("{key} is down").into()) } }
            })
        })
        .collect();
    let (now, kept) = (clock, seen);
    Client::builder()
        .store_arc(store)
        .clock(move || now.load(Ordering::SeqCst))
        .no_cron_secret()
        .alerts(channels)
        .on_error(move |_, where_| kept.lock().unwrap().errors.push(where_.to_string()))
        .build()
        .expect("a client")
}

/// A held alert as the fixture writes it: `{ type, run, owed? }`.
fn held_entry(alert: &Value) -> Value {
    let a = alert.as_object().expect("an alert");
    let run = a.get("run").and_then(Value::as_object).and_then(|r| r.get("id")).cloned().unwrap_or(Value::Null);
    let mut o = Object::new().with("type", a.get("type").cloned().unwrap_or(Value::Null)).with("run", run);
    if let Some(owed) = a.get("owed") {
        o.set("owed", owed.clone());
    }
    Value::Object(o)
}

/// `outbox`: per-channel retry and the trimmed copy held in state, each
/// scenario a real client over a memory store whose channels refuse while
/// their key is down. Answers how many steps ran.
pub async fn replay_outbox() -> usize {
    let f = fixture();
    let mut wrong = Vec::new();
    let mut count = 0;
    for scenario in objects(field(&f, "outbox")) {
        let name = text(scenario, "name");
        let store = Arc::new(MemoryStore::new());
        let clock = Arc::new(AtomicI64::new(T0));
        let seen: Arc<Mutex<Outbox>> = Arc::default();
        let names = |v: &Value| -> Vec<String> {
            v.as_array().expect("names").iter().map(|n| n.as_str().expect("a name").to_string()).collect()
        };
        let mut cw = outbox_client(store.clone(), &names(field(scenario, "channels")), clock.clone(), seen.clone());
        for (i, step) in objects(field(scenario, "steps")).into_iter().enumerate() {
            count += 1;
            let op = text(step, "op");
            match op {
                "fail" | "ok" => {
                    let at = int(step, "at");
                    clock.store(at, Ordering::SeqCst);
                    let job = cw.job("nightly", JobOptions::new()).expect("nightly");
                    let handle = job.start(StartOptions::new().id(text(step, "id"))).await.expect("start");
                    clock.store(at + 1000, Ordering::SeqCst);
                    if op == "ok" {
                        handle.finish_with("done").await;
                    } else {
                        let error = step.get("error").map_or_else(|| "boom".to_string(), recipe);
                        seen.lock().unwrap().failed_with.insert(text(step, "id").to_string(), error.clone());
                        handle.fail(error.as_str()).await;
                    }
                }
                "check" => {
                    clock.store(int(step, "at"), Ordering::SeqCst);
                    cw.check().await.expect("check");
                }
                "down" => seen.lock().unwrap().down.extend(names(field(step, "channels"))),
                "up" => {
                    let up = names(field(step, "channels"));
                    seen.lock().unwrap().down.retain(|k| !up.contains(k));
                }
                "configure" => {
                    let _ = cw.close().await;
                    cw = outbox_client(store.clone(), &names(field(step, "channels")), clock.clone(), seen.clone());
                }
                "legacy" => {
                    let state = store.get_state("nightly").await.unwrap().expect("a state");
                    let Value::Object(mut o) = state.to_value() else { panic!("a state") };
                    if let Some(Value::Array(queue)) = o.get("undelivered").cloned() {
                        let queue: Vec<Value> = queue
                            .into_iter()
                            .map(|a| match a {
                                Value::Object(a) => {
                                    let mut b = Object::new();
                                    for (k, v) in a.iter().filter(|(k, _)| *k != "owed") {
                                        b.set(k, v.clone());
                                    }
                                    Value::Object(b)
                                }
                                other => other,
                            })
                            .collect();
                        o.set("undelivered", queue);
                    }
                    let state = JobState::from_value(&Value::Object(o)).expect("the state");
                    store.set_state(&state).await.expect("set_state");
                }
                other => panic!("unknown outbox op {other}"),
            }
            let state = store.get_state("nightly").await.unwrap().map(|s| s.to_value());
            let state = state.as_ref().and_then(Value::as_object);
            let list = |key: &str, alert_of: &dyn Fn(&Value) -> Option<Value>| -> Value {
                let entries = state.and_then(|s| s.get(key)).and_then(Value::as_array).cloned().unwrap_or_default();
                Value::Array(entries.iter().filter_map(alert_of).map(|a| held_entry(&a)).collect())
            };
            let mut s = seen.lock().unwrap();
            let mut sent = std::mem::take(&mut s.sent);
            sent.sort_by_key(|(position, _)| *position);
            let got = Value::Object(
                Object::new()
                    .with("sent", sent.into_iter().map(|(_, r)| Value::Object(r)).collect::<Vec<_>>())
                    .with("undelivered", list("undelivered", &|a| Some(a.clone())))
                    .with("sending", list("sending", &|e| e.as_object().and_then(|e| e.get("alert")).cloned()))
                    .with("lastAlertAt", state.and_then(|s| s.get("lastAlertAt")).cloned().unwrap_or(Value::Null))
                    .with("errors", std::mem::take(&mut s.errors).into_iter().map(Value::from).collect::<Vec<_>>()),
            );
            let (got, want) = (canonical(&got), canonical(field(step, "expect")));
            if got != want {
                wrong.push(format!("{name}, step {i} ({op}):\n  got  {got}\n  want {want}"));
            }
        }
        let _ = cw.close().await;
    }
    assert!(wrong.is_empty(), "client.json outbox: {} steps differ:\n{}", wrong.len(), wrong.join("\n"));
    count
}
