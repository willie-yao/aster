import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { test } from "node:test";

function source(path: string): string {
  return readFileSync(resolve(process.cwd(), path), "utf8");
}

test("mobile branding link keeps an accessible home name", () => {
  const layout = source("src/components/Layout.tsx");
  const rail = source("src/components/NavRail.tsx");
  const navigation = source("src/lib/navigation.ts");

  assert.match(layout, /<MuiLink[\s\S]*?aria-label=\{homeLabel\}[\s\S]*?>/);
  assert.match(
    layout,
    /<Typography[\s\S]*?display: \{ xs: "none", sm: "block" \}/,
  );
  // Layout mounts one nav per breakpoint, but both components must carry the
  // full destination name as a description. The visible label stays the
  // accessible name so speech control can target what it reads (WCAG 2.5.3).
  assert.equal(rail.match(/title=\{d\.title\}/g)?.length, 3);
  assert.doesNotMatch(rail, /aria-label=\{d\.title\}/);
  assert.match(navigation, /title: "Failure Trends"/);
  assert.match(navigation, /title: "Analysis Health"/);
  assert.match(navigation, /title: "AI Usage"/);
});

test("primary navigation swaps between a rail and a bottom bar without a gap", () => {
  const layout = source("src/components/Layout.tsx");
  const rail = source("src/components/NavRail.tsx");
  const profile = source("src/components/ProfileMenu.tsx");

  // Operator destinations render only a sign-in wall without a session, so the
  // rail must derive access from auth rather than the deployment flags alone.
  assert.match(layout, /operatorAccess: auth\.status === "authenticated"/);
  // Exactly one primary nav is mounted at any width: the rail from md up, the
  // bottom bar below it. Mounting decides it, so neither is in the DOM twice.
  assert.match(layout, /\{railHostsControls && \(\s*<NavRail/);
  assert.match(layout, /\{!railHostsControls && <NavBottomBar/);
  // The fixed bottom bar must not cover the end of the page.
  assert.match(layout, /BOTTOM_BAR_HEIGHT\}px \+ env\(safe-area-inset-bottom\)/);
  assert.match(rail, /pb: "env\(safe-area-inset-bottom\)"/);
  assert.match(layout, /href="#main-content"[\s\S]*Skip to main content/);
  assert.match(layout, /id="main-content"[\s\S]*tabIndex=\{-1\}/);
  assert.match(profile, /width: \{ xs: 44, sm: 36 \}/);
  assert.match(profile, /height: \{ xs: 44, sm: 36 \}/);
});

test("search and account controls are placed once, never mounted twice", () => {
  const layout = source("src/components/Layout.tsx");
  const rail = source("src/components/NavRail.tsx");

  // SearchBar owns a global Cmd+K listener, so two mounts would register two
  // handlers and fight over focus. The breakpoint decides placement at render
  // time instead of hiding a second copy with CSS.
  assert.match(layout, /const railHostsControls = useMediaQuery\(theme\.breakpoints\.up\("md"\)\)/);
  assert.match(layout, /search=\{<SearchBar variant="rail" \/>\}/);
  assert.match(layout, /controls=\{controls\}/);
  // The top bar exists only when the rail is not hosting those controls.
  assert.match(layout, /\{!railHostsControls && \(\s*<AppBar/);
  assert.equal(layout.match(/<SearchBar/g)?.length, 2);
  assert.equal(layout.match(/<ProfileMenu /g)?.length, 1);
  assert.equal(layout.match(/<FetchStatusControl /g)?.length, 1);
  // The rail renders each slot once; the bottom bar must not repeat them.
  assert.equal(rail.match(/\{search\}/g)?.length, 1);
  assert.equal(rail.match(/\{controls\}/g)?.length, 1);
});

test("rail-hosted controls stay inside the 76px column", () => {
  const layout = source("src/components/Layout.tsx");
  const profile = source("src/components/ProfileMenu.tsx");
  const fetchStatus = source("src/components/FetchStatus.tsx");

  // At md+ these render their labelled desktop form, which overflows the rail.
  assert.match(layout, /<FetchStatusControl response=\{fetchStatus\} iconOnly=\{railHostsControls\} \/>/);
  assert.match(layout, /<ProfileMenu compact=\{railHostsControls\} \/>/);
  assert.match(fetchStatus, /width: iconOnly \? 44 : \{ xs: 44, md: "auto" \}/);
  assert.match(fetchStatus, /"& \.MuiButton-endIcon": \{ display: iconOnly \? "none"/);
  // The status label is the widest part of the control and must be hidden too,
  // or a 44px button still overflows the rail.
  assert.match(fetchStatus, /display: iconOnly \? "none" : \{ xs: "none", md: "inline" \}/);
  // Signed out, the rail's sign-in stacks glyph over label like the rail's
  // destination items, and fills the column rather than sizing to its content.
  assert.match(profile, /if \(compact\) \{[\s\S]*?aria-label="Sign in with GitHub"/);
  assert.match(profile, /if \(compact\) \{[\s\S]*?width: "100%"/);
});

test("the rail keeps every destination reachable on a short viewport", () => {
  const rail = source("src/components/NavRail.tsx");

  // The rail is pinned to 100vh. Search, five destinations, and the footer can
  // exceed a short laptop viewport, so the destination list scrolls while the
  // brand and footer stay put.
  assert.match(rail, /height: "100vh"/);
  assert.match(rail, /overflow: "hidden"/);
  assert.match(rail, /flex: 1, minHeight: 0, overflowY: "auto"/);
  // The rail root, brand, search, and footer must not shrink or the list would
  // not be the element that scrolls.
  assert.equal(rail.match(/flexShrink: 0/g)?.length, 4);
});

test("the deployment title identifies the instance on its home page", () => {
  const dashboard = source("src/pages/DashboardPage.tsx");
  const layout = source("src/components/Layout.tsx");
  const rail = source("src/components/NavRail.tsx");

  // The rail only has room for the short name, so the full title lives in the
  // overview header. It is a sibling of the h1, not a competing heading.
  assert.match(dashboard, /component="p"[\s\S]*?\{manifest\.branding\.title\}/);
  assert.match(dashboard, /component="h1"[\s\S]*?Test Health Overview/);
  assert.doesNotMatch(dashboard, /component="h1"[^>]*>\s*\{manifest\.branding\.title\}/);
  // short_name is optional, so the rail always has something to show.
  assert.match(layout, /brandLabel=\{manifest\.short_name \?\? manifest\.name\}/);
  // The accessible name must contain the visible short name (WCAG 2.5.3).
  assert.match(rail, /aria-label=\{brandLabel \? `\$\{brandLabel\} home` : homeLabel\}/);
});

test("mobile inputs clear the iOS zoom threshold and keep a 44px target", () => {
  const search = source("src/components/SearchBar.tsx");
  const filters = source("src/components/OverviewFilters.tsx");

  // iOS and iPadOS Safari force-zoom a focused input under 16px, and a hybrid
  // iPad reports a fine primary pointer while its screen still takes a finger.
  assert.match(search, /"@media \(any-pointer: coarse\)": \{ fontSize: "16px" \}/);
  assert.match(search, /height: \{ xs: 44, md: 36 \}/);
  // The Select renders an inner box that takes the tap, and MUI's own rule wins
  // over a class selector, so the 44px target is set inline on that box. It
  // stays a block box so the selected value keeps its ellipsis.
  assert.match(filters, /SelectDisplayProps=\{\{[\s\S]*?minHeight: 44/);
  assert.doesNotMatch(filters, /SelectDisplayProps=\{\{[\s\S]*?display: "flex"/);
  assert.match(filters, /height: 44/);
});

test("analysis chat contains multiline composer growth inside the transcript", () => {
  const chat = source("src/components/AnalysisChat.tsx");
  const content = chat.slice(chat.indexOf("id={chatContentId}"), chat.indexOf("<ChatFixDialog"));

  assert.match(chat, /const clearComposer = useCallback\(\(\) => \{\s*setQuestion\(""\);\s*setDraftContentHeight\(null\);\s*\}, \[\]\)/);
  assert.match(content, /display: "flex",\s*flexDirection: "column"/);
  assert.match(content, /height: draftContentHeight \?\? "auto"/);
  assert.match(content, /maxHeight: \{ xs: "min\(72vh, 680px\)", sm: "min\(80vh, 800px\)" \}/);
  assert.match(content, /overflow: "hidden"/);
  assert.match(content, /maxHeight: \{ xs: "min\(62vh, 560px\)", sm: "min\(70vh, 680px\)" \},\s*flex: "1 1 auto",\s*minHeight: 0,\s*overflowY: "auto"/);
  assert.match(content, /setDraftContentHeight\(\(current\) =>\s*current \?\? chatContentRef\.current\?\.getBoundingClientRect\(\)\.height \?\? null\s*\)/);
});

test("truncated ledger text recovers by touch and keyboard, not hover alone", () => {
  const ledger = source("src/components/JobHealthTable.tsx");

  // A title attribute is mouse-only, and the job description is not repeated
  // anywhere else in the interface. The row's one focusable element carries the
  // recovery through a Tooltip, which opens on hover, focus, and long press.
  assert.match(ledger, /<Tooltip[\s\S]*?title=\{recovery\}/);
  assert.doesNotMatch(ledger, /title=\{displayName\}/);
  assert.doesNotMatch(ledger, /title=\{job\.description\}/);
  assert.match(ledger, /const description = compact \? "" : job\.description/);
  // The description must describe the link, not rename it. The tooltip stays
  // hoverable for a pointer (WCAG 1.4.13) but clears on touch release, so it
  // cannot sit over the next row's tap target.
  assert.match(ledger, /describeChild=\{Boolean\(description\)\}/);
  assert.doesNotMatch(ledger, /disableInteractive/);
  assert.match(ledger, /leaveTouchDelay=\{0\}/);
});

test("run history remains contained on narrow detail pages", () => {
  const timeline = source("src/components/RunHistory.tsx");
  const jobDetail = source("src/pages/JobDetailPage.tsx");

  assert.match(timeline, /width: "100%"[\s\S]*minWidth: 0[\s\S]*maxWidth: "100%"[\s\S]*overflowX: "auto"/);
  assert.match(timeline, /width: "max-content"[\s\S]*minWidth: "100%"/);
  assert.match(jobDetail, /<RunHistory[\s\S]*metadata=\{`\$\{historyRuns\.length\} recent/);
  assert.match(jobDetail, /gridTemplateColumns: \{\s*xs: "minmax\(0, 1fr\)"[\s\S]*minWidth: 0/);
});

test("test analysis and run history reflow at mobile and zoom widths", () => {
  const timeline = source("src/components/RunHistory.tsx");
  const testDetail = source("src/pages/TestDetailPage.tsx");
  const analysis = source("src/components/AiAnalysisPanel.tsx");
  const pattern = source("src/components/PatternBanner.tsx");
  const briefing = source("src/components/AnalysisBriefing.tsx");

  assert.match(testDetail, /gridTemplateColumns: \{ xs: "minmax\(0, 1fr\)"/);
  assert.match(testDetail, /display: "grid",[\s\S]*minWidth: 0/);
  assert.match(analysis, /component="section"[\s\S]*minWidth: 0[\s\S]*maxWidth: "100%"[\s\S]*overflowWrap: "anywhere"/);
  assert.match(pattern, /<AnalysisBriefing[\s\S]*mobileSynopsis/);
  assert.doesNotMatch(pattern, /className="ai-aurora"/);
  assert.match(briefing, /component="section"[\s\S]*minWidth: 0[\s\S]*maxWidth: "100%"/);
  assert.match(briefing, /mobileNotice[\s\S]*DetailSectionBand title="Full analysis"/);
  assert.match(pattern, /<CausalGroupNextStep[\s\S]*group=\{group\}/);
  assert.match(testDetail, /overflowX: "clip"/);
  assert.match(testDetail, /failure_location[\s\S]*overflowWrap: "anywhere"/);
  assert.match(timeline, /width: "100%"[\s\S]*overflowX: "auto"[\s\S]*overflowY: "hidden"/);
  assert.match(timeline, /<Tooltip title=\{tooltip\}>/);
  assert.match(timeline, /width: \{ xs: 44, sm: 32 \}/);
  assert.match(timeline, /height: \{ xs: 44, sm: 32 \}/);
  assert.match(timeline, /formatAccessibleDate\(run\.started\)/);
});

test("each ledger grid fits the viewport at the width it engages", () => {
  const ledger = source("src/components/JobHealthTable.tsx");
  const rail = source("src/components/NavRail.tsx");
  const railWidth = Number(/export const RAIL_WIDTH = (\d+)/.exec(rail)?.[1]);
  assert.ok(railWidth);

  // Every track's smallest possible width: a plain px track, or the floor of a
  // minmax. The grid can never render narrower than their sum.
  const trackFloors = (name: string) => {
    const columns = new RegExp(`const ${name} = "([^"]+)"`).exec(ledger)?.[1] ?? "";
    assert.ok(columns, `${name} not found`);
    return (columns.match(/minmax\([^)]*\)|\S+/g) ?? []).map((track) => {
      const min = /^minmax\((\d+)px,/.exec(track) ?? /^(\d+)px$/.exec(track);
      assert.ok(min, `unrecognised track in ${name}: ${track}`);
      return Number(min[1]);
    });
  };

  const grids = [
    {
      name: "compactColumns",
      // The compact grid engages as soon as the desktop ledger mounts.
      engagesAt: Number(/const desktopQuery = "\(min-width: (\d+)px\)"/.exec(ledger)?.[1]),
      rows: [...ledger.matchAll(/gridTemplateColumns: compactColumns,[\s\S]{0,400}?columnGap: ([\d.]+),\s*px: ([\d.]+)/g)],
    },
    {
      name: "wideColumns",
      engagesAt: Number(/const wideBreakpoint = "@media \(min-width: (\d+)px\)"/.exec(ledger)?.[1]),
      rows: [...ledger.matchAll(/gridTemplateColumns: wideColumns,[\s\S]{0,400}?columnGap: ([\d.]+),\s*px: ([\d.]+)/g)],
    },
  ];

  for (const grid of grids) {
    assert.ok(grid.engagesAt, `${grid.name} has no breakpoint`);
    // Both the header row and the data row lay out on each grid; a row that
    // stops matching here would otherwise go unmeasured.
    const declared = ledger.match(new RegExp(`gridTemplateColumns: ${grid.name},`, "g"))?.length ?? 0;
    assert.equal(grid.rows.length, declared, `${grid.name}: ${declared} rows declared, ${grid.rows.length} measured`);

    const floors = trackFloors(grid.name);
    // MUI's Container pads 24px each side from sm up, and the rail is fixed.
    const available = grid.engagesAt - railWidth - 24 * 2;
    for (const [, gapUnits, padUnits] of grid.rows) {
      const needed =
        floors.reduce((a, b) => a + b, 0) + 8 * Number(gapUnits) * (floors.length - 1) + 8 * Number(padUnits) * 2;
      assert.ok(
        needed <= available,
        `${grid.name} needs ${needed}px but has ${available}px at ${grid.engagesAt}px, so the ledger overflows its row`,
      );
    }
  }
});
