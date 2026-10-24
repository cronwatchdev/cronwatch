# Dependency advisories

`advisories.yml` scans every ecosystem through the main CI workflow on code changes, and weekly. Its failure fails CI, which gates automatic deployment; site-only changes keep their existing fast checks. It queries OSV for resolved runtime, optional, and test dependencies. A missing inventory, an unavailable advisory service, an expired exception, or an unexcepted finding fails the job. The normal framework matrix still tests compatibility with older supported frameworks; the advisory job scans the current default test stack.

Run `python3 scripts/audit-dependencies.py` with Python 3.11 or newer after resolving the dependencies. `--ecosystem npm` (repeatable) scans one ecosystem. The script reads npm, uv, Cargo, Composer, Hex and Ruby lockfiles, the selected Go module graphs (not obsolete downloads in go.sum), and .NET's restored project.assets.json files. Ruby's three Rails bundles and Elixir's example and webserver each need their own lock resolved, as in the workflow. The Go installation used for the scan is checked as `stdlib`, too.

For Maven, generate all dependency trees, including tests, with:

```sh
cd packages/java
./mvnw -B -Dmaven.test.skip=true install org.apache.maven.plugins:maven-dependency-plugin:3.8.1:tree -DoutputType=json -DoutputFile=target/security-dependencies.json
```

Exceptions live in `advisory-exceptions.json`, scoped to an exact package version, advisory IDs, and inventory paths, with a reason and expiry. One current exception is SQLx's test-only RSA public-key authentication dependency, for which no patched upstream version exists. It does not perform the private-key operation affected by the Marvin attack. Another covers the unused OpenPGP subpackages of x/crypto in the unpublished Go examples: the scanner verifies that no test or example imports those packages. Those two have no upstream patch and expire on 2027-01-10. Two more hold ctutils 0.4.2 and crypto-bigint 0.7.5 in `packages/rust/Cargo.lock`: their fixes need Rust 1.87, above the core's 1.85 floor, and the lockfile is not published, so an app on 1.87 or newer resolves the fixed releases. They are removed when the floor reaches 1.87. See [RUSTSEC-2023-0071](https://rustsec.org/advisories/RUSTSEC-2023-0071.html).

The Elixir Phoenix test fixture uses Cowboy while Plug 1.20.3 and Phoenix 1.8.15 discard the updated connection when rendering JSON parse errors, which Bandit 1.13 correctly rejects. The plain router and handler tests continue on patched Bandit. Cowlib 2.21.0 has a narrow test-inventory exception until 2027-01-10 for [EEF-CVE-2026-43966](https://cna.erlef.org/cves/CVE-2026-43966.html). Its advisory identifies Cowboy 2.16.0 or newer as protected by the default rejection of unsafe response headers. The test dependency sets that floor, leaves the protection enabled, and the scanner requires a protected Cowboy version in the same inventory before accepting the exception. Neither server is a published runtime dependency.

The Rust TLS hardening test's committed certificate and key in `packages/rust/cronwatch/src/alerts/testdata` are public fixtures for a local server no client trusts. They are never production credentials. This avoids the vulnerable `time` dependency pulled in by certificate generation while preserving Rust 1.85 support. Normal Rust CI uses `--locked`; the release script requires Cargo and refreshes workspace entries in Cargo.lock before checking a release.

Applications choose their own optional framework dependencies. Current patched baselines from this audit are Rails 7.2.3.1, Decimal 3.1.2 (through a compatible Ecto release), Tomcat 11.0.25 and Jackson 3.1.7. The unpublished Go examples now need Go 1.26 because their patched transitive tooling needs it; the core module still supports Go 1.25. Local verification uses Go 1.26.9 without changing the machine's default installation.
