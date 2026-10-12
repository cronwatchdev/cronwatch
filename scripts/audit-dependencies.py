#!/usr/bin/env python3
"""Scan resolved runtime and test dependencies against OSV, without third-party libraries."""
import argparse
import collections
import concurrent.futures
import datetime
import json
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
        for package in tomllib.loads(source.read_text())["package"]:
            if "registry" in package.get("source", {}):
                add(package["name"], package.get("version"), source)
    elif ecosystem == "RubyGems":
        sources = [ROOT / "packages/ruby/Gemfile.lock"] + [
            ROOT / f"packages/ruby/test/rails/gemfiles/rails_{series}.gemfile.lock"
            for series in ("7_2", "8_0", "8_1")
        ]
        for source in sources:
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
        for source in (ROOT / "packages/rust/Cargo.lock", ROOT / "packages/rust/fuzz/Cargo.lock"):
            for package in tomllib.loads(source.read_text())["package"]:
                if package.get("source", "").startswith("registry+"):
                    add(package["name"], package["version"], source)
    elif ecosystem == "Hex":
        for source in (ROOT / "packages/elixir/mix.lock", ROOT / "packages/elixir/webserver/mix.lock", ROOT / "packages/elixir/examples/crontab/mix.lock"):
            for name, version in re.findall(r'"([^"\n]+)": \{:hex, :[^,]+, "([^"\n]+)"', source.read_text()):
                add(name, version, source)
    elif ecosystem == "Packagist":
        source = ROOT / "packages/php/composer.lock"
        lock = json.loads(source.read_text())
        for package in lock.get("packages", []) + lock.get("packages-dev", []):
            add(package["name"], package["version"].removeprefix("v"), source)
    elif ecosystem == "Maven":
        sources = sorted((ROOT / "packages/java").glob("*/target/security-dependencies.json"))
        if not sources:
            raise ValueError("resolve Maven dependency trees first (see security/README.md)")
        def visit(node, source):
            if not node["groupId"].startswith("dev.cronwatch"):
                add(node["groupId"] + ":" + node["artifactId"], node["version"], source)
            for child in node.get("children", []):
                visit(child, source)
        for source in sources:
            visit(json.loads(source.read_text()), source)
    elif ecosystem == "NuGet":
        sources = sorted((ROOT / "packages/dotnet").glob("**/obj/project.assets.json"))
        if not sources:
            raise ValueError("run dotnet restore first")
        for source in sources:
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
            return list(zip(packages, rows))
        except (urllib.error.URLError, TimeoutError):
            if attempt == 2:
                raise
            time.sleep(2 ** attempt)


def scan(entries, exceptions, today):
    for exception in exceptions:
        if datetime.date.fromisoformat(exception["expires"]) <= today:
            raise ValueError(f"expired advisory exception: {exception['name']}")
        if not exception.get("reason"):
            raise ValueError("advisory exception needs a reason")
    for exception in exceptions:
        key = (exception["ecosystem"], exception["name"], exception["version"])
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
