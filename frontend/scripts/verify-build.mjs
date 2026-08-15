import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { join } from "node:path";

const dist = join(process.cwd(), "dist");
const index = readFileSync(join(dist, "index.html"), "utf8");
const fallback = readFileSync(join(dist, "404.html"), "utf8");

function scriptSource(html, name) {
  const match = html.match(new RegExp(`<script[^>]+src="([^"]*${name})"`));
  assert.ok(match, `${name} script is missing`);
  return match[1];
}

const indexScript = scriptSource(index, "spa-index-redirect\\.js");
const fallbackScript = scriptSource(fallback, "spa-404-redirect\\.js");
const base = indexScript.slice(0, -"spa-index-redirect.js".length);
assert.equal(fallbackScript, `${base}spa-404-redirect.js`);
assert.ok(fallbackScript.startsWith("/"), "404 script must use an absolute app path");

const nested = new URL(fallbackScript, "https://dashboard.example/repo/job/foo/test/bar");
assert.equal(nested.origin, "https://dashboard.example");
assert.equal(nested.pathname, fallbackScript);

for (const [name, html] of [["index", index], ["404", fallback]]) {
  assert.doesNotMatch(
    html,
    /<script(?![^>]*\bsrc=)[^>]*>[\s\S]*?<\/script>/i,
    `${name} contains an inline script`,
  );
}

console.log(`verified built SPA entries under ${base || "/"}`);
