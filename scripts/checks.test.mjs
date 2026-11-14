// The gates that run as scripts: scripts/check-dashes.mjs over a directory
// of files, and scripts/check-links.mjs over a built site and a CSP. Each is
// run as a child, as npm and CI run it.
//
//   node --test scripts/checks.test.mjs
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { test } from "node:test";

const SCRIPTS = import.meta.dirname;

/** A scratch directory holding `files` (path: text), removed when the test ends. */
function scratch(t, files) {
  const dir = mkdtempSync(path.join(tmpdir(), "cronwatch-checks-"));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  for (const [name, text] of Object.entries(files)) {
    mkdirSync(path.dirname(path.join(dir, name)), { recursive: true });
    writeFileSync(path.join(dir, name), text);
  }
  return dir;
}

function run(script, args, options = {}) {
  const result = spawnSync(process.execPath, [path.join(SCRIPTS, script), ...args], { encoding: "utf8", ...options });
  return { code: result.status, out: `${result.stdout}${result.stderr}` };
}

// Outside a git checkout the script walks the directory it is run in.
const dashes = (dir) => run("check-dashes.mjs", [], { cwd: dir });

test("check-dashes passes hyphens and fails on each dash character", (t) => {
  assert.equal(dashes(scratch(t, { "a.md": "low-hanging, 4-20, a - b, --flag\n" })).code, 0);
  for (const code of [0x2012, 0x2013, 0x2014, 0x2015]) {
    const result = dashes(scratch(t, { "docs/a.md": `fine\nbefore ${String.fromCharCode(code)} after\n` }));
    assert.equal(result.code, 1, code.toString(16));
    assert.match(result.out, /docs\/a\.md:2 /);
  }
});

test("check-dashes fails on a dash spelled as an HTML entity or a string escape", (t) => {
  const spellings = [
    "&mdash;", "&ndash;", "&horbar;", "&MDASH;", "&#8212;", "&#8211;", "&#08210;", "&#8213;", "&#x2014;", "&#X2013;", "&#x02012;",
    "\\u2014", "\\u2013", "\\u2012", "\\u2015", "\\u{2014}", "\\N{EM DASH}", "\\N{EN DASH}",
  ];
  for (const spelling of spellings) {
    const result = dashes(scratch(t, { "site/page.html": `<p>one ${spelling} two</p>\n` }));
    assert.equal(result.code, 1, spelling);
    assert.match(result.out, /site\/page\.html:1 /, spelling);
    assert.match(result.out, /as the character, an HTML entity, or a string escape/, spelling);
  }
  // What only looks like one: other entities, other code points, a hyphen's own escape.
  const fine = ["&amp;", "&minus;", "&#8216;", "&#x2018;", "&#8209;", "\\u2010", "\\u2011", "\\u2016", "mdash", "&mdash"];
  const result = dashes(scratch(t, { "site/page.html": fine.map((s) => `<p>${s}</p>`).join("\n") }));
  assert.equal(result.code, 0, result.out);
});

test("check-dashes lets only itself and this test spell a dash, and never write one", (t) => {
  const spelled = "const BAD = /[\\u2012-\\u2015]/; // &mdash;\n";
  assert.equal(dashes(scratch(t, { "scripts/check-dashes.mjs": spelled, "scripts/checks.test.mjs": spelled })).code, 0);
  assert.equal(dashes(scratch(t, { "scripts/other.mjs": spelled })).code, 1);
  assert.equal(dashes(scratch(t, { "packages/x/scripts/check-dashes.mjs": spelled })).code, 1);
  assert.equal(dashes(scratch(t, { "scripts/check-dashes.mjs": `// ${String.fromCharCode(0x2014)}\n` })).code, 1);
});

const CSP = "default-src 'self'; script-src 'self' https://analytics.example; style-src 'self' https://fonts.example; img-src 'self'; form-action 'self'";
const page = (body) => `<!doctype html><html><head><title>t</title><script src="https://analytics.example/s.js" defer></script></head><body>${body}</body></html>`;

/** check-links over a site of `files`, under a CSP. */
function links(t, files, csp = CSP) {
  const dir = scratch(t, { "nginx.conf": `server {\n    add_header Content-Security-Policy "${csp}" always;\n}\n`, ...Object.fromEntries(Object.entries(files).map(([name, text]) => [`dist/${name}`, text])) });
  return run("check-links.mjs", [path.join(dir, "dist")], { env: { ...process.env, CRONWATCH_NGINX_CONF: path.join(dir, "nginx.conf") } });
}

test("check-links passes a site whose links resolve and whose origins the CSP allows", (t) => {
  const result = links(t, {
    "index.html": page('<link rel="stylesheet" href="https://fonts.example/kit.css"><a href="/docs/#top">docs</a><img src="/logo.png" srcset="/logo.png 1x, /logo.png 2x"><form action="/contact" method="post"></form><a href="https://elsewhere.example/">out</a>'),
    "docs/index.html": page('<h1 id="top">Docs</h1><a href="../">home</a>'),
    "logo.png": "png",
  });
  assert.equal(result.code, 0, result.out);
  assert.match(result.out, /links on 2 pages, all resolve/);
});

test("check-links fails on a build with no pages", (t) => {
  const result = links(t, { "style.css": "body{}" });
  assert.equal(result.code, 1);
  assert.match(result.out, /no HTML pages/);
});

test("check-links fails on what the CSP would block beyond inline style and script", (t) => {
  const cases = [
    ['<img src="https://img.shields.io/badge.svg" alt="">', /an image from https:\/\/img\.shields\.io, which the CSP's img-src does not allow/],
    ['<img src="/logo.png" srcset="/logo.png 1x, https://cdn.example/logo@2x.png 2x" alt="">', /an image from https:\/\/cdn\.example, which the CSP's img-src/],
    ['<img src="data:image/png;base64,AAAA" alt="">', /an image from a data: URL, which the CSP's img-src/],
    ['<img src="//cdn.example/logo.png" alt="">', /an image from https:\/\/cdn\.example/],
    ['<button onclick="go()">Go</button>', /an inline onclick handler, which the CSP blocks/],
    ["<body onload=init()>", /an inline onload handler/],
    ['<script src="//evil.example/x.js"></script>', /a script from https:\/\/evil\.example, which the CSP's script-src does not allow/],
    ['<link rel="stylesheet" href="//styles.example/a.css">', /a style sheet from https:\/\/styles\.example/],
    ['<iframe src="https://video.example/embed/1"></iframe>', /a frame from https:\/\/video\.example, which the CSP's frame-src does not allow/],
    ['<video src="https://video.example/a.mp4"></video>', /a media file from https:\/\/video\.example, which the CSP's media-src/],
    ['<form action="https://forms.example/submit" method="post"></form>', /a form posting to https:\/\/forms\.example, which the CSP's form-action/],
  ];
  for (const [body, problem] of cases) {
    const result = links(t, { "index.html": page(body), "logo.png": "png" });
    assert.equal(result.code, 1, body);
    assert.match(result.out, problem, body);
    assert.match(result.out, /check-links: 1 problem /, `${body}\n${result.out}`);
  }
});

test("check-links reads each directive as a browser does", (t) => {
  // img-src names the origin and data:, and a scheme admits every origin on it.
  const body = '<img src="https://cdn.example/a.png" alt=""><img src="data:image/png;base64,AAAA" alt=""><iframe src="https://video.example/1"></iframe>';
  const open = links(t, { "index.html": page(body) }, "default-src 'self'; script-src 'self' https://analytics.example; img-src 'self' data: https://cdn.example; frame-src https:");
  assert.equal(open.code, 0, open.out);
  // No form-action: a form may post anywhere. No img-src: default-src decides.
  const fallback = links(t, { "index.html": page('<form action="https://forms.example/submit"></form><img src="https://cdn.example/a.png" alt="">') }, "default-src 'self'; script-src 'self' https://analytics.example");
  assert.equal(fallback.code, 1);
  assert.match(fallback.out, /check-links: 1 problem /);
  assert.match(fallback.out, /an image from https:\/\/cdn\.example/);
});
