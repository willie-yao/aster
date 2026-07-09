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

/** Status-driven accent for glass blocks: an edge bar plus tinted border and
 * glow. Failing draws the eye with a colored border and drop glow; passing and
 * flaky stay calm with only the edge bar. */
export interface StatusAccent {
  bar: string;
  border: string;
  glow: string;
  hoverBorder: string;
  hoverGlow: string;
}

export function statusAccent(theme: Theme, status: DashboardStatus): StatusAccent {
  const p = (theme.vars ?? theme).palette;
  const color = statusToMuiColor(status);
  if (color === "default") {
    return {
      bar: p.divider,
      border: p.divider,
      glow: "none",
      hoverBorder: soft(theme, "primary", 0.5),
      hoverGlow: `0 10px 34px -14px ${soft(theme, "primary", 0.4)}`,
    };
  }
  const main = p[color].main;
  const emphasize = color === "error";
  return {
    bar: main,
    border: emphasize ? soft(theme, color, 0.42) : p.divider,
    glow: emphasize ? `0 8px 30px -14px ${soft(theme, color, 0.6)}` : "none",
    hoverBorder: soft(theme, color, 0.6),
    hoverGlow: `0 12px 38px -14px ${soft(theme, color, emphasize ? 0.65 : 0.42)}`,
  };
}
