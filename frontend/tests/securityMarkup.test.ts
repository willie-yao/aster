import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { test } from "node:test";

function source(path: string): string {
  return readFileSync(join(process.cwd(), path), "utf8");
}

test("entry documents use same-origin scripts and fonts", () => {
  const index = source("index.html");
  const fallback = source("404.html");
  for (const [name, html] of [["index", index], ["404", fallback]] as const) {
    assert.doesNotMatch(html, /fonts\.(?:googleapis|gstatic)\.com/i, name);
    assert.doesNotMatch(html, /<script(?![^>]*\bsrc=)[^>]*>[\s\S]*?<\/script>/i, name);
    assert.match(html, /Content-Security-Policy/i, name);
    assert.match(html, /script-src 'self'/i, name);
    assert.match(html, /font-src 'self'/i, name);
  }
  assert.match(index, /src="\/spa-index-redirect\.js"/);
  assert.match(fallback, /src="\/spa-404-redirect\.js"/);
});
