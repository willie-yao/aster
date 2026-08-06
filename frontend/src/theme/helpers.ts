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
