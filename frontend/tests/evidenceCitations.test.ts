import assert from "node:assert/strict";
import { test } from "node:test";

import {
  COLLAPSE_THRESHOLD,
  MAX_RENDERED_CITATIONS,
  citationKey,
  citationSummary,
  formatCitationRange,
  usableCitations,
} from "../src/lib/evidenceCitations.js";
import type { EvidenceCitation } from "../src/types/dashboard.js";

function citation(overrides: Partial<EvidenceCitation> = {}): EvidenceCitation {
  return {
    path: "build-log.txt",
    line_start: 12,
    line_end: 18,
    quote: "failed to reconcile machine",
    ...overrides,
  };
}

test("an analysis without citations renders nothing", () => {
  // Every analysis cached before citations were rendered has no field at all,
  // so the section must stay absent rather than show an empty header.
  assert.deepEqual(usableCitations(undefined), []);
  assert.deepEqual(usableCitations([]), []);
});

test("a single cited line reads as one line, not a range", () => {
  assert.equal(formatCitationRange(citation({ line_start: 12, line_end: 12 })), "L12");
  assert.equal(formatCitationRange(citation({ line_start: 12, line_end: 18 })), "L12-L18");
});

test("a malformed line range is treated as a single line", () => {
  // The engine rejects these, but the UI must not print "L12-L4".
  assert.equal(formatCitationRange(citation({ line_start: 12, line_end: 4 })), "L12");
});

test("citations naming the same region are shown once", () => {
  const usable = usableCitations([
    citation(),
    citation({ quote: "a differently worded quote for the same lines" }),
  ]);
  assert.equal(usable.length, 1);
});

test("citations are ordered by artifact then position", () => {
  const usable = usableCitations([
    citation({ path: "b.txt", line_start: 5, line_end: 5 }),
    citation({ path: "a.txt", line_start: 90, line_end: 95 }),
    citation({ path: "a.txt", line_start: 3, line_end: 4 }),
  ]);
  assert.deepEqual(
    usable.map(citationKey),
    ["a.txt:3-4", "a.txt:90-95", "b.txt:5-5"],
  );
});

test("malformed citations are dropped rather than rendered as broken claims", () => {
  const usable = usableCitations([
    citation(),
    citation({ path: "   ", line_start: 1, line_end: 1 }),
    citation({ path: "empty-quote.txt", quote: "   " }),
    citation({ path: "zero-line.txt", line_start: 0, line_end: 0 }),
    citation({ path: "inverted.txt", line_start: 9, line_end: 2 }),
    citation({ path: "nan.txt", line_start: Number.NaN, line_end: 4 }),
  ]);
  assert.deepEqual(usable.map((entry) => entry.path), ["build-log.txt"]);
});

test("the rendered list is bounded even if the backend cap changes", () => {
  const many = Array.from({ length: MAX_RENDERED_CITATIONS + 15 }, (_unused, index) =>
    citation({ line_start: index + 1, line_end: index + 1 }),
  );
  assert.equal(usableCitations(many).length, MAX_RENDERED_CITATIONS);
});

test("terminal colour codes are stripped from the quote", () => {
  const [only] = usableCitations([
    citation({ quote: "\u001B[31mfatal:\u001B[0m context deadline exceeded" }),
  ]);
  assert.equal(only.quote, "fatal: context deadline exceeded");
});

test("a quote of nothing but colour codes is dropped", () => {
  assert.deepEqual(usableCitations([citation({ quote: "\u001B[31m\u001B[0m" })]), []);
});

test("trailing whitespace is trimmed but leading indentation is kept", () => {
  const [only] = usableCitations([citation({ quote: "    indented failure line   " })]);
  assert.equal(only.quote, "    indented failure line");
});

test("the summary counts citations and the artifacts they come from", () => {
  assert.equal(citationSummary(usableCitations([citation()])), "1 citation from 1 artifact");
  assert.equal(
    citationSummary(
      usableCitations([
        citation({ path: "a.txt", line_start: 1, line_end: 1 }),
        citation({ path: "a.txt", line_start: 9, line_end: 9 }),
        citation({ path: "b.txt", line_start: 4, line_end: 4 }),
      ]),
    ),
    "3 citations from 2 artifacts",
  );
});

test("a short list stays open and a longer one collapses", () => {
  const short = Array.from({ length: COLLAPSE_THRESHOLD }, (_unused, index) =>
    citation({ line_start: index + 1, line_end: index + 1 }),
  );
  const long = Array.from({ length: COLLAPSE_THRESHOLD + 1 }, (_unused, index) =>
    citation({ line_start: index + 1, line_end: index + 1 }),
  );
  assert.equal(usableCitations(short).length > COLLAPSE_THRESHOLD, false);
  assert.equal(usableCitations(long).length > COLLAPSE_THRESHOLD, true);
});

test("fractional line numbers are rejected", () => {
  assert.deepEqual(usableCitations([citation({ line_start: 1.5, line_end: 4 })]), []);
  assert.deepEqual(usableCitations([citation({ line_start: 1, line_end: 4.2 })]), []);
});

test("a span wider than the engine allows is rejected", () => {
  // A wider span claims a precision about the evidence that was never
  // established, so it is not rendered as though it were verified.
  // The engine rejects when the difference reaches 200, so L1-L200 is valid.
  assert.equal(usableCitations([citation({ line_start: 1, line_end: 201 })]).length, 0);
  assert.equal(usableCitations([citation({ line_start: 1, line_end: 200 })]).length, 1);
});

test("OSC hyperlink sequences do not leak printable junk into the quote", () => {
  const [only] = usableCitations([
    citation({ quote: "see \u001B]8;;https://example.com/build\u0007the build log\u001B]8;;\u0007 now" }),
  ]);
  assert.equal(only.quote, "see the build log now");
  assert.doesNotMatch(only.quote, /\]8;;/u);
});

test("an OSC sequence terminated by ST is also stripped", () => {
  const [only] = usableCitations([
    citation({ quote: "title\u001B]0;window title\u001B\\ kept" }),
  ]);
  assert.equal(only.quote, "title kept");
});

test("a progress bar does not become dozens of blank lines", () => {
  // Under pre-wrap each carriage return would otherwise render as a line
  // break and grow the panel without bound.
  const progress = Array.from({ length: 40 }, (_unused, index) => `downloading ${index}%`).join("\r");
  const [only] = usableCitations([citation({ quote: progress })]);
  assert.equal(only.quote, "downloading 39%");
  assert.equal(only.quote.split("\n").length, 1);
});

test("carriage-return line endings do not double the rendered height", () => {
  const [only] = usableCitations([citation({ quote: "first\r\nsecond\r\nthird" })]);
  assert.equal(only.quote, "first\nsecond\nthird");
});

test("genuine multi-line quotes keep every line", () => {
  const [only] = usableCitations([citation({ quote: "line one\nline two\nline three" })]);
  assert.equal(only.quote.split("\n").length, 3);
});

test("a quote of nothing but control sequences is dropped", () => {
  assert.deepEqual(usableCitations([citation({ quote: "\u001B]8;;https://x\u0007\u001B[0m\r" })]), []);
});

test("colon-separated truecolor codes are consumed whole", () => {
  // ITU T.416 SGR uses colons, so a parameter range of only [0-9;?] would leak
  // the tail as visible text.
  const [only] = usableCitations([
    citation({ quote: "\u001B[38:2::255:0:0mred failure\u001B[0m" }),
  ]);
  assert.equal(only.quote, "red failure");
});
