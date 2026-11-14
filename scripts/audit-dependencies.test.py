"""Regression tests for advisory exception boundaries and failed inventory collection."""
import contextlib
import datetime
import importlib.util
import io
import json
import os
import tempfile
import unittest
import sys
from unittest.mock import patch
from pathlib import Path

sys.dont_write_bytecode = True

spec = importlib.util.spec_from_file_location("audit", Path(__file__).with_name("audit-dependencies.py"))
audit = importlib.util.module_from_spec(spec)
spec.loader.exec_module(audit)


class Exceptions(unittest.TestCase):
    def setUp(self):
        self.key = ("crates.io", "rsa", "0.10.0-rc.18")
        self.source = "packages/rust/Cargo.lock"
        self.exception = {
            "ecosystem": "crates.io", "name": "rsa", "version": "0.10.0-rc.18",
            "sources": [self.source], "advisories": ["RUSTSEC-2023-0071"],
            "reason": "Public-key-only test authentication", "expires": "2027-01-10",
        }
        self.today = datetime.date(2026, 10, 12)

    def run_scan(self, key=None, source=None, ident="RUSTSEC-2023-0071"):
        key = key or self.key
        with patch.object(audit, "query", return_value=[(key, {"vulns": [{"id": ident}]})]), contextlib.redirect_stdout(io.StringIO()):
            return audit.scan({key: {source or self.source}}, [self.exception], self.today)

    def test_only_the_exact_exception_is_accepted(self):
        self.assertEqual(0, self.run_scan())
        self.assertEqual(1, self.run_scan(source="packages/rust/fuzz/Cargo.lock"))
        self.assertEqual(1, self.run_scan(key=("crates.io", "rsa", "0.10.0-rc.19")))
        self.assertEqual(1, self.run_scan(ident="NEW-ADVISORY"))
        self.assertEqual(1, self.run_scan(key=("npm", "rsa", "0.10.0-rc.18")))

    def test_unimported_package_exception_is_checked(self):
        self.exception["absent_import_prefixes"] = ["golang.org/x/crypto/openpgp"]
        with patch.object(audit.subprocess, "check_output", return_value="golang.org/x/crypto/openpgp/packet\n"):
            with self.assertRaisesRegex(ValueError, "imports the vulnerable"):
                self.run_scan()
        with patch.object(audit.subprocess, "check_output", return_value="golang.org/x/crypto/ssh\n"):
            self.assertEqual(0, self.run_scan())

    def test_mitigation_dependency_must_be_patched_and_in_the_same_inventory(self):
        self.exception["required_dependencies"] = [{"ecosystem": "Hex", "name": "cowboy", "minimum_version": "2.16.0"}]
        with self.assertRaisesRegex(ValueError, "requires cowboy"):
            self.run_scan()
        for version, source, accepted in [
            ("2.15.0", self.source, False),
            ("2.16.0-rc.1", self.source, False),
            ("2.20.0", "another/mix.lock", False),
            ("2.16.0", self.source, True),
            ("2.20.0", self.source, True),
        ]:
            entries = {self.key: {self.source}, ("Hex", "cowboy", version): {source}}
            with patch.object(audit, "query", return_value=[]), contextlib.redirect_stdout(io.StringIO()):
                if accepted:
                    self.assertEqual(0, audit.scan(entries, [self.exception], self.today))
                else:
                    with self.assertRaisesRegex(ValueError, "requires cowboy"):
                        audit.scan(entries, [self.exception], self.today)

    def test_expiry_cannot_silently_ignore_findings(self):
        with self.assertRaisesRegex(ValueError, "expired"):
            audit.scan({self.key: {self.source}}, [self.exception], datetime.date(2027, 1, 10))

    def test_service_failure_is_an_error(self):
        with patch.object(audit, "query", side_effect=TimeoutError("unavailable")):
            with self.assertRaises(TimeoutError):
                audit.scan({self.key: {self.source}}, [], self.today)

    def test_missing_resolved_inventory_is_an_error(self):
        with patch.object(audit, "ROOT", Path("/does-not-exist")):
            with self.assertRaises(ValueError):
                audit.inventory("Maven")

    def test_expiry_is_weighed_only_for_the_ecosystems_scanned(self):
        expired = datetime.date(2027, 1, 10)
        npm = ("npm", "left-pad", "1.0.0")
        with patch.object(audit, "query", return_value=[]), contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(0, audit.scan({npm: {"package-lock.json"}}, [self.exception], expired))
            with self.assertRaisesRegex(ValueError, "expired"):
                audit.scan({npm: {"package-lock.json"}, self.key: {self.source}}, [self.exception], expired)
        # Nor does one without a reason in another ecosystem stop this one.
        del self.exception["reason"]
        with patch.object(audit, "query", return_value=[]), contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(0, audit.scan({npm: {"package-lock.json"}}, [self.exception], self.today))
            with self.assertRaisesRegex(ValueError, "needs a reason"):
                audit.scan({self.key: {self.source}}, [self.exception], self.today)

    def test_a_paginated_answer_is_an_error(self):
        class Answer(io.BytesIO):
            def __enter__(self):
                return self

            def __exit__(self, *args):
                return False

        def answering(rows):
            return patch.object(audit.urllib.request, "urlopen", side_effect=lambda *a, **k: Answer(json.dumps({"results": rows}).encode()))

        packages = [("Go", "stdlib", "1.20.1"), ("npm", "left-pad", "1.0.0")]
        with answering([{"vulns": [{"id": "GO-1"}], "next_page_token": "more"}, {}]):
            with self.assertRaisesRegex(ValueError, r"only part of the advisories for stdlib@1\.20\.1"):
                audit.query(packages)
        with answering([{"vulns": [{"id": "GO-1"}]}, {}]):
            self.assertEqual([(packages[0], {"vulns": [{"id": "GO-1"}]}), (packages[1], {})], audit.query(packages))


class Freshness(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)
        self.root = Path(self.dir.name).resolve()
        patcher = patch.object(audit, "ROOT", self.root)
        patcher.start()
        self.addCleanup(patcher.stop)

    def write(self, name, text, at):
        path = self.root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text)
        os.utime(path, (at, at))
        return path

    def test_an_inventory_older_than_its_manifest_is_an_error(self):
        lock = self.write("packages/php/composer.lock", json.dumps({"packages": [{"name": "a/b", "version": "v1.2.3"}]}), 2000)
        manifest = self.write("packages/php/composer.json", "{}", 1000)
        self.assertEqual({("Packagist", "a/b", "1.2.3")}, set(audit.inventory("Packagist")))
        os.utime(manifest, (2000, 2000))
        self.assertEqual({("Packagist", "a/b", "1.2.3")}, set(audit.inventory("Packagist")))
        os.utime(manifest, (3000, 3000))
        with self.assertRaisesRegex(ValueError, r"packages/php/composer\.lock is older than packages/php/composer\.json.*composer update"):
            audit.inventory("Packagist")
        os.utime(lock, (4000, 4000))
        self.assertEqual({("Packagist", "a/b", "1.2.3")}, set(audit.inventory("Packagist")))

    def test_every_maven_module_needs_a_tree_newer_than_every_pom(self):
        tree = lambda artifact: json.dumps({"groupId": "dev.cronwatch", "artifactId": "x", "version": "1", "children": [{"groupId": "org.example", "artifactId": artifact, "version": "2.0"}]})
        for module in ("", "cronwatch/", "examples/crontab/"):
            self.write(f"packages/java/{module}pom.xml", "<project/>", 1000)
        self.write("packages/java/target/security-dependencies.json", tree("parent"), 2000)
        self.write("packages/java/cronwatch/target/security-dependencies.json", tree("core"), 2000)
        with self.assertRaisesRegex(ValueError, r"no dependency tree for packages/java/examples/crontab/pom\.xml"):
            audit.inventory("Maven")
        # A module two directories down is read like any other.
        self.write("packages/java/examples/crontab/target/security-dependencies.json", tree("example-only"), 2000)
        self.assertEqual({"org.example:parent", "org.example:core", "org.example:example-only"}, {name for _, name, _ in audit.inventory("Maven")})
        os.utime(self.root / "packages/java/cronwatch/pom.xml", (3000, 3000))
        with self.assertRaisesRegex(ValueError, r"security-dependencies\.json is older than packages/java/cronwatch/pom\.xml"):
            audit.inventory("Maven")

    def test_a_restore_older_than_its_project_or_the_shared_props_is_an_error(self):
        assets = json.dumps({"libraries": {"Npgsql/9.0.0": {"type": "package"}, "Cronwatch/1.0.0": {"type": "project"}}})
        self.write("packages/dotnet/src/App/obj/project.assets.json", assets, 2000)
        project = self.write("packages/dotnet/src/App/App.csproj", "<Project/>", 1000)
        props = self.write("packages/dotnet/Directory.Packages.props", "<Project/>", 1000)
        self.assertEqual({("NuGet", "Npgsql", "9.0.0")}, set(audit.inventory("NuGet")))
        for manifest in (project, props):
            os.utime(manifest, (3000, 3000))
            with self.assertRaisesRegex(ValueError, "is older than .*dotnet restore --force"):
                audit.inventory("NuGet")
            os.utime(manifest, (1000, 1000))


if __name__ == "__main__":
    unittest.main()
