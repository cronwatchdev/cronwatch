import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import test from "node:test";

const filter = fileURLToPath(new URL("./go-api-report.awk", import.meta.url));
const version = '- Version: value changed from "0.14.0" to "0.14.1"';
function report(input, pkg = "cronwatch.dev/go") {
  return execFileSync("awk", ["-v", `package=${pkg}`, "-f", filter], { input, encoding: "utf8" });
}

test("a release version bump is compatible", () => {
  assert.equal(report(version + "\n"), "");
});
test("a version bump does not hide other breaking changes", () => {
  assert.equal(report(version + "\n- Job.Run: removed\n"), "- Job.Run: removed\n");
});
test("Version type changes and removals still fail", () => {
  for (const change of ["- Version: removed", "- Version: changed from untyped string to int", '- Limit: value changed from 1 to 2']) {
    assert.equal(report(change + "\n"), change + "\n");
  }
});
test("other packages get no Version exception", () => {
  assert.equal(report(version + "\n", "cronwatch.dev/go/river"), version + "\n");
});
