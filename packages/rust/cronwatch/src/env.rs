//! The environment, read in one place.

/// The variables that name the environment, first set wins. The SDK reads
/// `NODE_ENV`; Rust has no one convention, so CronWatch's own variable comes
/// first, then `APP_ENV` (as the PHP and Go ports read it) and `RUST_ENV`
/// (the Go port's `GO_ENV`).
const VARIABLES: [&str; 3] = ["CRONWATCH_ENV", "APP_ENV", "RUST_ENV"];

/// The environment's name, lowercased, or `""` when no variable names one.
/// `dev` and `local` count as `development` and `prod` as `production`, as
/// in every port. `test` and `testing` are not development: a test run or
/// CI needs `CRON_SECRET` or a token, as production does. A debug build is
/// not development either: `cfg!(debug_assertions)` says how the code was
/// compiled, not where it runs.
pub(crate) fn environment() -> String {
    environment_from(|name| std::env::var(name).ok())
}

/// [`environment`] over variables `read` gives. The first whose value,
/// trimmed as `String.prototype.trim` trims, is not empty wins: a value of
/// only spaces counts as unset, and the next variable is read, as in every
/// port.
fn environment_from(read: impl Fn(&str) -> Option<String>) -> String {
    for name in VARIABLES {
        let value = crate::js::trim(&read(name).unwrap_or_default()).to_lowercase();
        if value.is_empty() {
            continue;
        }
        return match value.as_str() {
            "prod" => "production".into(),
            "dev" | "local" => "development".into(),
            _ => value,
        };
    }
    String::new()
}

/// A token or secret, trimmed as `String.prototype.trim` trims (not U+0085,
/// and U+FEFF included, which Rust's own `trim` has the other way round): a
/// value read from a file often ends in a newline that no header can carry,
/// since HTTP strips the whitespace around a header's value. `None` when
/// that leaves nothing, so a blank one counts as unset.
pub(crate) fn secret(value: &str) -> Option<String> {
    let trimmed = crate::js::trim(value);
    (!trimmed.is_empty()).then(|| trimmed.to_string())
}

/// A secret from the environment (`CRONWATCH_TOKEN`, `CRON_SECRET`), trimmed
/// (see [`secret`]): `None` when the variable is unset, empty, or only
/// whitespace, so a blank value counts as not set and the routes and
/// handlers fail closed.
pub(crate) fn secret_var(name: &str) -> Option<String> {
    std::env::var(name).ok().and_then(|v| secret(&v))
}

/// Claude triage's API key and base URL, from the variables the official
/// Anthropic client reads: `ANTHROPIC_API_KEY` and `ANTHROPIC_BASE_URL`,
/// `""` when unset.
#[cfg(feature = "triage")]
pub(crate) fn anthropic() -> (String, String) {
    let read = |name: &str| std::env::var(name).unwrap_or_default();
    (read("ANTHROPIC_API_KEY"), read("ANTHROPIC_BASE_URL"))
}

#[cfg(test)]
mod tests {
    use super::{environment_from, secret};
    use crate::conformance::{field, fixture, objects};
    use crate::js::Value;

    /// `client.json`'s `environment` and `secrets`, with `RUST_ENV` as the
    /// language's own variable.
    #[test]
    fn client_json_environment_and_secrets() {
        let f = fixture("client");
        // Every section is replayed: runIds, unknownFields, and outbox by
        // tests/client_fixture, these two here.
        let known = ["generatedBy", "sdkVersion", "runIds", "unknownFields", "outbox", "environment", "secrets"];
        for key in f.keys() {
            assert!(known.contains(&key), "client.json has {key}, not replayed");
        }
        assert!(!objects(&f, "environment").is_empty() && !objects(&f, "secrets").is_empty());
        for c in objects(&f, "environment") {
            let read = |name: &str| {
                let key = match name {
                    "CRONWATCH_ENV" => "CRONWATCH_ENV",
                    "APP_ENV" => "APP_ENV",
                    "RUST_ENV" => "native",
                    _ => return None,
                };
                field(c, key).as_str().map(str::to_string)
            };
            let got = environment_from(read);
            let want = field(c, "environment").as_str().expect("an environment");
            assert_eq!(got, want, "{}", Value::Object(c.clone()).to_json());
            assert_eq!(got == "development", matches!(field(c, "development"), Value::Bool(true)));
            assert_eq!(got == "production", matches!(field(c, "production"), Value::Bool(true)));
        }
        for c in objects(&f, "secrets") {
            let value = field(c, "value").as_str().expect("a value");
            assert_eq!(secret(value).as_deref(), field(c, "secret").as_str(), "{value:?}");
        }
    }

    /// Blank as JavaScript's `trim` sees it (the second review, Medium 1).
    #[test]
    fn a_secret_of_only_whitespace_is_blank() {
        for blank in ["", " ", "  ", "\t", " \n  \u{feff} ", "\u{a0}", "\u{2003}", "\u{feff}", "\u{3000}\u{2028}"] {
            assert_eq!(secret(blank), None, "{blank:?}");
        }
        for set in ["x", " padded ", "\u{85}", "\u{180e}", "\u{1c}"] {
            assert!(secret(set).is_some(), "{set:?}");
        }
    }

    /// The SDK's cases (packages/sdk/test/env.test.ts, plan D8), with
    /// `RUST_ENV` in `NODE_ENV`'s place.
    #[test]
    fn the_environment_is_read_as_every_port_reads_it() {
        let cases: [(&str, &str, &str, &str); 12] = [
            ("", "", "", ""),
            ("", "", "development", "development"),
            ("", "", "test", "test"),
            ("", "", "production", "production"),
            ("", "local", "production", "development"),
            ("production", "dev", "development", "production"),
            ("staging", "", "development", "staging"),
            ("  PROD ", "", "", "production"),
            ("", "Testing", "", "testing"),
            ("", "DEV", "", "development"),
            ("", "   ", "production", "production"),
            (" \t", "", "", ""),
        ];
        for (cronwatch, app, own, want) in cases {
            let got = environment_from(|name| {
                let v = match name {
                    "CRONWATCH_ENV" => cronwatch,
                    "APP_ENV" => app,
                    "RUST_ENV" => own,
                    _ => "",
                };
                (!v.is_empty() || name == "CRONWATCH_ENV").then(|| v.to_string())
            });
            assert_eq!(got, want, "CRONWATCH_ENV={cronwatch:?} APP_ENV={app:?} RUST_ENV={own:?}");
        }
    }

    /// Each value is trimmed as `String.prototype.trim` trims it: U+FEFF
    /// alone counts as unset, and U+0085, which JavaScript keeps, does not.
    #[test]
    fn the_environment_is_trimmed_as_javascript_trims() {
        let read = |first: &'static str| {
            move |name: &str| match name {
                "CRONWATCH_ENV" => Some(first.to_string()),
                "APP_ENV" => Some("production".to_string()),
                _ => None,
            }
        };
        assert_eq!(environment_from(read("\u{feff}")), "production");
        assert_eq!(environment_from(read("\u{feff}dev\u{3000}")), "development");
        assert_eq!(environment_from(read("\u{85}")), "\u{85}");
    }
}
