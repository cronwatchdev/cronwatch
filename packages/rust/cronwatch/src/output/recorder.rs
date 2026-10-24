use std::collections::VecDeque;
use std::sync::Mutex;

use super::{OUTPUT_CAP, REDACT_EDGE, strip_nul};
use crate::js;

/// How much logged text a recorder holds, in code units, before it drops
/// lines from the front. `redact_and_cap` trims exactly at the end, so this only
/// bounds memory: well past the cap, so the kept tail is whole.
const WINDOW: usize = OUTPUT_CAP + REDACT_EDGE + 2;

/// `createRecorder` in `job.ts`: the lines a run logs and the numbers it
/// reports. It is safe to share, since a job may log from tasks of its own.
#[derive(Debug, Default)]
pub(crate) struct Recorder {
    inner: Mutex<Inner>,
}

#[derive(Debug, Default)]
struct Inner {
    /// The lines kept and their lengths in code units; the front is dropped
    /// once the total passes `WINDOW`.
    lines: VecDeque<(String, usize)>,
    size: usize,
    /// The first lines logged, until they reach `OUTPUT_CAP`, kept for
    /// expect even after the window has dropped them.
    head: Vec<String>,
    head_size: usize,
    whole: String,
    /// `whole`'s length in code units, so a line is not counted twice.
    whole_size: usize,
    whole_lost: bool,
    /// The lines behind expect's tail, each cut to `OUTPUT_CAP`: the front
    /// is dropped while what is left still holds `OUTPUT_CAP`, and they are
    /// joined only when the tail is read, so a line logged copies no more
    /// than itself.
    raw: VecDeque<(String, usize)>,
    raw_size: usize,
    logged: bool,
    metrics: js::Object,
}

impl Recorder {
    /// An empty recorder.
    pub(crate) fn new() -> Recorder {
        Recorder::default()
    }

    fn lock(&self) -> std::sync::MutexGuard<'_, Inner> {
        self.inner.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// Appends a line.
    pub(crate) fn log(&self, line: impl Into<String>) {
        let mut line = line.into();
        let mut n = js::len16(&line);
        let mut r = self.lock();
        let separator = if r.logged { "\n" } else { "" };
        r.logged = true;
        if !r.whole_lost {
            if r.whole_size + separator.len() + n <= 2 * OUTPUT_CAP {
                r.whole.push_str(separator);
                r.whole.push_str(&line);
                r.whole_size += separator.len() + n;
            } else {
                r.whole = String::new();
                r.whole_lost = true;
            }
        }
        let cut = tail(&line, OUTPUT_CAP);
        let cut_size = js::len16(&cut);
        r.raw.push_back((cut, cut_size));
        r.raw_size += cut_size + 1;
        while r.raw.len() > 1 && r.raw_size - (r.raw[0].1 + 1) > OUTPUT_CAP {
            let (_, len) = r.raw.pop_front().expect("more than one line");
            r.raw_size -= len + 1;
        }
        if r.head_size < OUTPUT_CAP {
            let room = OUTPUT_CAP - r.head_size;
            r.head.push(js::head16(&line, room));
            r.head_size += n + 1;
        }
        line = strip_nul(&line);
        n = js::len16(&line);
        if n >= WINDOW {
            line = js::tail16(&line, WINDOW - 1);
            n = js::len16(&line);
        }
        r.lines.push_back((line, n));
        r.size += n + 1;
        // Drop from the front once well past the cap; redact_and_cap trims
        // exactly at the end.
        while r.size > WINDOW && r.lines.len() > 1 {
            let (_, len) = r.lines.pop_front().expect("more than one line");
            r.size -= len + 1;
        }
    }

    /// The lines still held (past 64 KB the oldest are let go), joined and
    /// not yet capped: the client redacts them first, then caps them
    /// (`redact_and_cap`). `None` when nothing was logged.
    pub(crate) fn output(&self) -> Option<String> {
        let r = self.lock();
        if r.lines.is_empty() {
            return None;
        }
        Some(join(&r.lines))
    }

    /// What an expect rule is checked against: everything logged, or when
    /// that ran long, the first 16 KB and the last 16 KB. The stored output
    /// keeps only the tail, so a "done" line printed early would otherwise
    /// be lost. `None` when nothing was logged.
    pub(crate) fn expect_text(&self) -> Option<String> {
        let r = self.lock();
        if r.lines.is_empty() {
            return None;
        }
        if !r.whole_lost {
            return Some(r.whole.clone());
        }
        Some(format!("{}\n{}", js::head16(&r.head.join("\n"), OUTPUT_CAP), tail(&join(&r.raw), OUTPUT_CAP)))
    }

    /// Reports a number for the run; a later value for the same name
    /// replaces an earlier one.
    pub(crate) fn metric(&self, name: &str, value: f64) -> Result<(), String> {
        if !value.is_finite() {
            return Err(format!("metric \"{name}\" must be a finite number"));
        }
        self.lock().metrics.set(name, value);
        Ok(())
    }

    /// A copy of the numbers reported, in JavaScript's key order.
    pub(crate) fn metrics(&self) -> js::Object {
        self.lock().metrics.clone()
    }
}

fn join(lines: &VecDeque<(String, usize)>) -> String {
    let mut out = String::new();
    for (i, (line, _)) in lines.iter().enumerate() {
        if i > 0 {
            out.push('\n');
        }
        out.push_str(line);
    }
    out
}

fn tail(s: &str, n: usize) -> String {
    if js::len16(s) <= n { s.to_owned() } else { js::tail16(s, n) }
}
