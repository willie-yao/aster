import assert from "node:assert/strict";
import { readdirSync, readFileSync } from "node:fs";
import { join, resolve } from "node:path";
import { test } from "node:test";

function source(path: string): string {
  return readFileSync(resolve(process.cwd(), path), "utf8");
}

function sourceFiles(dir: string): string[] {
  return readdirSync(resolve(process.cwd(), dir), { withFileTypes: true }).flatMap((entry) =>
    entry.isDirectory()
      ? sourceFiles(join(dir, entry.name))
      : /\.tsx?$/.test(entry.name)
        ? [join(dir, entry.name)]
        : [],
  );
}

// The Eleven Pixel Floor is a whole-system rule, so this walks every source
// file rather than the handful a change happens to touch. The root is 17px,
// which puts the floor at 0.647rem. Icon sizing also rides on `fontSize`, so a
// deliberately tiny glyph would report here; none exists, and one should be
// looked at rather than waved through.
test("no functional text is declared below the eleven pixel floor", () => {
  const offenders: string[] = [];
  const tooSmall = (value: string, unit: string) =>
    unit === "rem" ? Number(value) * 17 < 11 : Number(value) < 11;
  for (const file of sourceFiles("src")) {
    const text = readFileSync(resolve(process.cwd(), file), "utf8");
    for (const [, value, unit] of text.matchAll(/fontSize: "?(\d+(?:\.\d+)?)(rem|px)?"?[,\s}]/g)) {
      if (tooSmall(value, unit ?? "px")) offenders.push(`${file}: ${value}${unit ?? "px"}`);
    }
    // SVG chart labels set the attribute directly, e.g. fontSize="11".
    for (const [, value] of text.matchAll(/fontSize="(\d+(?:\.\d+)?)"/g)) {
      if (tooSmall(value, "px")) offenders.push(`${file}: ${value}px`);
    }
    // Responsive values hide their sizes one level down, e.g. { xs: 10, md: 14 }.
    // A quoted value must carry rem or px so a unit this rule cannot judge,
    // such as em, is skipped rather than read as pixels.
    for (const [, block] of text.matchAll(/fontSize: \{([^{}]*)\}/g)) {
      for (const [, quoted, unit, bare] of block.matchAll(/:\s*(?:"(\d+(?:\.\d+)?)(rem|px)"|(\d+(?:\.\d+)?))\s*[,}]?/g)) {
        const value = quoted ?? bare;
        if (value && tooSmall(value, unit ?? "px")) offenders.push(`${file}: responsive ${value}${unit ?? "px"}`);
      }
    }
  }
  assert.deepEqual(offenders, []);
});

test("prose on the trends and results ledgers is held to a measure", () => {
  const trends = source("src/pages/FlakinessPage.tsx");
  const testCases = source("src/components/TestCaseTable.tsx");
  const ledger = source("src/components/ResultLedger.tsx");

  // Failure text and analysis prose run to hundreds of characters on a wide
  // ledger, so each is capped at the same measure the expanded detail uses.
  assert.match(trends, /WebkitLineClamp: 2[\s\S]{0,400}?maxWidth: "74ch"/);
  assert.match(trends, /maxWidth: "74ch"[\s\S]{0,200}?activeDefinition\.tooltip/);
  assert.match(testCases, /maxWidth: "74ch"[\s\S]{0,200}?fontSize: "13\.5px"/);
  assert.match(ledger, /maxWidth: "74ch"[\s\S]{0,200}?Skipped tests/);
});

test("detail headings use the enlarged readable scale", () => {
  const job = source("src/pages/JobDetailPage.tsx");
  const testDetail = source("src/pages/TestDetailPage.tsx");
  const buildFailure = source("src/pages/BuildFailurePage.tsx");

  assert.match(job, /fontSize: \{ xs: "26px", sm: "30px" \}/);
  assert.match(job, /lineHeight: \{ xs: "33px", sm: "38px" \}/);
  assert.match(job, /fontWeight: 720/);
  assert.match(testDetail, /: \{ xs: "26px", sm: "30px" \}/);
  assert.match(testDetail, /: \{ xs: "33px", sm: "38px" \}/);
  assert.match(testDetail, /fontWeight: 720/);
  assert.match(buildFailure, /fontSize: \{ xs: "26px", sm: "30px" \}/);
  assert.match(buildFailure, /lineHeight: \{ xs: "33px", sm: "38px" \}/);
  assert.match(buildFailure, /fontWeight: 720/);
});

test("analysis briefing uses a calmer prose measure and rhythm", () => {
  const briefing = source("src/components/AnalysisBriefing.tsx");
  const section = source("src/components/BriefingSection.tsx");

  assert.match(briefing, /maxWidth: "68ch"/);
  assert.match(briefing, /fontSize: "16px"/);
  assert.match(briefing, /lineHeight: "25px"/);
  assert.match(briefing, /fontWeight: 550/);
  assert.match(briefing, /gap: 2\.25/);
  // Both pages now get the section body scale from the shared component.
  assert.match(section, /fontSize: "16px", lineHeight: "25px"/);
});

test("detail strip dividers always use the quiet divider token", () => {
  const metrics = source("src/components/MetricStrip.tsx");
  const metadata = source("src/components/RunMetadata.tsx");

  assert.doesNotMatch(metrics, /borderLeft: "1px solid"/);
  assert.match(metrics, /borderInlineStartColor: "var\(--mui-palette-divider\)"/);
  assert.match(metrics, /borderTopColor: "var\(--mui-palette-divider\)"/);
  assert.doesNotMatch(metadata, /borderLeft:/);
  assert.match(metadata, /stacked = false/);
  assert.match(metadata, /gridTemplateColumns: stacked/);
  assert.match(metadata, /borderInlineStartWidth: stacked/);
  assert.match(metadata, /borderInlineStartColor: "var\(--mui-palette-divider\)"/);
  assert.match(metadata, /borderTopColor: "var\(--mui-palette-divider\)"/);
});

test("test rows provide a large analysis link and separate evidence controls", () => {
  const table = source("src/components/TestCaseTable.tsx");
  const linkStart = table.indexOf("component={RouterLink}", table.indexOf("analysisPath ?"));
  const linkEnd = table.indexOf("</Link>", linkStart);

  assert.notEqual(linkStart, -1);
  assert.notEqual(linkEnd, -1);
  assert.match(table, /gridColumn: \{ xs: "1", md: "1 \/ 5" \}/);
  assert.match(table, /minHeight: 54/);
  assert.match(table, /Analysis →/);
  assert.match(table, /gridColumn: \{ xs: "1", md: "5" \}/);
  assert.match(table, /Show inline evidence/);
  assert.match(table, /overflowX: "clip"/);
  assert.doesNotMatch(table.slice(linkStart, linkEnd), /failure_location_url/);
  assert.doesNotMatch(table, /<OpenInNew/);
});

test("AI summaries use readable body contrast instead of caption styling", () => {
  const table = source("src/components/TestCaseTable.tsx");

  assert.match(table, /tc\.ai_summary[\s\S]*component="div"/);
  assert.match(table, /color: "text\.primary"/);
  assert.match(table, /fontSize: "13\.5px"/);
  assert.match(table, /lineHeight: "20px"/);
  assert.match(table, /Likely transient/);
});

test("each cause is a bounded card with its own ordinal identity", () => {
  const pattern = source("src/components/PatternBanner.tsx");
  const causeStart = pattern.indexOf("causalGroups.map((group, index)");
  const causeEnd = pattern.indexOf('label="Unclassified builds"');
  const cause = pattern.slice(causeStart, causeEnd);

  assert.notEqual(causeStart, -1);
  assert.notEqual(causeEnd, -1);

  // Without a container the only boundary between two multi-paragraph causes
  // was 12px of whitespace, barely more than the gaps inside one cause.
  assert.match(cause, /border: "1px solid"/);
  assert.match(cause, /borderColor: "divider"/);
  assert.match(cause, /borderRadius: "4px"/);
  assert.match(cause, /bgcolor: "surface\.containerLow"/);

  // The header band reuses the detail-band vocabulary and carries the ordinal
  // plus the confidence, so neither reads as a continuation of the prose.
  assert.match(cause, /bgcolor: "surface\.containerHigh"/);
  assert.match(cause, /boxShadow: "inset 3px 0 0 var\(--mui-palette-primary-main\)"/);
  assert.match(cause, /causalGroups\.length > 1 \? `Cause \$\{index \+ 1\} of \$\{causalGroups\.length\}` : "Cause"/);
  assert.match(cause, /\{group\.confidence\} confidence/);

  // The confidence row kept its xs column reflow; the band grid now owns it,
  // alongside the toggle a resolved cause folds itself away with.
  assert.match(cause, /xs: '"cause toggle" "confidence confidence"'/);
  assert.match(cause, /sm: '"cause confidence toggle"'/);
});

test("causal group rhythm and headings express the hierarchy", () => {
  const pattern = source("src/components/PatternBanner.tsx");
  const nextStep = source("src/components/CausalGroupNextStep.tsx");
  const routing = source("src/components/CausalGroupFixRouting.tsx");
  const cause = pattern.slice(
    pattern.indexOf("causalGroups.map((group, index)"),
    pattern.indexOf('label="Unclassified builds"'),
  );

  // The gap between two unrelated causes has to exceed every gap inside one,
  // or the reader cannot tell a cause boundary from a paragraph boundary.
  const between = Number(/<Stack spacing=\{([\d.]+)\}>\s*\n\s*\{causalGroups\.map/.exec(pattern)?.[1]);
  const within = [cause, nextStep, routing]
    .flatMap((text) => [...text.matchAll(/\bmt: ([\d.]+)/g)])
    .map((match) => Number(match[1]));

  assert.ok(Number.isFinite(between), "inter-cause spacing not found");
  assert.ok(within.length > 0, "intra-cause spacing not found");
  assert.ok(
    between > Math.max(...within),
    `inter-cause spacing ${between} must exceed the widest intra-cause gap ${Math.max(...within)}`,
  );

  // Heading levels stay sequential: h3 section, h4 cause, h5 rows within it.
  // The h3 now lives in the shared BriefingSection rather than in each page.
  assert.match(source("src/components/BriefingSection.tsx"), /component="h3"/);
  assert.match(cause, /component="h4"/);
  assert.match(cause, /component="h5"[\s\S]*Affected \{group\.builds\.length === 1 \? "build" : "builds"\}/);
  assert.match(nextStep, /component="h5"[\s\S]*>\s*Next step\s*</);
  assert.match(nextStep, /component="h6"[\s\S]*>\s*Suggested remediation/);
});

test("one shared component defines the briefing section treatment", () => {
  const pattern = source("src/components/PatternBanner.tsx");
  const panel = source("src/components/AiAnalysisPanel.tsx");

  // These two were byte-identical copies, which is how the two pages drifted.
  assert.match(pattern, /import \{ BriefingSection \} from "\.\/BriefingSection"/);
  assert.match(panel, /import \{ BriefingSection \} from "\.\/BriefingSection"/);
  assert.doesNotMatch(pattern, /^function BriefingSection\(/m);
  assert.doesNotMatch(panel, /DetailAnalysisSection/);

  // Both pages render their labelled blocks through it.
  assert.match(pattern, /<BriefingSection label="Causal groups">/);
  assert.match(panel, /<BriefingSection label="Root cause">/);
  assert.match(panel, /<BriefingSection label="Suggested remediation">/);
});

test("a long section cannot run into the next one without a boundary", () => {
  const section = source("src/components/BriefingSection.tsx");

  // A root cause routinely runs several hundred pixels tall. The container gap
  // alone did not read as a boundary against a block that size.
  assert.match(section, /\.\$\{briefingSectionClass\} ~ &/);
  assert.match(section, /borderTop: "1px solid"/);
  assert.match(section, /borderColor: "divider"/);

  // Keying on a preceding sibling section rather than on position means an
  // intervening div, or a future sibling that happens to be a section, cannot
  // change which block goes unruled.
  assert.match(section, /className=\{briefingSectionClass\}/);
  assert.doesNotMatch(section, /first-of-type|first-child/);

  // The separation added on top of the container gap must exceed the gap
  // between a section's own label and its body, or the rhythm inverts.
  const pt = Number(/pt: ([\d.]+)/.exec(section)?.[1]);
  const labelGap = Number(/mt: ([\d.]+)[^}]*fontSize: "16px"/.exec(section)?.[1]);
  assert.ok(Number.isFinite(pt), "section top padding not found");
  assert.ok(Number.isFinite(labelGap), "label-to-body gap not found");
  assert.ok(pt > labelGap, `section padding ${pt} must exceed the label gap ${labelGap}`);
});

test("the runtime trend states its stats once and makes each sample reachable", () => {
  const trend = source("src/components/RuntimeTrend.tsx");

  // The band already reads "N samples · median X · p95 Y · <direction>", so a
  // footer repeating those same three values a few hundred pixels below is
  // duplication, not reinforcement.
  assert.match(trend, /<DetailSectionBand title="Runtime trend" metadata=\{summaryText\} \/>/);
  assert.doesNotMatch(trend, /Median: \{summary\.medianSeconds/);
  assert.doesNotMatch(trend, /p95: \{summary\.p95Seconds/);
  assert.doesNotMatch(trend, /Direction: \{trendLabel\(summary\)\}/);

  // What replaces it explains the two dashed reference lines, which nothing
  // else on the page identifies. The swatches repeat the chart's own dash
  // patterns so the mapping does not rely on telling the colours apart.
  assert.match(trend, /strokeDasharray="7 6"[\s\S]*strokeDasharray="7 6"/);
  assert.match(trend, /strokeDasharray="3 5"[\s\S]*strokeDasharray="3 5"/);

  // Every sample is a link to its run, matching how Sparkline already treats
  // run dots, rather than an inert circle. The destination is supplied by the
  // page so the test page keeps the reader on the test.
  assert.match(trend, /component=\{RouterLink\}/);
  assert.match(trend, /to=\{runHref\(sample\.buildID\)\}/);
  assert.match(trend, /cursor: "pointer"/);
  assert.match(trend, /"&:focus-visible"/);
  assert.doesNotMatch(trend, /role="img"/);

  const job = source("src/pages/JobDetailPage.tsx");
  const testDetail = source("src/pages/TestDetailPage.tsx");
  assert.match(job, /runHref=\{\(buildID\) => jobRunPath\(canonicalJobID, buildID\)\}/);
  assert.match(testDetail, /runHref=\{\(buildID\) => testRunPath\(canonicalJobID, testName, buildID\)\}/);

  // A long history reserves width per sample, so the plot must scroll rather
  // than clip, and the drawing width must grow with it so the chart does not
  // also scale taller. RunHistory directly above uses the same treatment.
  assert.match(trend, /const width = Math\.max\(720, values\.length \* 32\)/);
  assert.match(trend, /const minChartWidth = Math\.max\(320, values\.length \* 32\)/);
  assert.match(trend, /overflowX: "auto"/);
  assert.match(trend, /minWidth: minChartWidth/);
  // Points run oldest to newest, so a scrolling chart opens on the latest run.
  assert.match(trend, /node\.scrollLeft = node\.scrollWidth/);
});

test("the chat wears the page's section band instead of its own chat styling", () => {
  const chat = source("src/components/AnalysisChat.tsx");
  const band = source("src/components/DetailSectionBand.tsx");
  // Scope to the header, since UserMessage below carries the same accent and
  // would otherwise satisfy these assertions on its own.
  const header = chat.slice(
    chat.indexOf("{!detailAppearance && <Divider"),
    chat.indexOf("<Collapse in={expanded}"),
  );
  assert.ok(header.length > 0, "chat header block not found");
  assert.ok(
    header.length < chat.length / 2,
    "chat header slice ran past its end anchor and no longer scopes these assertions",
  );

  // On a detail page the chat is a peer of Run history and Runtime trend, so
  // its header carries the same surface and accent those bands use, read from
  // the one helper rather than restated per surface.
  assert.match(header, /sectionBandSx\(\)/);
  assert.match(band, /sectionBandSx\(\)/);
  assert.match(header, /minHeight: overviewLayout\.categoryBandMinHeight/);
  assert.match(header, /overviewTypography\.categoryHeading/);

  // The asymmetric bubble was the strongest consumer-chat signal on a page
  // that squares every other container.
  assert.doesNotMatch(chat, /borderRadius: "10px 10px 3px 10px"/);

  // Chips were the one control the radius reset did not reach, so they
  // rendered as pills beside the squared chips everywhere else. The reset is
  // gone now: the theme token itself is the page radius, so no surface has to
  // drag MUI primitives back to it.
  assert.doesNotMatch(chat, /MuiChip-root": \{\s*borderRadius/s);
  assert.match(source("src/theme/createAppTheme.ts"), /shape: \{ borderRadius: 4 \}/);

  // The attempts counter belongs in the band metadata slot, not floating at
  // the bottom of the panel, and must not appear in both.
  assert.match(header, /detailAppearance && turnUsage/);
  assert.match(chat, /turnUsage && !detailAppearance/);

  // A `border-*` shorthand carries no color, so CSS resets that edge to
  // currentColor. `sectionBandSx()` contributes `borderColor`, and a duplicate
  // key keeps its first position, so the spread is where the color is really
  // emitted and any shorthand written after it lands last, painting a
  // near-white rule in dark mode.
  const bandSx = header.slice(header.indexOf("<Stack"), header.indexOf("{/* The heading wraps"));
  const spreadAt = bandSx.indexOf("...sectionBandSx()");
  const explicitColorAt = bandSx.indexOf('borderColor: "divider"');
  const topAt = bandSx.indexOf("borderTop:");
  const bottomAt = bandSx.indexOf("borderBottom:");
  assert.ok(
    spreadAt > 0 && explicitColorAt > 0 && topAt > 0 && bottomAt > 0,
    "chat header band border rules not found",
  );
  assert.doesNotMatch(
    bandSx.slice(Math.min(spreadAt, explicitColorAt)),
    /border(Top|Right|Bottom|Left|(Block|Inline)(Start|End)?)?:/,
    "a border shorthand after borderColor resets that edge to currentColor",
  );

  // A heading is not valid phrasing content inside a button, so the heading
  // wraps the toggle rather than sitting within it.
  assert.match(header, /component=\{detailAppearance \? "h3" : "div"\}[\s\S]*<ButtonBase/);

  // Every container in the conversation squares to the page's single radius,
  // including the ones that used to round themselves off a detail page.
  assert.doesNotMatch(chat, /borderRadius: "\d+px"/);
});

test("the severity chip is suppressed exactly where a header already states it", () => {
  const panel = source("src/components/AiAnalysisPanel.tsx");
  const testDetail = source("src/pages/TestDetailPage.tsx");
  const table = source("src/components/TestCaseTable.tsx");
  const buildFailure = source("src/components/BuildFailurePanel.tsx");

  assert.match(panel, /severityInHeader = false/);
  assert.match(panel, /\{!severityInHeader && \(\s*<Chip/);

  // The test detail band always leads with the severity, so it always opts out.
  assert.match(testDetail, /severity\} severity ·/);
  assert.match(testDetail, /severityInHeader/);

  // The build failure band states severity only once an analysis has landed,
  // so it opts out conditionally, and carries the severity on BOTH breakpoints
  // so suppressing the chip cannot leave mobile without a severity signal.
  assert.match(buildFailure, /const headerSeverity = state === "succeeded"/);
  assert.match(buildFailure, /severityInHeader=\{Boolean\(headerSeverity\)\}/);
  assert.match(buildFailure, /mobileMetadata=\{`Build \$\{run\.build_id\}\$\{headerSeverity \? ` · \$\{headerSeverity\}` : ""\}`\}/);

  // The inline table row has no header of its own, so it keeps the chip.
  assert.doesNotMatch(table, /severityInHeader/);
});


test("analysis prose preserves step breaks and cleans evidence placeholders", () => {
  const section = source("src/components/BriefingSection.tsx");
  const richText = source("src/components/RichText.tsx");

  assert.match(section, /whiteSpace: "pre-line"/);
  assert.match(richText, /normalizeAnalysisProse/);
  assert.match(richText, /\(cited evidence\)/);
  assert.match(richText, /\(\?:\[\\w\.-\]\+\\\/\)\*/);
});
