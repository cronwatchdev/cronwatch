/**
 * Fails if an em dash (U+2014), en dash (U+2013), figure dash (U+2012), or
 * horizontal bar (U+2015) appears anywhere in the repository. Runs before every build and test, so one can never quietly appear.
 * The same four spelled so that a browser or a program writes them count
 * too: an HTML entity by name or by number, and a backslash-u escape in a
 * string. Only this script and its tests may spell them, to say what to catch.
 *
 * Scans every file git tracks or would track (untracked files not ignored by
 * .gitignore), whatever its extension, skipping only binaries. Outside a git
 * checkout it walks the tree instead.
 *
 *   node scripts/check-dashes.mjs                 the files
 *   node scripts/check-dashes.mjs --commits A..B  the messages of those commits
 *                                                 (everything after --commits goes to git log)
 *
 * Ordinary hyphens are fine.
 */
import { execFileSync } from "node:child_process";
import { existsSync, readdirSync, readFileSync, statSync } from "node:fs";
import path from "node:path";

const ROOT = process.cwd();
// Skipped only at the top of the repository, so a directory deeper down that
// happens to share a name (a test's data/, say) is still checked; ignored
// build output deeper down is already left out by .gitignore. .agents and
// .claude hold third-party skills installed by tooling, not our prose.
const SKIP_ROOT = new Set(["node_modules", ".git", "dist", "data", ".next", "out", ".agents", ".claude"]);
// node_modules and .git are never ours, wherever they are.
const SKIP_ANYWHERE = new Set(["node_modules", ".git"]);
const skipped = (parts) => SKIP_ROOT.has(parts[0]) || parts.some((part) => SKIP_ANYWHERE.has(part));
const BINARY = new Set([
  ".png", ".jpg", ".jpeg", ".gif", ".webp", ".avif", ".ico", ".woff", ".woff2", ".ttf", ".otf",
  ".pdf", ".zip", ".gz", ".tgz", ".db", ".sqlite",
]);
// Written as escapes so this file does not trip its own check.
const BAD = /[\u2012-\u2015]/;
// The same characters as an HTML entity (by name, decimal, or hex) or as a
// string escape (\uXXXX, \u{XXXX}, and \N{EM DASH} and its kind in Python),
// which become the character on the page or in the output.
const SPELLED = /&(?:mdash|ndash|horbar);|&#0*821[0-3];|&#x0*201[2-5];|\\u201[2-5]|\\u\{0*201[2-5]\}|\\N\{(?:EM DASH|EN DASH|FIGURE DASH|HORIZONTAL BAR)\}/i;
// The files that name the spellings in order to catch them.
const SPELLS_THEM = new Set(["scripts/check-dashes.mjs", "scripts/checks.test.mjs"]);

const hits = [];

function scan(label, text, spelled = true) {
  text.split("\n").forEach((line, i) => {
    if (BAD.test(line) || (spelled && SPELLED.test(line))) hits.push(`${label}:${i + 1}  ${line.trim().slice(0, 90)}`);
  });
}

function git(args) {
  return execFileSync("git", args, { cwd: ROOT, encoding: "utf8", maxBuffer: 64 * 1024 * 1024 });
}

function listFiles() {
  try {
    const out = git(["ls-files", "-z", "--cached", "--others", "--exclude-standard"]);
    return out.split("\0").filter((f) => f && !skipped(f.split("/")));
  } catch {
    const files = [];
    const walk = (dir) => {
      for (const entry of readdirSync(dir)) {
        if (skipped(path.relative(ROOT, path.join(dir, entry)).split(path.sep))) continue;
        if (entry.startsWith(".env") && entry !== ".env.example") continue;
        const full = path.join(dir, entry);
        if (statSync(full).isDirectory()) walk(full);
        else files.push(path.relative(ROOT, full));
      }
    };
    walk(ROOT);
    return files;
  }
}

const commitsAt = process.argv.indexOf("--commits");
if (commitsAt >= 0) {
  const revisions = process.argv.slice(commitsAt + 1);
  if (revisions.length === 0) {
    console.error("check-dashes: --commits needs a revision range, e.g. origin/main..HEAD");
    process.exit(2);
  }
  const log = git(["log", "--format=%H%x00%B%x01", ...revisions]);
  for (const entry of log.split("\x01")) {
    const [sha, message] = entry.replace(/^\n/, "").split("\0");
    if (sha && message !== undefined) scan(`commit ${sha.slice(0, 12)}`, message);
  }
} else {
  for (const file of listFiles()) {
    if (BINARY.has(path.extname(file).toLowerCase())) continue;
    const full = path.join(ROOT, file);
    // A tracked file deleted in the working tree, or a submodule.
    if (!existsSync(full) || !statSync(full).isFile()) continue;
    const buffer = readFileSync(full);
    if (buffer.subarray(0, 8000).includes(0)) continue;
    scan(file, buffer.toString("utf8"), !SPELLS_THEM.has(file.split(path.sep).join("/")));
  }
}

if (hits.length > 0) {
  console.error(`\nFound ${hits.length} dash(es): em, en, figure, or horizontal bar, as the character, an HTML entity, or a string escape. Use a period, colon, comma, or parentheses.\n`);
  for (const h of hits) console.error("  " + h);
  console.error("");
  process.exit(1);
}

console.log(commitsAt >= 0 ? "check-dashes: commit messages clean" : "check-dashes: clean");
