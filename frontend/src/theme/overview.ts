const monoFontFamily =
  'ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, "Liberation Mono", monospace';

// Overview-only typography keeps the incident briefing readable without
// changing type scales on detail, trace, or action pages.
export const overviewTypography = {
  eyebrow: {
    fontSize: "0.8125rem",
    lineHeight: "1.125rem",
    fontWeight: 650,
  },
  pageHeadline: {
    fontSize: "1.6875rem",
    lineHeight: "2.125rem",
    fontWeight: 700,
    letterSpacing: "-0.018em",
  },
  majorHeading: {
    fontSize: "1.125rem",
    lineHeight: "1.625rem",
    fontWeight: 680,
  },
  categoryHeading: {
    fontSize: "1rem",
    lineHeight: "1.5rem",
    fontWeight: 680,
  },
  subsectionHeading: {
    fontSize: "0.84375rem",
    lineHeight: "1.25rem",
    fontWeight: 700,
  },
  primaryBody: {
    fontSize: "0.9375rem",
    lineHeight: "1.375rem",
  },
  mobileFeaturedBody: {
    fontSize: "1rem",
    lineHeight: "1.5rem",
  },
  secondaryBody: {
    fontSize: "0.875rem",
    lineHeight: "1.3125rem",
  },
  jobIdentifier: {
    fontFamily: monoFontFamily,
    fontSize: "0.875rem",
    lineHeight: "1.25rem",
    fontWeight: 600,
    letterSpacing: 0,
  },
  description: {
    fontSize: "0.8125rem",
    lineHeight: "1.1875rem",
  },
  data: {
    fontFamily: monoFontFamily,
    fontSize: "0.8125rem",
    lineHeight: "1.1875rem",
    fontWeight: 500,
    letterSpacing: 0,
    fontFeatureSettings: '"tnum" 1, "cv01" 1',
  },
  tableHeading: {
    fontSize: "0.8125rem",
    lineHeight: "1.125rem",
    fontWeight: 700,
  },
} as const;

export const overviewLayout = {
  majorSectionGap: 3.5,
  majorBandMinHeight: 48,
  subsectionBandMinHeight: 36,
  categoryBandMinHeight: 44,
  ledgerRowMinHeight: 52,
} as const;
