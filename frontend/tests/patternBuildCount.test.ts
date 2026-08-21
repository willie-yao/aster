import assert from "node:assert/strict";
import { test } from "node:test";
import { buildsAnalyzedLabel, patternCountOutdated } from "../src/lib/dashboardOverview.js";
import type { PatternRefreshStatus } from "../src/types/dashboard.js";

function refresh(
  state: PatternRefreshStatus["state"],
  evidenceAvailable: boolean,
): PatternRefreshStatus {
  return { state, evidence_available: evidenceAvailable };
}

const correlated = { builds_analyzed: 3, shared_builds: ["10", "11", "12"] };

// A current pattern was just rebuilt from the live window, so its count always
// describes that window no matter what evidence_available reports.
test("a current pattern is never reported as an earlier window", () => {
  assert.equal(patternCountOutdated(correlated, refresh("current", true)), false);
  assert.equal(patternCountOutdated(correlated, refresh("current", false)), false);
  assert.equal(buildsAnalyzedLabel(correlated, refresh("current", false)), "3 builds");
});

// This is the case the label exists for: correlation dropped below its build
// threshold, the pattern froze, and the builds behind its count aged out.
test("a retained pattern whose correlated builds aged out is marked", () => {
  assert.equal(patternCountOutdated(correlated, refresh("retained", false)), true);
  assert.equal(
    buildsAnalyzedLabel(correlated, refresh("retained", false)),
    "3 builds (earlier window)",
  );
  assert.equal(
    buildsAnalyzedLabel(correlated, refresh("unavailable", false)),
    "3 builds (earlier window)",
  );
});

// A retained pattern that still holds every build it correlated is accurate.
test("a retained pattern with its evidence intact is not marked", () => {
  assert.equal(patternCountOutdated(correlated, refresh("retained", true)), false);
  assert.equal(buildsAnalyzedLabel(correlated, refresh("retained", true)), "3 builds");
});

// PatternEvidenceAvailable returns false whenever shared_builds is empty, which
// a non-systemic pattern always is. That says nothing about builds ageing out,
// so it must not borrow the stale label.
test("a pattern with no shared builds is never marked, whatever its refresh state", () => {
  const unrelated = { builds_analyzed: 4, shared_builds: [] };
  assert.equal(patternCountOutdated(unrelated, refresh("retained", false)), false);
  assert.equal(buildsAnalyzedLabel(unrelated, refresh("retained", false)), "4 builds");
  assert.equal(patternCountOutdated({}, refresh("retained", false)), false);
});

test("a pattern with no refresh status keeps a plain count", () => {
  assert.equal(patternCountOutdated(correlated, undefined), false);
  assert.equal(buildsAnalyzedLabel(correlated, undefined), "3 builds");
  assert.equal(buildsAnalyzedLabel({ builds_analyzed: 1, shared_builds: ["9"] }), "1 build");
});
