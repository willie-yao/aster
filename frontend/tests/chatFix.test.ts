import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { test } from "node:test";
import { MemoryStorage } from "./helpers/memoryStorage.js";

import { chatFixRequestPresentation } from "../src/lib/chatFixPresentation.js";
import {
  chatFixRequestStorageKey,
  clearStoredChatFixRequest,
  readStoredChatFixRequest,
  storeChatFixRequest,
} from "../src/lib/chatFixRequestStorage.js";

function source(path: string): string {
  return readFileSync(resolve(process.cwd(), path), "utf8");
}

test("exact JUnit chat fix requires only a completed answer on a structural target", () => {
  const chat = source("src/components/AnalysisChat.tsx");
  assert.match(chat, /features\.junit_chat_fix/);
  assert.match(chat, /analysisRef\.source !== "build"/);
  assert.match(chat, /analysisRef\.junit_file/);
  assert.match(chat, /completedFinding = Boolean\(message\.request_id && message\.content\.trim\(\)\)/);
  assert.match(chat, /exactFixEligible = exactFixEnabled && completedFinding/);
  assert.doesNotMatch(chat, /chatFixVerifiedCitationRequestIDs|chatFixVerifiedSourcePaths/);
  assert.doesNotMatch(chat, /hasArtifactEvidence|hasVerifiedSourcePaths|fixSourceUnavailable/);
});

test("qualified and unverified chat findings can start an investigation", () => {
  const chat = source("src/components/AnalysisChat.tsx");
  assert.match(chat, /Partially verified/);
  assert.match(chat, /Some citations were omitted or could not be verified/);
  assert.match(chat, /The evidence shown below is verified/);
  assert.match(chat, /chatFixEnabled && fixEligible && message\.request_id/);
  assert.doesNotMatch(chat, /chatFixEnabled && !unverified/);
  assert.match(chat, /validation repair/);
  assert.doesNotMatch(chat, /response-contract repair/);
  assert.doesNotMatch(chat, /The response contract was rejected/);
  assert.match(chat, /The response or its evidence did not pass validation/);
});

test("missing source hints and citations do not create permanent ineligibility notices", () => {
  const chat = source("src/components/AnalysisChat.tsx");
  assert.doesNotMatch(chat, /Fix preview is not possible|no verified immutable source path/);
  assert.doesNotMatch(chat, /validated artifact citation.*start a fix preview/);
  assert.match(chat, /questions = causeScope \? causeSuggestedQuestions : patternScope \? patternSuggestedQuestions : suggestedQuestions/);
  assert.match(chat, /"What does the build log show at the failure\?"/);
});

test("cause chat fixes use a representative failure and replace the global pattern chat", () => {
  const chat = source("src/components/AnalysisChat.tsx");
  const nextStep = source("src/components/CausalGroupNextStep.tsx");
  const banner = source("src/components/PatternBanner.tsx");
  const dialog = source("src/components/ChatFixDialog.tsx");
  assert.match(chat, /causeFixEnabled = causeScope && Boolean\(fixTarget\)/);
  assert.match(chat, /exactAnalysis=\{!patternScope\}/);
  assert.match(chat, /causeScope=\{causeScope\}/);
  assert.match(nextStep, /fixTarget=\{routable\?\.target \?\? undefined\}/);
  assert.match(banner, /causalGroups\.length === 0 && chatAvailability === "ready"/);
  assert.match(dialog, /representative failed JUnit target for this cause/);
});


test("prepared cause findings are labeled and remain immediately fix-eligible", () => {
  const chat = source("src/components/AnalysisChat.tsx");
  const types = source("src/types/analysisChat.ts");
  assert.match(types, /prepared\?: boolean/);
  assert.match(chat, /message\.prepared \? "Prepared finding" : "Analysis agent"/);
  assert.match(chat, /Generated during the scheduled analysis run/);
  assert.match(chat, /void createPreparedSession\(\)/);
  assert.match(chat, /chatFixEnabled && fixEligible && message\.request_id/);
});

test("exact JUnit fix dialog excludes pattern authority and keeps confirmation separate", () => {
  const dialog = source("src/components/ChatFixDialog.tsx");
  const api = source("src/lib/chatFix.ts");
  assert.match(dialog, /createAnalysisChatFixRequest/);
  assert.match(dialog, /loadAnalysisChatFixRequest/);
  assert.match(dialog, /Generation continues in the background/);
  assert.match(dialog, /previewChatFix\([\s\S]*patternID/);
  assert.match(dialog, /server resolves the repository and tested branch from build metadata/);
  assert.match(dialog, /rejects the preview if that identity changes/);
  assert.match(dialog, /Source paths from the analysis are optional investigation hints/);
  assert.match(dialog, /This finding is unverified/);
  assert.match(dialog, /No validated artifact citations accompany this finding/);
  assert.match(dialog, /Generate fix preview/);
  assert.match(dialog, /Open draft PR with warnings/);
  assert.match(dialog, /Regenerate with feedback/);
  assert.match(dialog, /request\.warning/);
  assert.match(dialog, /instruction\.trim\(\) !== submittedInstruction\.trim\(\)/);
  assert.match(dialog, /Change the previous instruction to enable regeneration/);
  assert.match(dialog, /Investigation warning/);
  assert.match(dialog, /Coding agent summary/);
  assert.match(dialog, /cancelAnalysisChatFixRequest\(request\.id\)/);
  assert.match(dialog, /clearStoredChatFixRequest[\s\S]*createAnalysisChatFixRequest/);
  assert.match(api, /fix\/requests/);
  assert.match(api, /patternID \? \{ pattern_id: patternID \} : \{\}/);
  assert.match(api, /cancelActionRequest\(API_BASE, id\)/);
  assert.match(api, /api\/actions\/confirm/);
});

test("exact JUnit fix request storage preserves the durable request identity and instruction", () => {
  const storage = new MemoryStorage();
  const key = chatFixRequestStorageKey("session", "chat-request");
  storeChatFixRequest(storage, "session", "chat-request", { id: "request-1", instruction: "keep compatibility" });
  assert.equal(storage.values.has(key), true);
  assert.deepEqual(readStoredChatFixRequest(storage, "session", "chat-request"), {
    id: "request-1",
    instruction: "keep compatibility",
  });
  clearStoredChatFixRequest(storage, "session", "chat-request");
  assert.equal(readStoredChatFixRequest(storage, "session", "chat-request"), null);
});

test("exact JUnit fix request storage rejects malformed request identities", () => {
  const storage = new MemoryStorage();
  const key = chatFixRequestStorageKey("session", "chat-request");
  storage.values.set(key, JSON.stringify({ id: "bad request id", instruction: "x" }));
  assert.equal(readStoredChatFixRequest(storage, "session", "chat-request"), null);
});

test("one chat serves questions and fix proposals with no separate mode", () => {
  const chat = source("src/components/AnalysisChat.tsx");
  // The mode, its toggle, and its fresh-session reset are gone.
  assert.doesNotMatch(chat, /Start fix investigation|Return to normal chat/);
  assert.doesNotMatch(chat, /fixIntentMode|startFixInvestigation|returnToNormalChat/);
  assert.doesNotMatch(chat, /beginAnalysisChatFixInvestigation/);
  // A finding in the one conversation still opens a fix proposal.
  assert.match(chat, /onUseForFix=\{\(\) => openFix\(message\)\}/);
  assert.doesNotMatch(chat, /fixSourceUnavailable/);
});

test("anonymous fix proposal uses the existing OAuth sign-in path", () => {
  const chat = source("src/components/AnalysisChat.tsx");
  const api = source("src/lib/analysisChat.ts");
  assert.match(chat, /function openFix\([\s\S]*authStatus === "anonymous"[\s\S]*signIn\(\)/);
  assert.match(api, /credentials: "same-origin"/);
  assert.match(chat, /isAnalysisChatOAuthExpired\(effectiveError, authMode\)[\s\S]*signIn\(\)/);
  assert.doesNotMatch(chat + api, /document\.cookie|Authorization.*Bearer|session[_-]?key/i);
});


test("exact JUnit reload and polling observe requests without regenerating", () => {
  const dialog = source("src/components/ChatFixDialog.tsx");
  const recovery = dialog.slice(dialog.indexOf("useEffect(() => {", dialog.indexOf("observeAnalysisFixRequest")), dialog.indexOf("async function generatePreview"));
  assert.match(recovery, /readStoredChatFixRequest/);
  assert.match(recovery, /observeAnalysisFixRequest/);
  assert.doesNotMatch(recovery, /createAnalysisChatFixRequest/);
});

test("exact JUnit terminal requests render persisted state without false reconnect guidance", () => {
  const dialog = source("src/components/ChatFixDialog.tsx");
  const observer = dialog.slice(dialog.indexOf("const observeAnalysisFixRequest"), dialog.indexOf("useEffect(() => {", dialog.indexOf("const observeAnalysisFixRequest")));
  assert.doesNotMatch(observer, /throw new Error\(current\.error/);
  assert.doesNotMatch(dialog, /If the connection was lost after admission, select Generate again/);
  assert.match(dialog, /request\?\.warning && !preview/);
  assert.match(dialog, /Regenerate with feedback/);
  assert.match(dialog, /requestPresentation\.severity/);
});

test("exact JUnit request presentation separates recoverable hard and observation states", () => {
  const base = {
    id: "request", failure_id: "failure", kind: "analysis-fix" as const, owner: "alice",
    stage: "drafting" as const, created_at: "2026-08-14T00:00:00Z", updated_at: "2026-08-14T00:00:00Z",
    expires_at: "2026-08-15T00:00:00Z",
  };
  const recoverable = chatFixRequestPresentation({
    ...base, status: "failed", reason_code: "no_reviewable_patch",
    error: "The coding agent completed without changing repository files.",
    failure: {
      category: "no_reviewable_patch", detail: "no_repository_change", terminal_state: "succeeded",
      operator_summary: "No deterministic repository edit was available.",
    },
  });
  assert.deepEqual(recoverable, {
    severity: "warning",
    message: "The coding agent completed, but no repository change was generated. If the remedy belongs in this repository, revise the maintainer instruction and regenerate. If it is external or operational, no patch can be generated.",
    canRegenerate: true,
    shouldObserve: false,
  });
  const tooBroad = chatFixRequestPresentation({
    ...base, status: "failed", reason_code: "no_reviewable_patch",
    error: "The coding agent returned changes outside the allowed review scope.",
    failure: { category: "no_reviewable_patch", detail: "review_scope_exceeded", terminal_state: "failed" },
  });
  assert.equal(tooBroad?.message, "The coding agent returned changes outside the allowed review scope. Add a narrower maintainer instruction and regenerate.");
  assert.equal(tooBroad?.canRegenerate, true);
  const reasonOnly = chatFixRequestPresentation({
    ...base, status: "failed", reason_code: "no_reviewable_patch",
    error: "No reviewable patch was generated.",
  });
  assert.equal(reasonOnly?.severity, "error");
  assert.equal(reasonOnly?.canRegenerate, false);
  const unauthorized = chatFixRequestPresentation({
    ...base, status: "failed", reason_code: "provider_credential_rejected",
    failure: {
      category: "provider_credential", detail: "provider_unauthorized", terminal_state: "failed",
      operator_summary: "Auth Secret agent-sandbox-model, key AI_TOKEN.",
    },
  });
  assert.equal(unauthorized?.severity, "warning");
  assert.equal(unauthorized?.canRegenerate, true);
  assert.match(unauthorized?.message ?? "", /rejected the sandbox credential \(HTTP 401\)/);
  const forbidden = chatFixRequestPresentation({
    ...base, status: "failed", reason_code: "provider_credential_rejected",
    failure: { category: "provider_credential", detail: "provider_forbidden", terminal_state: "failed" },
  });
  assert.equal(forbidden?.canRegenerate, true);
  assert.match(forbidden?.message ?? "", /refused the request \(HTTP 403\)/);
  assert.match(forbidden?.message ?? "", /model entitlement/);
  assert.doesNotMatch(forbidden?.message ?? "", /invalid credential/);
  const hard = chatFixRequestPresentation({
    ...base, status: "failed", reason_code: "unsafe_remediation", error: "Unsafe remediation blocked.",
    failure: { category: "safety_integrity" },
  });
  assert.equal(hard?.severity, "error");
  assert.equal(hard?.canRegenerate, false);
  assert.equal(hard?.message, "Fix preview generation was blocked by a safety or integrity check.");
  const pending = chatFixRequestPresentation({ ...base, status: "unknown" });
  assert.equal(pending?.severity, "info");
  assert.equal(pending?.shouldObserve, true);
});

test("exact JUnit regeneration keeps feedback replacement separate from provider retry", () => {
  const dialog = source("src/components/ChatFixDialog.tsx");
  const regenerate = dialog.slice(dialog.indexOf("async function regeneratePreview"), dialog.indexOf("async function confirm"));
  assert.match(regenerate, /providerCredentialRetry = request\.status === "failed" && request\.failure\?\.category === "provider_credential"/);
  assert.match(regenerate, /feedbackReplacement = request\.status === "failed" && request\.failure\?\.category === "no_reviewable_patch"/);
  const createRequest = regenerate.indexOf("const replacement = await createAnalysisChatFixRequest");
  const providerClearBranch = regenerate.indexOf("if (providerCredentialRetry)", createRequest);
  const clearStored = regenerate.indexOf("clearStoredChatFixRequest", providerClearBranch);
  assert.ok(createRequest >= 0 && providerClearBranch > createRequest && clearStored > providerClearBranch);
  const beforeCreate = regenerate.slice(0, createRequest);
  assert.doesNotMatch(beforeCreate, /if \(providerCredentialRetry\) \{[\s\S]*clearStoredChatFixRequest/);
  assert.doesNotMatch(beforeCreate, /setRequest\(null\)/);
  assert.match(regenerate, /if \(!providerCredentialRetry && !feedbackReplacement\) \{[\s\S]*cancelAnalysisChatFixRequest/);
  assert.equal((regenerate.match(/createAnalysisChatFixRequest\(/g) ?? []).length, 1);
  assert.match(regenerate, /feedbackReplacement \? request\.id : undefined/);
  assert.match(dialog, /disabled=\{busy !== null \|\| \(!isProviderCredentialRetry && !hasRevisedInstruction\)\}/);
  assert.match(dialog, /Retry fix preview/);
  assert.match(dialog, /Provider diagnostic/);
  assert.match(dialog, /!request && !preview && !url/);
  assert.match(dialog, /Review fix preview/);
});

test("recoverable no-patch feedback is grouped with regeneration controls", () => {
  const dialog = source("src/components/ChatFixDialog.tsx");
  const finding = dialog.indexOf("Investigation warning");
  const noPatch = dialog.indexOf("Generation completed without a patch");
  const instruction = dialog.indexOf('label="Maintainer instruction (optional)"');
  const regenerate = dialog.indexOf("Regenerate with feedback");
  assert.ok(finding > dialog.indexOf('title="Selected chat finding"'));
  assert.ok(noPatch > finding);
  assert.ok(noPatch < instruction);
  assert.ok(instruction < regenerate);
  assert.match(dialog, /requestPresentation && !requestPresentation\.canRegenerate/);
});
