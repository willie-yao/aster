import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { test } from "node:test";

function source(path: string): string {
  return readFileSync(resolve(process.cwd(), path), "utf8");
}

test("run history remains contained on narrow detail pages", () => {
  const timeline = source("src/components/RunTimeline.tsx");
  const jobDetail = source("src/pages/JobDetailPage.tsx");

  assert.match(timeline, /minWidth: 0, maxWidth: "100%", overflowX: "auto"/);
  assert.match(timeline, /width: "max-content", minWidth: "100%"/);
  assert.match(jobDetail, /component="section" sx=\{\{ minWidth: 0 \}\}[\s\S]*SectionHeading title="Run History"/);
  assert.match(jobDetail, /gridTemplateColumns: \{ xs: "minmax\(0, 1fr\)"[\s\S]*minWidth: 0/);
});
