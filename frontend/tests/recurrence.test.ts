import assert from "node:assert/strict";
import test from "node:test";
import {
  describeRecurrence,
  recurrenceForBuild,
  recurrenceForBuilds,
} from "../src/lib/recurrence.js";
import type { FailureRecurrence } from "../src/types/dashboard.js";

const entry = (
  signature: string,
  occurrences: number,
  builds: string[],
): FailureRecurrence => ({
  signature,
  occurrences,
  builds,
  first_seen: "2026-03-01T00:00:00Z",
  last_seen: "2026-08-01T00:00:00Z",
});

test("recurrence is hidden when the current window already shows every failure", () => {
  // Two failures, both on screen: the run history already says this.
  assert.equal(recurrenceForBuild([entry("a", 2, ["10", "11"])], "10"), null);
  // A single failure is not yet a recurrence.
  assert.equal(recurrenceForBuild([entry("a", 1, ["10"])], "10"), null);
  // Four failures, one on screen: the other three are what the window cannot show.
  assert.equal(recurrenceForBuild([entry("a", 4, ["10"])], "10")?.occurrences, 4);
});

test("recurrence lookups tolerate missing data", () => {
  assert.equal(recurrenceForBuild(undefined, "10"), null);
  assert.equal(recurrenceForBuild([entry("a", 4, ["10"])], undefined), null);
  assert.equal(recurrenceForBuild([entry("a", 4, ["10"])], "99"), null);
  assert.equal(recurrenceForBuilds(undefined, ["10"]), null);
  assert.equal(recurrenceForBuilds([entry("a", 4, ["10"])], []), null);
});

// A causal group is identified by its verdict signature, which preserves numbers,
// while recurrence is counted under an identity that collapses them. The group
// card therefore resolves its history through the builds the two share.
test("a causal group resolves recurrence through overlapping builds", () => {
  const recurrence = [entry("rec", 6, ["20", "21"])];
  assert.equal(recurrenceForBuilds(recurrence, ["20", "21", "22"])?.occurrences, 6);
  assert.equal(recurrenceForBuilds(recurrence, ["90", "91"]), null);
});

// A fully visible cause can outrank one that reaches past the window, so ranking
// before filtering would hide the only history worth showing.
test("a fully visible cause does not suppress a longer running one", () => {
  const visible = entry("visible", 9, ["20", "21", "22", "23", "24", "25", "26", "27", "28"]);
  const older = entry("older", 5, ["20"]);
  assert.equal(recurrenceForBuilds([visible, older], ["20", "21"])?.signature, "older");
});

test("the longest running history wins among several usable candidates", () => {
  const shorter = entry("shorter", 3, ["20"]);
  const longer = entry("longer", 7, ["21"]);
  assert.equal(recurrenceForBuilds([shorter, longer], ["20", "21"])?.signature, "longer");
});

// The identity groups failures that look alike, so the wording claims similarity
// rather than a proven shared cause.
test("recurrence is described as similar failures since a month", () => {
  assert.match(describeRecurrence(entry("a", 8, ["20"])), /^8 similar failures since /);
  assert.equal(
    describeRecurrence({ ...entry("a", 1, ["20"]), first_seen: "not a date" }),
    "1 similar failure",
  );
});
