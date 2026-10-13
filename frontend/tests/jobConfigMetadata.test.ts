import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { test } from "node:test";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import type { JobDetail } from "../src/types/dashboard.js";
import type { Discovery } from "../src/types/manifest.js";
import { withSSR } from "./helpers/ssr.js";

interface Props {
  job: Pick<JobDetail, "config_file" | "config_revision">;
  discovery?: Discovery;
  branch?: string;
}

const { JobConfigMetadata } = await withSSR(async (vite) =>
  await vite.ssrLoadModule("/src/components/JobConfigMetadata.tsx") as {
    JobConfigMetadata: (props: Props) => ReturnType<typeof createElement> | null;
  },
);

const configFile = "config/jobs/kubernetes/sig-scalability/sig-scalability-periodic-azure.yaml";
const revision = "a".repeat(40);

function render(overrides: Partial<Props> = {}): string {
  return renderToStaticMarkup(createElement(JobConfigMetadata, {
    job: { config_file: configFile, config_revision: revision },
    discovery: { source: "testgrid" },
    branch: "release-1.24",
    ...overrides,
  }));
}

test("job config metadata links the exact discovery revision and shows the known branch", () => {
  const html = render();
  assert.ok(html.includes(`href="https://github.com/kubernetes/test-infra/blob/${revision}/${configFile}"`));
  assert.match(html, /Defined in/);
  assert.ok(html.includes(configFile));
  assert.match(html, /tests branch/);
  assert.match(html, /release-1\.24/);
  assert.match(html, /target="_blank"/);
  assert.match(html, /rel="noopener noreferrer"/);
  assert.match(html, /aria-label="Job definition in kubernetes\/test-infra:.*opens in a new tab/);
});

test("job config metadata uses the config repository default branch when no revision is recorded", () => {
  const html = render({ job: { config_file: configFile } });
  assert.ok(html.includes(`href="https://github.com/kubernetes/test-infra/blob/master/${configFile}"`));
});

test("job config metadata recognizes TestGrid as the default discovery source", () => {
  const html = render({ discovery: {} });
  assert.ok(html.includes(`href="https://github.com/kubernetes/test-infra/blob/${revision}/${configFile}"`));
});

test("job config metadata never guesses an unknown config repository or file", () => {
  assert.equal(render({ discovery: undefined }), "");
  assert.equal(render({ discovery: { source: "bucket" } }), "");
  for (const file of [undefined, "", " ", "/config/jobs/a.yaml", "config/jobs/../a.yaml", "config/jobs//a.yaml", "config/jobs/./a.yaml", "config/jobs/a\\b.yaml"]) {
    assert.equal(render({ job: { config_file: file } }), "");
  }
});

test("job config metadata keeps the config link when the tested branch is unknown", () => {
  for (const branch of [undefined, "", " "]) {
    const html = render({ branch });
    assert.match(html, /Defined in/);
    assert.doesNotMatch(html, /tests branch/);
  }
});

test("job config metadata encodes paths and escapes branch text", () => {
  const html = render({
    job: { config_file: "config/jobs/example/job #1.yaml" },
    branch: "release/<script>",
  });
  assert.ok(html.includes("blob/master/config/jobs/example/job%20%231.yaml"));
  assert.match(html, /release\/&lt;script&gt;/);
  assert.doesNotMatch(html, /<script>/);
});

test("job page renders config metadata in its header using canonical job identity", () => {
  const page = readFileSync(resolve(process.cwd(), "src/pages/JobDetailPage.tsx"), "utf8");
  assert.match(page, /dashboard\?\.jobs\.find\(\(job\) => job\.job_id === canonicalJobID\)/);
  const header = page.slice(page.indexOf("</Breadcrumbs>"), page.indexOf("<TechnicalIdentity"));
  assert.match(header, /<JobConfigMetadata\s+job=\{data\}\s+discovery=\{manifest\.discovery\}\s+branch=\{jobSummary\?\.branch\}/);
});
