#!/usr/bin/env python3
"""Scan resolved runtime and test dependencies against OSV, without third-party libraries."""
import argparse
import collections
import concurrent.futures
import datetime
import json
import os
import pathlib
import re
import subprocess
import sys
import time
import tomllib
import urllib.error
import urllib.request

ROOT = pathlib.Path(__file__).resolve().parent.parent
ECOSYSTEMS = ("npm", "PyPI", "RubyGems", "Go", "crates.io", "Hex", "Packagist", "Maven", "NuGet")


def require_fresh(source, manifests, resolve):
    """Fail when a resolved inventory is older than a manifest it was resolved from.

    These inventories are not committed, so a scan of one left from an earlier
    resolution says nothing about what the manifests resolve to now. `resolve`
    is the command that writes it again. A fresh checkout resolves after it
    writes the manifests, so CI passes.
    """
    resolved = os.stat(source).st_mtime_ns
    for manifest in manifests:
        if manifest.exists() and os.stat(manifest).st_mtime_ns > resolved:
            raise ValueError(
                f"{source.relative_to(ROOT)} is older than {manifest.relative_to(ROOT)}, so it may not be what the manifest resolves to now; "
                f"resolve it again with `{resolve}` (if that leaves the file as it is, delete it first)"
            )


def inventory(ecosystem):
    entries = collections.defaultdict(set)

    def add(name, version, source):
        if name and version:
            entries[(ecosystem, name, version)].add(str(source.relative_to(ROOT)))

    if ecosystem == "npm":
        source = ROOT / "package-lock.json"
        for location, package in json.loads(source.read_text())["packages"].items():
            if "node_modules/" in location and not package.get("link"):
                add(package.get("name") or location.rsplit("node_modules/", 1)[1], package.get("version"), source)
    elif ecosystem == "PyPI":
        source = ROOT / "packages/python/uv.lock"
        require_fresh(source, [source.with_name("pyproject.toml")], "cd packages/python && uv lock")
        for package in tomllib.loads(source.read_text())["package"]:
            if "registry" in package.get("source", {}):
                add(package["name"], package.get("version"), source)
    elif ecosystem == "RubyGems":
        sources = [ROOT / "packages/ruby/Gemfile.lock"] + [
            ROOT / f"packages/ruby/test/rails/gemfiles/rails_{series}.gemfile.lock"
            for series in ("7_2", "8_0", "8_1")
        ]
        gemfile = ROOT / "packages/ruby/Gemfile"
        for source in sources:
            own = source.with_name(source.name.removesuffix(".lock"))
            require_fresh(
                source, [own, gemfile, ROOT / "packages/ruby/cronwatch.gemspec"],
                f"cd packages/ruby && BUNDLE_GEMFILE={own.relative_to(ROOT / 'packages/ruby')} bundle lock",
            )
            for name, version in re.findall(r"^    ([\w.-]+) \(([^ )]+)\)", source.read_text(), re.M):
                add(name, re.sub(r"-(?:aarch64|arm64|x86_64|arm)-.*$", "", version), source)
    elif ecosystem == "Go":
        for source in sorted((ROOT / "packages/go").rglob("go.mod")):
            # go.sum also holds obsolete downloads. Scan the selected module graph instead.
            output = subprocess.check_output(["go", "list", "-m", "-json", "all"], cwd=source.parent, text=True)
            decoder = json.JSONDecoder()
            while output.strip():
                package, end = decoder.raw_decode(output.lstrip())
                output = output.lstrip()[end:]
                package = package.get("Replace", package)
                if package.get("Version"):
                    add(package["Path"], package["Version"], source)
        version = subprocess.check_output(["go", "env", "GOVERSION"], text=True).strip()
        add("stdlib", version.removeprefix("go"), ROOT / "packages/go/go.mod")
    elif ecosystem == "crates.io":
        # The workspace's lock is committed, and CI builds with --locked; the fuzz crate's is not.
        fuzz = ROOT / "packages/rust/fuzz/Cargo.lock"
        require_fresh(
            fuzz, [fuzz.with_name("Cargo.toml"), *sorted((ROOT / "packages/rust").glob("*/Cargo.toml"))],
            "cargo generate-lockfile --manifest-path packages/rust/fuzz/Cargo.toml",
        )
        for source in (ROOT / "packages/rust/Cargo.lock", fuzz):
            for package in tomllib.loads(source.read_text())["package"]:
                if package.get("source", "").startswith("registry+"):
                    add(package["name"], package["version"], source)
    elif ecosystem == "Hex":
        for source in (ROOT / "packages/elixir/mix.lock", ROOT / "packages/elixir/webserver/mix.lock", ROOT / "packages/elixir/examples/crontab/mix.lock"):
            require_fresh(source, [source.with_name("mix.exs")], f"cd {source.parent.relative_to(ROOT)} && MIX_ENV=test mix deps.get")
            for name, version in re.findall(r'"([^"\n]+)": \{:hex, :[^,]+, "([^"\n]+)"', source.read_text()):
                add(name, version, source)
    elif ecosystem == "Packagist":
        source = ROOT / "packages/php/composer.lock"
        require_fresh(source, [source.with_name("composer.json")], "cd packages/php && composer update --no-interaction")
        lock = json.loads(source.read_text())
        for package in lock.get("packages", []) + lock.get("packages-dev", []):
            add(package["name"], package["version"].removeprefix("v"), source)
    elif ecosystem == "Maven":
        # Every module's tree, however deep the module is (examples/crontab), and the parent's.
        java = ROOT / "packages/java"
        poms = [pom for pom in sorted(java.rglob("pom.xml")) if "target" not in pom.relative_to(java).parts]
        missing = [pom for pom in poms if not (pom.parent / "target/security-dependencies.json").exists()]
        if missing:
            raise ValueError(f"no dependency tree for {missing[0].relative_to(ROOT)}: resolve Maven dependency trees first (see security/README.md)")
        sources = [pom.parent / "target/security-dependencies.json" for pom in poms]
        for source in sources:
            require_fresh(source, poms, "the Maven command in security/README.md")
        def visit(node, source):
            if not node["groupId"].startswith("dev.cronwatch"):
                add(node["groupId"] + ":" + node["artifactId"], node["version"], source)
            for child in node.get("children", []):
                visit(child, source)
        for source in sources:
            visit(json.loads(source.read_text()), source)
    elif ecosystem == "NuGet":
        dotnet = ROOT / "packages/dotnet"
        sources = sorted(dotnet.glob("**/obj/project.assets.json"))
        if not sources:
            raise ValueError("run dotnet restore first")
        shared = [dotnet / name for name in ("Directory.Packages.props", "Directory.Build.props", "global.json")]
        for source in sources:
            require_fresh(source, [*sorted(source.parent.parent.glob("*.csproj")), *shared], "cd packages/dotnet && dotnet restore --force")
            for name, package in json.loads(source.read_text()).get("libraries", {}).items():
                if package.get("type") == "package":
                    add(*name.rsplit("/", 1), source)
    if not entries:
        raise ValueError(f"no resolved dependencies for {ecosystem}")
    return entries


def query(packages):
    body = json.dumps({"queries": [{"package": {"ecosystem": e, "name": n}, "version": v} for e, n, v in packages]}).encode()
    for attempt in range(3):
        request = urllib.request.Request("https://api.osv.dev/v1/querybatch", data=body, headers={"Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(request, timeout=45) as response:
                rows = json.load(response)["results"]
            if len(rows) != len(packages):
                raise ValueError("OSV returned an incomplete response")
            # A row cut short names where its rest is; reading only the first page would pass what the rest holds.
            cut = [f"{name}@{version}" for (_, name, version), row in zip(packages, rows) if row.get("next_page_token")]
            if cut:
                raise ValueError(f"OSV returned only part of the advisories for {', '.join(cut)}; scan fewer packages at once")
            return list(zip(packages, rows))
        except (urllib.error.URLError, TimeoutError):
            if attempt == 2:
                raise
            time.sleep(2 ** attempt)


def scan(entries, exceptions, today):
    # Only the exceptions of the ecosystems being scanned are weighed: an
    # expired one in another ecosystem fails that ecosystem's scan, not this one.
    scanned = {ecosystem for ecosystem, _, _ in entries}
    exceptions = [exception for exception in exceptions if exception["ecosystem"] in scanned]
    for exception in exceptions:
        if datetime.date.fromisoformat(exception["expires"]) <= today:
            raise ValueError(f"expired advisory exception: {exception['name']}")
        if not exception.get("reason"):
            raise ValueError("advisory exception needs a reason")
    for exception in exceptions:
        key = (exception["ecosystem"], exception["name"], exception["version"])
        if key in entries:
            for required in exception.get("required_dependencies", []):
                minimum = tuple(map(int, required["minimum_version"].split(".")))
                for source in set(exception["sources"]) & entries[key]:
                    versions = [
                        version for (ecosystem, name, version), sources in entries.items()
                        if ecosystem == required["ecosystem"] and name == required["name"] and source in sources
                    ]
                    if not versions or any(
                        not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", version)
                        or tuple(map(int, version.split("."))) < minimum for version in versions
                    ):
                        raise ValueError(f"advisory exception requires {required['name']} >= {required['minimum_version']} in {source}")
        if key in entries and exception.get("absent_import_prefixes"):
            for source in exception["sources"]:
                imports = subprocess.check_output(
                    ["go", "list", "-deps", "-test", "-f", "{{.ImportPath}}", "./..."],
                    cwd=(ROOT / source).parent, text=True,
                ).splitlines()
                if any(p == prefix or p.startswith(prefix + "/") for p in imports for prefix in exception["absent_import_prefixes"]):
                    raise ValueError(f"advisory exception imports the vulnerable package: {exception['name']}")
    packages = sorted(entries)
    batches = [packages[i:i + 100] for i in range(0, len(packages), 100)]
    findings = []
    with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
        for batch in pool.map(query, batches):
            for (ecosystem, name, version), row in batch:
                for vuln in row.get("vulns", []):
                    ident = vuln["id"]
                    for source in sorted(entries[(ecosystem, name, version)]):
                        allowed = any(
                            x["ecosystem"] == ecosystem and x["name"] == name and x["version"] == version
                            and source in x["sources"] and ident in x["advisories"]
                            for x in exceptions
                        )
                        print(f"{'EXCEPTION' if allowed else 'VULNERABLE'} {ecosystem} {name}@{version}: {ident} ({source})", flush=True)
                        if not allowed:
                            findings.append((ecosystem, name, version, ident, source))
    print(f"Scanned {len(packages)} package versions; {len(findings)} unexcepted findings", flush=True)
    return 1 if findings else 0


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--ecosystem", choices=ECOSYSTEMS, action="append")
    args = parser.parse_args()
    entries = {}
    for ecosystem in args.ecosystem or ECOSYSTEMS:
        for package, sources in inventory(ecosystem).items():
            entries.setdefault(package, set()).update(sources)
    exceptions = json.loads((ROOT / "security/advisory-exceptions.json").read_text())
    return scan(entries, exceptions, datetime.datetime.now(datetime.timezone.utc).date())


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (OSError, ValueError, KeyError, subprocess.CalledProcessError) as error:
        print(f"Dependency audit failed: {error}", file=sys.stderr)
        sys.exit(2)
