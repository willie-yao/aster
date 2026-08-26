const monoFontFamily =
  'ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, "Liberation Mono", monospace';

// Overview-only typography keeps the incident briefing readable without
// changing type scales on detail, trace, or action pages.
export const overviewTypography = {
  eyebrow: {
    fontSize: "13px",
    lineHeight: "18px",
    fontWeight: 650,
  },
  pageHeadline: {
    fontSize: "27px",
    lineHeight: "34px",
    fontWeight: 700,
    letterSpacing: "-0.018em",
  },
  majorHeading: {
    fontSize: "18px",
    lineHeight: "26px",
    fontWeight: 680,
  },
  categoryHeading: {
    fontSize: "16px",
    lineHeight: "24px",
    fontWeight: 680,
  },
  subsectionHeading: {
    fontSize: "13.5px",
    lineHeight: "20px",
    fontWeight: 700,
  },
  primaryBody: {
    fontSize: "15px",
    lineHeight: "22px",
  },
  mobileFeaturedBody: {
    fontSize: "16px",
    lineHeight: "24px",
  },
  secondaryBody: {
    fontSize: "14px",
    lineHeight: "21px",
  },
  jobIdentifier: {
    fontFamily: monoFontFamily,
    fontSize: "14px",
    lineHeight: "20px",
    fontWeight: 600,
    letterSpacing: 0,
  },
  description: {
    fontSize: "13px",
    lineHeight: "19px",
  },
  data: {
    fontFamily: monoFontFamily,
    fontSize: "13px",
    lineHeight: "19px",
    fontWeight: 500,
    letterSpacing: 0,
    fontFeatureSettings: '"tnum" 1, "cv01" 1',
  },
  tableHeading: {
    fontSize: "13px",
    lineHeight: "18px",
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

/**
 * The header band shared by detail sections and action dialogs: a raised
 * surface ruled off from its body and marked with an accent edge. Keeping one
 * definition is what stops an overlay from reading as a different product.
 */
export function sectionBandSx(accent: "primary" | "warning" = "primary") {
  return {
    bgcolor: "surface.containerHigh",
    borderColor: "divider",
    boxShadow: `inset 3px 0 0 var(--mui-palette-${accent}-main)`,
  } as const;
}

/**
 * Gutter shared by every action dialog's title, content, and actions. Dialogs
 * sit over dense detail pages, so they stay close to the page's own rhythm.
 */
export const dialogGutter = 2.5;

/** Squared, flat dialog surface. Radius comes from the theme. */
export const dialogPaperSx = {
  border: "1px solid",
  borderColor: "divider",
  backgroundImage: "none",
} as const;

/**
 * The one floating-overlay surface: popovers and menus that genuinely detach
 * from the page. A detached layer needs the separation no border can give and a
 * softer edge than an inline one, so it takes the overlay shadow and 8px rather
 * than the console's 4px. Nothing inline may borrow it.
 */
export const overlayPaperSx = {
  border: "1px solid",
  borderColor: "divider",
  borderRadius: "8px",
  bgcolor: "surface.container",
  backgroundImage: "none",
  boxShadow: "0 18px 50px rgba(0, 0, 0, 0.28)",
} as const;

/**
 * Interactive target from the buttons rule: 36px under a pointer, 44px wherever
 * touch is available. Keyed to the input device rather than a breakpoint, so a
 * landscape phone still gets a touch-sized target despite its width.
 */
export const touchTargetSx = {
  minWidth: 36,
  minHeight: 36,
  "@media (any-pointer: coarse)": { minWidth: 44, minHeight: 44 },
} as const;

/**
 * Operator filter field. Sizing follows the Inputs rule: 44px tall, 14px on a
 * pointer, 16px wherever touch is available. The 16px is keyed to the input
 * device rather than a breakpoint because iOS force-zooms a focused input below
 * it and a landscape phone is wider than the `sm` breakpoint. The monospace
 * family stays: these fields hold identifiers, and the rule governs size only.
 */
export const filterFieldSx = {
  minWidth: 0,
  "& .MuiOutlinedInput-root": {
    minHeight: 44,
    borderRadius: "4px",
    bgcolor: "surface.containerLow",
    ...overviewTypography.data,
    fontSize: "14px",
    "@media (any-pointer: coarse)": { fontSize: "16px" },
  },
} as const;
