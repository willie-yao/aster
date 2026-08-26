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
  // Both navs are rendered, so each must carry the full destination name as a
  // description. The visible label stays the accessible name so speech control
  // can target what it reads (WCAG 2.5.3).
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
  // Signed out, the labelled "Sign in" button becomes an icon button.
  assert.match(profile, /if \(compact\) \{[\s\S]*?aria-label="Sign in"/);
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

test("run history remains contained on narrow detail pages", () => {
  const timeline = source("src/components/RunHistory.tsx");
  const jobDetail = source("src/pages/JobDetailPage.tsx");

  assert.match(timeline, /width: "100%"[\s\S]*minWidth: 0[\s\S]*maxWidth: "100%"[\s\S]*overflowX: "auto"/);
  assert.match(timeline, /width: "max-content"[\s\S]*minWidth: "100%"/);
  assert.match(jobDetail, /<RunHistory[\s\S]*metadata=\{`\$\{historyRuns\.length\} recent/);
  assert.match(jobDetail, /gridTemplateColumns: \{ xs: "minmax\(0, 1fr\)"[\s\S]*minWidth: 0/);
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
