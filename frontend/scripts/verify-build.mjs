import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { join } from "node:path";

const dist = join(process.cwd(), "dist");
const index = readFileSync(join(dist, "index.html"), "utf8");
const fallback = readFileSync(join(dist, "404.html"), "utf8");

function tags(html, name) {
  return html.replace(/<!--[\s\S]*?-->/g, "").match(new RegExp(`<${name}(?=\\s|/?>)[^>]*>`, "gi")) ?? [];
}

function attribute(tag, name) {
  for (const match of tag.matchAll(/\s+([^\s=/>]+)\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))/g)) {
    if (match[1].toLowerCase() === name) return match[2] ?? match[3] ?? match[4];
  }
}

function scriptSource(html, name) {
  const sources = tags(html, "script").map(tag => attribute(tag, "src")).filter(src => src?.endsWith(`/${name}`));
  assert.equal(sources.length, 1, `${name} must appear exactly once`);
  return sources[0];
}

function contentSecurityPolicy(html, name) {
  const policies = tags(html, "meta").filter(tag => attribute(tag, "http-equiv")?.toLowerCase() === "content-security-policy");
  assert.equal(policies.length, 1, `${name} must contain exactly one CSP`);
  const content = attribute(policies[0], "content");
  assert.ok(content?.trim(), `${name} CSP must not be empty`);
  return content.trim().replace(/\s+/g, " ");
}

const indexScript = scriptSource(index, "spa-index-redirect.js");
const fallbackScript = scriptSource(fallback, "spa-404-redirect.js");
const base = (process.env.VITE_BASE_PATH || "/").replace(/^\/?/, "/").replace(/\/?$/, "/");
assert.equal(indexScript, `${base}spa-index-redirect.js`);
assert.equal(fallbackScript, `${base}spa-404-redirect.js`);
assert.ok(fallbackScript.startsWith("/"), "404 script must use an absolute app path");

const nested = new URL(fallbackScript, "https://dashboard.example/repo/job/foo/test/bar");
assert.equal(nested.origin, "https://dashboard.example");
assert.equal(nested.pathname, fallbackScript);

for (const [name, html] of [["index", index], ["404", fallback]]) {
  assert.equal(
    contentSecurityPolicy(html, `${name} built entry`),
    contentSecurityPolicy(readFileSync(`${name}.html`, "utf8"), `${name} source entry`),
    `${name} built CSP differs from its source`,
  );
  for (const tag of tags(html, "script")) {
    assert.ok(attribute(tag, "src"), `${name} contains an inline script`);
  }
  const script = `spa-${name}-redirect.js`;
  assert.ok(readFileSync(join(dist, script), "utf8").trim(), `${script} must not be empty`);
}

console.log(`verified built SPA entries under ${base}`);
