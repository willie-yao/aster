import { alpha, type Theme } from "@mui/material/styles";

/** Semantic palette colors that have CSS-variable channel tokens. */
export type SoftColor = "primary" | "success" | "warning" | "error" | "info";

/**
 * Translucent background helper for tinted surfaces. Uses the palette's channel
 * token so it stays correct across light/dark, falling back to the alpha helper
 * if CSS variables are unavailable.
 */
export function soft(theme: Theme, color: SoftColor, opacity: number): string {
  const channel = theme.vars?.palette?.[color]?.mainChannel;
  if (channel) return `rgba(${channel} / ${opacity})`;
  return alpha(theme.palette[color].main, opacity);
}

/**
 * Label color for text sitting on its own accent's tint. Light mode reads from
 * the darker ramp, because `main` on its own tint falls under 4.5:1.
 */
export function accentLabelSx(theme: Theme, color: SoftColor) {
  return {
    color: `${color}.main`,
    ...theme.applyStyles("light", { color: `${color}.dark` }),
  };
}

/**
 * Tinted background plus label color for a status chip that labels rather
 * than acts, so it carries no outline.
 */
export function softChipSx(theme: Theme, color: SoftColor) {
  return {
    bgcolor: soft(theme, color, 0.16),
    ...accentLabelSx(theme, color),
  };
}

/**
 * MUI's Alert asserts by default, interrupting a screen reader. Only error and
 * warning earn that; information and success are announced politely.
 */
export function alertRole(severity: string): "alert" | "status" {
  return severity === "error" || severity === "warning" ? "alert" : "status";
}

/** Test/job status as reported in the data. Matching is case-insensitive. */
export type DashboardStatus = string;

/**
 * Map a dashboard status to the MUI color used by Chip/Alert/etc.
 *   PASSING/passed -> success, FAILING/failed -> error, FLAKY -> warning.
 */
export function statusToMuiColor(
  status: DashboardStatus,
): "success" | "warning" | "error" | "default" {
  switch (status.toUpperCase()) {
    case "PASSING":
    case "PASSED":
      return "success";
    case "FAILING":
    case "FAILED":
      return "error";
    case "FLAKY":
    case "RUNNING":
      return "warning";
    default:
      return "default";
  }
}

/**
 * Solid theme color for pass/fail dots and bars. Returns a CSS color string
 * from the active theme.
 */
export function dotColorFor(
  theme: Theme,
  passed: boolean,
  result?: string,
): string {
  const p = (theme.vars ?? theme).palette;
  if (result === "PENDING") return p.warning.main;
  return passed ? p.dot.pass : p.dot.fail;
}
