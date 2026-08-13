import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { test } from "node:test";

import { chatFixVerifiedSourcePaths } from "../src/lib/chatFixEligibility.js";

function source(path: string): string {
  return readFileSync(resolve(process.cwd(), path), "utf8");
}

test("exact JUnit chat fix requires current-turn artifacts and verified source paths", () => {
  const chat = source("src/components/AnalysisChat.tsx");
  assert.match(chat, /features\.junit_chat_fix/);
  assert.match(chat, /analysisRef\.source !== "build"/);
  assert.match(chat, /analysisRef\.junit_file/);
  assert.match(chat, /message\.citations\?\.length/);
  assert.match(chat, /chatFixVerifiedSourcePaths/);
  assert.match(chat, /hasExplicitSourceSymbol/);
  assert.match(chat, /no validated artifact citation from this turn/);
  assert.match(chat, /no verified immutable source paths/);
  assert.match(chat, /explicit backticked source symbol/);
});

test("exact JUnit source-path eligibility requires the bound repository and revision", () => {
  const repository = {
    owner: "kubernetes-sigs",
    name: "cluster-api-provider-azure",
    revision: "0123456789abcdef0123456789abcdef01234567",
  };
  assert.deepEqual(
    chatFixVerifiedSourcePaths(
      {
        "controllers/cluster_controller.go":
          "https://github.com/kubernetes-sigs/cluster-api-provider-azure/blob/0123456789abcdef0123456789abcdef01234567/controllers/cluster_controller.go#L10",
      },
      repository,
    ),
    ["controllers/cluster_controller.go"],
  );
  assert.deepEqual(
    chatFixVerifiedSourcePaths(
      {
        "controllers/cluster_controller.go":
          "https://github.com/other/repo/blob/0123456789abcdef0123456789abcdef01234567/controllers/cluster_controller.go",
      },
      repository,
    ),
    [],
  );
  assert.deepEqual(
    chatFixVerifiedSourcePaths(
      {
        "controllers/cluster_controller.go":
          "https://github.com/kubernetes-sigs/cluster-api-provider-azure/blob/main/controllers/cluster_controller.go",
      },
      repository,
    ),
    [],
  );
});

test("exact JUnit fix dialog excludes pattern authority and keeps confirmation separate", () => {
  const dialog = source("src/components/ChatFixDialog.tsx");
  const api = source("src/lib/chatFix.ts");
  assert.match(dialog, /exactAnalysis \? null : patternID/);
  assert.match(dialog, /server resolves the exact repository revision from build metadata/);
  assert.match(dialog, /rejects the preview if the target branch has moved/);
  assert.match(dialog, /Generate fix preview/);
  assert.match(dialog, /Open draft PR/);
  assert.match(api, /patternID \? \{ pattern_id: patternID \} : \{\}/);
  assert.match(api, /api\/actions\/confirm/);
});
