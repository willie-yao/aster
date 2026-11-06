import assert from "node:assert/strict";
import test from "node:test";
import { ThemeProvider, type Theme } from "@mui/material/styles";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { withSSR } from "./helpers/ssr.js";
import { diffTotals, parseMarkdownBlocks, parseUnifiedDiff } from "../src/lib/fixReview.js";
import type { ActionPreview } from "../src/types/actions.js";

interface FixReviewProps {
  preview: ActionPreview;
  branch?: string;
  instruction: string;
  onInstruction: (value: string) => void;
  instructionBytes: number;
  canRevise: boolean;
  reviseDisabled: boolean;
  onRevise: () => void;
  generating: boolean;
  sentInstruction: string;
  busy: "preview" | "regenerate" | "confirm" | null;
  hasWarnings: boolean;
  error: string | null;
  observation: string | null;
  onBack: () => void;
  onCancel: () => void;
  onConfirm: () => void;
}

const { FixReviewWorkspace, ActionDraftPreview, defaultTheme } = await withSSR(async (vite) => {
  const { FixReviewWorkspace } = (await vite.ssrLoadModule("/src/components/FixReviewWorkspace.tsx")) as {
    FixReviewWorkspace: (props: FixReviewProps) => ReturnType<typeof createElement>;
  };
  const { defaultTheme } = (await vite.ssrLoadModule("/src/theme/index.ts")) as { defaultTheme: Theme };
  const { ActionDraftPreview } = (await vite.ssrLoadModule("/src/components/ActionDraftPreview.tsx")) as {
    ActionDraftPreview: (props: { preview: ActionPreview }) => ReturnType<typeof createElement>;
  };
  return { FixReviewWorkspace, ActionDraftPreview, defaultTheme };
});

const twoFileDiff = `diff --git a/pkg/controller.go b/pkg/controller.go
index 1111111..2222222 100644
--- a/pkg/controller.go
+++ b/pkg/controller.go
@@ -10,4 +10,4 @@ func reconcile() {
 	ctx := context.Background()
--	waitFor(ctx)
+	waitForWithTimeout(ctx, timeout)
 	return nil
\\ No newline at end of file
diff --git a/templates/cluster.yaml b/templates/cluster.yaml
--- a/templates/cluster.yaml
+++ b/templates/cluster.yaml
@@ -1,2 +1,3 @@
 kind: Cluster
+spec: {}
`;

test("unified diffs split into files with line numbers and change counts", () => {
  const files = parseUnifiedDiff(twoFileDiff);
  assert.deepEqual(files.map((file) => [file.path, file.additions, file.deletions]), [
    ["pkg/controller.go", 1, 1],
    ["templates/cluster.yaml", 1, 0],
  ]);
  // A removed line that itself starts with "-" is still a removal, not a file header.
  const [context, removed, added] = files[0].hunks[0].lines;
  assert.deepEqual(context, { kind: "ctx", oldNo: 10, newNo: 10, text: "\tctx := context.Background()" });
  assert.deepEqual(removed, { kind: "del", oldNo: 11, text: "-\twaitFor(ctx)" });
  assert.deepEqual(added, { kind: "add", newNo: 11, text: "\twaitForWithTimeout(ctx, timeout)" });
  assert.equal(files[0].hunks[0].lines.length, 4);
  assert.equal(files[0].hunks[0].lines[3].noNewline, true);
  assert.deepEqual(diffTotals(files), { additions: 2, deletions: 1 });
  assert.deepEqual(parseUnifiedDiff(""), []);
});

test("adding and removing a final newline parse differently", () => {
  const header = "diff --git a/f b/f\n--- a/f\n+++ b/f\n@@ -1 +1 @@\n";
  const added = parseUnifiedDiff(`${header}-x\n\\ No newline at end of file\n+x\n`)[0].hunks[0].lines;
  const removed = parseUnifiedDiff(`${header}-x\n+x\n\\ No newline at end of file\n`)[0].hunks[0].lines;
  assert.deepEqual(added.map((line) => [line.kind, Boolean(line.noNewline)]), [["del", true], ["add", false]]);
  assert.deepEqual(removed.map((line) => [line.kind, Boolean(line.noNewline)]), [["del", false], ["add", true]]);
});

test("drafted pull request bodies parse into blocks without template comments", () => {
  const blocks = parseMarkdownBlocks([
    "<!-- Describe the change. -->",
    "## Why",
    "",
    "The bootstrap script",
    "is not executable.",
    "",
    "- [template](https://github.com/o/r/blob/abc/t.yaml)",
    "- `kubeadm.service`",
    "> Quoted note",
    "```",
    "make test",
    "```",
  ].join("\n"));
  assert.deepEqual(blocks, [
    { type: "heading", level: 2, text: "Why" },
    { type: "paragraph", text: "The bootstrap script\nis not executable." },
    { type: "list", items: ["[template](https://github.com/o/r/blob/abc/t.yaml)", "`kubeadm.service`"] },
    { type: "quote", text: "Quoted note" },
    { type: "code", text: "make test" },
  ]);
});

function props(overrides: Partial<FixReviewProps> = {}): FixReviewProps {
  const noop = () => {};
  return {
    preview: {
      kind: "fix",
      title: "Fix recurring failure: kubeadm bootstrap is denied execution",
      body: "## Why\n\nThe bootstrap script is not executable.",
      diff: twoFileDiff,
      verify_status: "passed",
      verify_summary: "go build ./... passed",
    },
    instruction: "",
    onInstruction: noop,
    instructionBytes: 0,
    canRevise: true,
    reviseDisabled: true,
    onRevise: noop,
    generating: false,
    sentInstruction: "",
    busy: null,
    hasWarnings: false,
    error: null,
    observation: null,
    onBack: noop,
    onCancel: noop,
    onConfirm: noop,
    ...overrides,
  };
}

function render(value: FixReviewProps): string {
  return renderToStaticMarkup(createElement(ThemeProvider, { theme: defaultTheme }, createElement(FixReviewWorkspace, value)))
    .replace(/<style\b[^>]*>[\s\S]*?<\/style>/g, "");
}

test("fix review lists files only when the patch touches more than one", () => {
  const multi = render(props());
  assert.match(multi, /aria-label="Changed files"/);
  assert.match(multi, /2 files changed/);

  const single = render(props({ preview: { ...props().preview, diff: twoFileDiff.split("diff --git a/templates")[0] } }));
  assert.doesNotMatch(single, /aria-label="Changed files"/);
  assert.match(single, /pkg\/controller\.go/);
});

test("fix review keeps each labeled description field on its own line", () => {
  // The shape analysisFailureDescription drafts: one field per line, no blank lines.
  const body = "**Failure source:** `o/r@abc` on branch `release-1.26`\n**Generation base:** `o/r@def` on branch `release-1.26`";
  assert.deepEqual(parseMarkdownBlocks(body), [{ type: "paragraph", text: body }]);
  const html = render(props({ preview: { ...props().preview, body } }));
  assert.match(html, /<\/code> on branch <code[^>]*>release-1\.26<\/code>\n<strong>Generation base:<\/strong>/);
});

test("fix review keeps recovery guidance and a multiline revision", () => {
  const html = render(props({
    error: "Failed to fetch",
    observation: "The request could not be refreshed. Reopen this dialog to check its current status.",
    instruction: "Only touch\nthe template.",
    instructionBytes: 27,
  }));
  assert.match(html, /role="status"[^>]*>[\s\S]*?The request could not be refreshed/);
  assert.match(html, /<textarea[^>]*aria-label="Revision instruction"[^>]*>Only touch\nthe template\.<\/textarea>/);
  assert.match(html, /27\/4096 bytes/);
});

test("fix review links the angle-bracket destinations the engine writes", () => {
  const html = render(props({ preview: { ...props().preview, body: "Failed in [build 42](<https://prow.example/view/42>)." } }));
  assert.match(html, /<a[^>]*href="https:\/\/prow\.example\/view\/42"[^>]*>build 42<\/a>/);
});

test("fix review opens a short description and folds a long one", () => {
  assert.match(render(props()), /aria-expanded="true"[^>]*>[\s\S]*?Description/);
  const long = render(props({ preview: { ...props().preview, body: `## Why\n\n${"The bootstrap script is not executable. ".repeat(20)}` } }));
  assert.match(long, /aria-expanded="false"[^>]*>[\s\S]*?Description/);
});

test("fix review keeps the title, verification, and confirmation in view", () => {
  const html = render(props({ hasWarnings: true, branch: "release-1.26" }));
  assert.match(html, /<h2[^>]*title="Fix recurring failure: kubeadm bootstrap is denied execution"/);
  assert.match(html, /into <code[^>]*>release-1\.26<\/code>/);
  // The header names the result and opens the summary on demand; below md the
  // body shows the summary itself.
  assert.match(html, /aria-haspopup="dialog"[^>]*>[\s\S]*?Configured checks passed/);
  assert.match(html, /Passing checks do not establish that the change achieves the requested outcome/);
  assert.match(html, /\\ No newline at end of file/);
  assert.match(html, />Open draft PR with warnings</);
  assert.match(html, /aria-label="Revision instruction"/);

  const regenerating = render(props({ generating: true, canRevise: false, sentInstruction: "Only touch the template." }));
  assert.match(regenerating, /role="status"[\s\S]*Regenerating the fix preview[\s\S]*Only touch the template\./);
  assert.doesNotMatch(regenerating, /aria-label="Revision instruction"/);
});

const previewSurfaces = [
  (preview: ActionPreview) => render(props({ preview })),
  (preview: ActionPreview) => renderToStaticMarkup(createElement(ThemeProvider, { theme: defaultTheme }, createElement(ActionDraftPreview, { preview }))),
];

test("diff-only checks preserve the command scope and leave task success unverified", () => {
  for (const renderPreview of previewSurfaces) {
    const html = renderPreview({ ...props().preview, verify_summary: "git diff --cached --check passed in Agent Sandbox" });
    assert.match(html, /Configured checks passed/);
    assert.match(html, /git diff --cached --check passed in Agent Sandbox/);
    assert.match(html, /Passing checks do not establish that the change achieves the requested outcome/);
    assert.doesNotMatch(html, /Automated verification passed|Verification passed/);
  }
});

test("failed commands retain their specific result without inferring a build failure", () => {
  for (const summary of ["go build ./... failed in Agent Sandbox", "go test ./... failed in Agent Sandbox", "validator failed in Agent Sandbox"]) {
    for (const renderPreview of previewSurfaces) {
      const html = renderPreview({ ...props().preview, verify_status: "failed", verify_summary: summary, verify_output: "Failed command details" });
      assert.match(html, /Configured checks failed/);
      assert.ok(html.includes(summary));
      assert.match(html, /Review the failed command and output before using this change/);
      assert.match(html, /Command output/);
      assert.match(html, /Failed command details/);
      assert.doesNotMatch(html, /likely does not build|Passing checks/);
    }
  }
});

test("absent or skipped configured checks do not show a success verdict", () => {
  for (const status of [undefined, "skipped"]) {
    for (const renderPreview of previewSurfaces) {
      const html = renderPreview({ ...props().preview, verify_status: status, verify_summary: undefined });
      assert.match(html, /No configured checks ran/);
      assert.doesNotMatch(html, /Configured checks passed|Configured checks failed|Passing checks/);
    }
  }
});

test("passing configured checks and unavailable automated review remain separate", () => {
  const warning = "Automated review of the generated patch did not complete; review the patch without it.";
  for (const renderPreview of previewSurfaces) {
    const html = renderPreview({
      ...props().preview,
      verify_summary: "git diff --cached --check passed in Agent Sandbox",
      warnings: [{ message: warning }],
      review: {
        status: "unavailable",
        metadata: {
          attempts: [{ path: "response_format", outcome: "invalid_json", validator_called: false }],
        },
      },
    });
    assert.match(html, /Configured checks passed/);
    assert.ok(html.includes(warning));
    assert.doesNotMatch(html, /Review passed|review passed/);
  }
});
