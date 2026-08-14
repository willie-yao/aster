import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { test } from "node:test";

import { chatFixVerifiedSourcePaths, fixInvestigationAvailable } from "../src/lib/chatFixEligibility.js";
import {
  chatFixRequestStorageKey,
  clearStoredChatFixRequest,
  readStoredChatFixRequest,
  storeChatFixRequest,
} from "../src/lib/chatFixRequestStorage.js";
import type { AnalysisChatReference } from "../src/types/analysisChat.js";

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
  assert.match(dialog, /createAnalysisChatFixRequest/);
  assert.match(dialog, /loadAnalysisChatFixRequest/);
  assert.match(dialog, /Generation continues in the background/);
  assert.match(dialog, /previewChatFix\([\s\S]*patternID/);
  assert.match(dialog, /server resolves the exact repository revision from build metadata/);
  assert.match(dialog, /rejects the preview if the target branch has moved/);
  assert.match(dialog, /Generate fix preview/);
  assert.match(dialog, /Open draft PR/);
  assert.match(api, /fix\/requests/);
  assert.match(api, /patternID \? \{ pattern_id: patternID \} : \{\}/);
  assert.match(api, /api\/actions\/confirm/);
});

test("exact JUnit fix request storage preserves the durable request identity and instruction", () => {
  const values = new Map<string, string>();
  const storage = {
    getItem: (key: string) => values.get(key) ?? null,
    setItem: (key: string, value: string) => { values.set(key, value); },
    removeItem: (key: string) => { values.delete(key); },
  };
  const key = chatFixRequestStorageKey("session", "chat-request");
  storeChatFixRequest(storage, "session", "chat-request", { id: "request-1", instruction: "keep compatibility" });
  assert.equal(values.has(key), true);
  assert.deepEqual(readStoredChatFixRequest(storage, "session", "chat-request"), {
    id: "request-1",
    instruction: "keep compatibility",
  });
  clearStoredChatFixRequest(storage, "session", "chat-request");
  assert.equal(readStoredChatFixRequest(storage, "session", "chat-request"), null);
});

test("exact JUnit fix request storage rejects malformed request identities", () => {
  const values = new Map<string, string>();
  const storage = {
    getItem: (key: string) => values.get(key) ?? null,
    setItem: (key: string, value: string) => { values.set(key, value); },
    removeItem: (key: string) => { values.delete(key); },
  };
  const key = chatFixRequestStorageKey("session", "chat-request");
  values.set(key, JSON.stringify({ id: "bad request id", instruction: "x" }));
  assert.equal(readStoredChatFixRequest(storage, "session", "chat-request"), null);
});

test("Fix investigation control is limited to exact JUnit chat capability", () => {
  const exact: AnalysisChatReference = {
    scope: "test", job_id: "job", build_id: "1", test_name: "Test", junit_file: "junit.xml",
  };
  const pattern: AnalysisChatReference = { scope: "pattern", job_id: "job", pattern_id: "pattern", pattern_hash: "hash" };
  const build: AnalysisChatReference = {
    scope: "test", job_id: "job", build_id: "1", test_name: "Prow job execution", source: "build",
  };
  assert.equal(fixInvestigationAvailable(exact, true, true, "authenticated", true), true);
  assert.equal(fixInvestigationAvailable(exact, true, true, "anonymous", true), true);
  assert.equal(fixInvestigationAvailable(exact, true, false, "authenticated", true), false);
  assert.equal(fixInvestigationAvailable(pattern, true, true, "authenticated", true), false);
  assert.equal(fixInvestigationAvailable(build, true, true, "authenticated", true), false);
  assert.equal(fixInvestigationAvailable(exact, true, true, "loading", true), false);
  assert.equal(fixInvestigationAvailable(exact, true, true, "unavailable", true), false);
  assert.equal(fixInvestigationAvailable(exact, true, true, "authenticated", false), false);
});

test("Fix investigation starts a fresh session and keeps preview separate", () => {
  const chat = source("src/components/AnalysisChat.tsx");
  assert.match(chat, /Start fix investigation/);
  const start = chat.slice(chat.indexOf("async function startFixInvestigation"), chat.indexOf("function returnToNormalChat"));
  assert.match(start, /beginAnalysisChatFixInvestigation\(/);
  assert.match(start, /restoreControllerRef\.current\?\.abort\(\)/);
  assert.match(start, /createRequestIDRef\.current = started\.requestID/);
  assert.match(start, /const created = await started\.session/);
  assert.match(start, /setSession\(null\)/);
  assert.doesNotMatch(start, /findAnalysisChatSession/);
  assert.doesNotMatch(start, /previewChatFix|confirmChatFix|openFix/);
  assert.match(chat, /fixIntent: activeTurn\.fixIntent/);
  assert.match(chat, /Return to normal chat/);
  assert.match(chat, /does not create a branch or PR/);
});

test("anonymous Fix investigation uses the existing OAuth sign-in path", () => {
  const chat = source("src/components/AnalysisChat.tsx");
  const api = source("src/lib/analysisChat.ts");
  assert.match(chat, /async function startFixInvestigation\(\)[\s\S]*authStatus === "anonymous"[\s\S]*signIn\(\)/);
  assert.match(api, /credentials: "same-origin"/);
  assert.match(chat, /isAnalysisChatOAuthExpired\(effectiveError, authMode\)[\s\S]*signIn\(\)/);
  assert.doesNotMatch(chat + api, /document\.cookie|Authorization.*Bearer|session[_-]?key/i);
});
