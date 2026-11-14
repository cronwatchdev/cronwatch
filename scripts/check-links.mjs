/**
 * Checks every internal link in the built site. Reads each HTML file under
 * the directory given (default site/dist), and fails when an href or src on
 * this site points at a file that was not built, or at a #fragment that no
 * id on the target page carries, or when a page carries one id twice. Links
 * to other origins are not fetched. It also fails on what the site's CSP
 * would block in production: a style="" attribute, a <style> element, or a
 * <script> with its code inline (JSON-LD, which is data, is the exception),
 * an inline event handler (onclick="" and the like), a script, style sheet,
 * image, frame, or media file from an origin the CSP in deploy/nginx.conf
 * does not allow (a data: image included), a form posting to one, or a
 * script origin the CSP allows that no page loads from. A directory with no
 * pages in it fails too: a build that wrote nothing has nothing to pass.
 *
 *   node scripts/check-links.mjs [site/dist]
 *
 * CRONWATCH_NGINX_CONF names another nginx.conf to read the CSP from, for
 * this script's tests.
 */
import { existsSync, readFileSync, readdirSync, statSync } from "node:fs";
import path from "node:path";

const DIST = path.resolve(process.argv[2] ?? "site/dist");
if (!existsSync(DIST)) {
  console.error(`check-links: ${DIST} does not exist; run npm run build:site first`);
  process.exit(1);
}

function walk(dir) {
  return readdirSync(dir).flatMap((name) => {
    const full = path.join(dir, name);
    return statSync(full).isDirectory() ? walk(full) : [full];
  });
}

const pages = walk(DIST).filter((f) => f.endsWith(".html"));
const idCache = new Map();

/** The ids on a built page, read once. */
function idsOf(file) {
  if (!idCache.has(file)) {
    const html = readFileSync(file, "utf8");
    idCache.set(file, new Set([...html.matchAll(/\sid="([^"]+)"/g)].map((m) => decodeEntities(m[1]))));
  }
  return idCache.get(file);
}

function decodeEntities(s) {
  return s.replace(/&amp;/g, "&").replace(/&quot;/g, '"').replace(/&#39;/g, "'").replace(/&lt;/g, "<").replace(/&gt;/g, ">");
}

/** The built file a site path is served from, or null. */
function resolveFile(urlPath) {
  const clean = decodeURIComponent(urlPath);
  const target = path.join(DIST, clean);
  if (!target.startsWith(DIST)) return null;
  if (clean.endsWith("/")) return existsSync(path.join(target, "index.html")) ? path.join(target, "index.html") : null;
  if (existsSync(target) && statSync(target).isFile()) return target;
  if (existsSync(path.join(target, "index.html"))) return path.join(target, "index.html");
  if (existsSync(`${target}.html`)) return `${target}.html`;
  return null;
}

/** The CSP's sources for one directive, from the add_header line in deploy/nginx.conf. */
const NGINX = process.env.CRONWATCH_NGINX_CONF ?? path.join(path.dirname(new URL(import.meta.url).pathname), "..", "deploy", "nginx.conf");
const csp = /Content-Security-Policy "([^"]+)"/.exec(readFileSync(NGINX, "utf8"))?.[1] ?? "";
function sources(directive) {
  const entry = csp.split(";").map((d) => d.trim().split(/\s+/)).find(([name]) => name === directive);
  return new Set(entry ? entry.slice(1) : []);
}
/** A fetch directive's sources, or default-src's when the CSP does not name it, as a browser reads it. */
function fetchSources(directive) {
  return csp.split(";").some((d) => d.trim().split(/\s+/)[0] === directive) ? sources(directive) : sources("default-src");
}
const scriptSrc = fetchSources("script-src");
const styleSrc = fetchSources("style-src");
const imgSrc = fetchSources("img-src");
const frameSrc = fetchSources("frame-src");
const mediaSrc = fetchSources("media-src");
// form-action does not fall back to default-src: unnamed, a form may post anywhere.
const formAction = csp.split(";").some((d) => d.trim().split(/\s+/)[0] === "form-action") ? sources("form-action") : new Set(["*"]);
/** Whether a directive's sources admit a URL on another origin: the origin itself, its scheme, or *. */
function allows(set, url) {
  return set.has(url.origin) || set.has(url.protocol) || set.has("*");
}
const scriptOrigins = new Set();

const problems = [];
let checked = 0;
if (!csp) problems.push("deploy/nginx.conf: no Content-Security-Policy header found");
if (pages.length === 0) problems.push(`${DIST}: no HTML pages; run npm run build:site first`);
for (const page of pages) {
  const html = readFileSync(page, "utf8");
  const rel = `/${path.relative(DIST, page).split(path.sep).join("/")}`;
  // An id twice on a page sends a #fragment to the first of them, whichever
  // section the link meant.
  const seen = new Set();
  for (const m of html.matchAll(/\sid="([^"]+)"/g)) {
    const id = decodeEntities(m[1]);
    if (seen.has(id)) problems.push(`${rel}: the id "${id}" is on the page more than once`);
    seen.add(id);
  }
  // The CSP allows no inline style or script, so the browser would drop these.
  for (const m of html.matchAll(/<[a-z][^>]*\sstyle\s*=/gi)) {
    problems.push(`${rel}: a style attribute, which the CSP blocks (${m[0].slice(0, 60)})`);
  }
  if (/<style[\s>]/i.test(html)) problems.push(`${rel}: a <style> element, which the CSP blocks`);
  for (const m of html.matchAll(/<script\b([^>]*)>([\s\S]*?)<\/script>/gi)) {
    const attrs = m[1];
    if (/\ssrc\s*=/i.test(attrs)) continue;
    if (/\stype\s*=\s*"application\/ld\+json"/i.test(attrs)) continue;
    problems.push(`${rel}: an inline <script>, which the CSP blocks`);
  }
  // An onclick="" and its kind are inline script to the CSP, quoted or not.
  for (const m of html.matchAll(/<[a-z][^>]*?\s(on[a-z]+)\s*=/gi)) {
    problems.push(`${rel}: an inline ${m[1].toLowerCase()} handler, which the CSP blocks (${m[0].slice(0, 60)})`);
  }
  const pageUrl = new URL(rel, "https://site.invalid");
  /** The URL when it is on another origin (https:, http:, or //host), or null. */
  const elsewhere = (raw) => {
    let url;
    try {
      url = new URL(decodeEntities(raw).trim(), pageUrl);
    } catch {
      return null;
    }
    return (url.protocol === "https:" || url.protocol === "http:") && url.origin !== pageUrl.origin ? url : null;
  };
  // Other origins a page loads code, styles, images, frames, or media from, or posts a form to, must be in the CSP.
  for (const m of html.matchAll(/<script\b[^>]*\ssrc="([^"]*)"/gi)) {
    const url = elsewhere(m[1]);
    if (!url) continue;
    scriptOrigins.add(url.origin);
    if (!scriptSrc.has(url.origin)) problems.push(`${rel}: a script from ${url.origin}, which the CSP's script-src does not allow`);
  }
  for (const m of html.matchAll(/<link\b(?=[^>]*\srel="stylesheet")[^>]*\shref="([^"]*)"/gi)) {
    const url = elsewhere(m[1]);
    if (url && !styleSrc.has(url.origin)) problems.push(`${rel}: a style sheet from ${url.origin}, which the CSP's style-src does not allow`);
  }
  const loaded = [
    [/<(?:img|input)\b[^>]*\ssrc="([^"]*)"/gi, imgSrc, "an image", "img-src"],
    [/<(?:img|source)\b[^>]*\ssrcset="([^"]*)"/gi, imgSrc, "an image", "img-src"],
    [/<link\b(?=[^>]*\srel="(?:[^"]*\s)?(?:icon|apple-touch-icon)")[^>]*\shref="([^"]*)"/gi, imgSrc, "an icon", "img-src"],
    [/<video\b[^>]*\sposter="([^"]*)"/gi, imgSrc, "a poster image", "img-src"],
    [/<(?:iframe|frame)\b[^>]*\ssrc="([^"]*)"/gi, frameSrc, "a frame", "frame-src"],
    [/<(?:video|audio|track)\b[^>]*\ssrc="([^"]*)"/gi, mediaSrc, "a media file", "media-src"],
    [/<form\b[^>]*\saction="([^"]*)"/gi, formAction, "a form posting", "form-action"],
  ];
  for (const [pattern, allowed, what, directive] of loaded) {
    for (const m of html.matchAll(pattern)) {
      // A srcset is a list of URLs, each with an optional width or density.
      const urls = /srcset/.test(pattern.source) ? decodeEntities(m[1]).split(",").map((part) => part.trim().split(/\s+/)[0]).filter(Boolean) : [m[1]];
      for (const raw of urls) {
        if (/^\s*data:/i.test(raw)) {
          // * does not cover data:, which a CSP must name.
          if (!allowed.has("data:")) problems.push(`${rel}: ${what} from a data: URL, which the CSP's ${directive} does not allow`);
          continue;
        }
        const url = elsewhere(raw);
        if (url && !allows(allowed, url)) problems.push(`${rel}: ${what} ${what.endsWith("posting") ? "to" : "from"} ${url.origin}, which the CSP's ${directive} does not allow`);
      }
    }
  }
  for (const m of html.matchAll(/\s(?:href|src)="([^"]*)"/g)) {
    const raw = decodeEntities(m[1]);
    if (raw === "" || /^(mailto:|tel:|javascript:|data:)/i.test(raw)) continue;
    const url = new URL(raw, pageUrl);
    if (url.origin !== pageUrl.origin) continue;
    checked++;
    const file = url.pathname === pageUrl.pathname ? page : resolveFile(url.pathname);
    if (!file) {
      problems.push(`${rel}: ${raw} (no such file)`);
      continue;
    }
    const fragment = decodeURIComponent(url.hash.slice(1));
    if (fragment && file.endsWith(".html") && !idsOf(file).has(fragment)) {
      problems.push(`${rel}: ${raw} (no id "${fragment}" on ${path.relative(DIST, file)})`);
    }
  }
}

// An origin the CSP still trusts for script after the pages stopped using it.
for (const source of scriptSrc) {
  if (/^https?:/.test(source) && !scriptOrigins.has(source)) problems.push(`deploy/nginx.conf: script-src allows ${source}, which no page loads a script from`);
}

if (problems.length > 0) {
  console.error(`check-links: ${problems.length} problem${problems.length === 1 ? "" : "s"} (broken links, repeated ids, inline style or script, or the CSP)`);
  for (const p of problems) console.error(`  ${p}`);
  process.exit(1);
}
console.log(`check-links: ${checked} links on ${pages.length} pages, all resolve`);
