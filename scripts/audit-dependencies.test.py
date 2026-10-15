"""Regression tests for advisory exception boundaries and failed inventory collection."""
import contextlib
import datetime
import importlib.util
import io
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


if __name__ == "__main__":
    unittest.main()
