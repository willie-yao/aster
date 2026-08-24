import Box from "@mui/material/Box";
import ButtonBase from "@mui/material/ButtonBase";
import Divider from "@mui/material/Divider";
import MuiLink from "@mui/material/Link";
import Typography from "@mui/material/Typography";
import GridView from "@mui/icons-material/GridViewOutlined";
import TrendingDown from "@mui/icons-material/TrendingDownOutlined";
import AltRoute from "@mui/icons-material/AltRouteOutlined";
import MonitorHeart from "@mui/icons-material/MonitorHeartOutlined";
import Paid from "@mui/icons-material/PaidOutlined";
import type { SvgIconComponent } from "@mui/icons-material";
import { Link as RouterLink } from "react-router-dom";
import { AsterMark } from "./AsterMark";
import type { NavDestination } from "../lib/navigation";

export const RAIL_WIDTH = 76;
export const BOTTOM_BAR_HEIGHT = 60;

// Each destination gets a visually distinct glyph. Trends, Health, and Usage
// are all chart-shaped concepts, so they deliberately use a falling line, an
// ECG trace, and a coin rather than three variations of the same chart.
const ICONS: Record<string, SvgIconComponent> = {
  overview: GridView,
  flaky: TrendingDown,
  "pull-requests": AltRoute,
  "analysis-health": MonitorHeart,
  "ai-usage": Paid,
};

function NavIcon({ id }: { id: string }) {
  const Icon = ICONS[id] ?? GridView;
  return <Icon aria-hidden="true" sx={{ fontSize: 20 }} />;
}

function itemSx(active: boolean) {
  return {
    position: "relative" as const,
    display: "flex",
    flexDirection: "column" as const,
    alignItems: "center",
    justifyContent: "center",
    gap: 0.375,
    color: active ? "primary.main" : "text.secondary",
    bgcolor: active ? "var(--nav-active-bg)" : "transparent",
    transition: "color 140ms ease, background-color 140ms ease",
    "@media (prefers-reduced-motion: reduce)": { transition: "none" },
    "&:hover": {
      color: "text.primary",
      bgcolor: active ? "var(--nav-active-bg)" : "surface.containerHigh",
    },
    "&.Mui-focusVisible": {
      outline: "2px solid",
      outlineColor: "primary.main",
      outlineOffset: -2,
    },
  };
}

function NavLabel({ children }: { children: string }) {
  return (
    <Typography
      component="span"
      sx={{ fontSize: "0.625rem", fontWeight: 600, lineHeight: 1.2, letterSpacing: "0.01em" }}
    >
      {children}
    </Typography>
  );
}

/** Vertical navigation rail. Hidden below `md`, where NavBottomBar takes over. */
export function NavRail({
  destinations,
  homeLabel,
}: {
  destinations: NavDestination[];
  homeLabel: string;
}) {
  const signal = destinations.filter((d) => d.scope === "signal");
  const operator = destinations.filter((d) => d.scope === "operator");

  return (
    <Box
      component="nav"
      aria-label="Primary"
      sx={{
        display: { xs: "none", md: "flex" },
        flexDirection: "column",
        flexShrink: 0,
        width: RAIL_WIDTH,
        position: "sticky",
        top: 0,
        height: "100vh",
        py: 1.5,
        bgcolor: (theme) => (theme.vars ?? theme).palette.surface.container,
        borderRight: "1px solid",
        borderColor: "divider",
        "--nav-active-bg": "color-mix(in srgb, var(--mui-palette-primary-main) 14%, transparent)",
      }}
    >
      <MuiLink
        component={RouterLink}
        to="/"
        aria-label={homeLabel}
        underline="none"
        sx={{
          display: "flex",
          alignItems: "center",
          justifyContent: "center",
          height: 40,
          mb: 1,
          transition: "opacity 150ms ease",
          "@media (prefers-reduced-motion: reduce)": { transition: "none" },
          "&:hover": { opacity: 0.8 },
        }}
      >
        <AsterMark size={28} />
      </MuiLink>

      {signal.map((d) => (
        <ButtonBase
          key={d.id}
          component={RouterLink}
          to={d.to}
          title={d.title}
          aria-current={d.active ? "page" : undefined}
          sx={{ ...itemSx(d.active), height: 54, width: "100%" }}
        >
          {d.active && <ActiveBar />}
          <NavIcon id={d.id} />
          <NavLabel>{d.label}</NavLabel>
        </ButtonBase>
      ))}

      {operator.length > 0 && (
        <>
          <Divider sx={{ mx: 1.75, my: 1 }} />
          {operator.map((d) => (
            <ButtonBase
              key={d.id}
              component={RouterLink}
              to={d.to}
              title={d.title}
              aria-current={d.active ? "page" : undefined}
              sx={{ ...itemSx(d.active), height: 54, width: "100%" }}
            >
              {d.active && <ActiveBar />}
              <NavIcon id={d.id} />
              <NavLabel>{d.label}</NavLabel>
            </ButtonBase>
          ))}
        </>
      )}
    </Box>
  );
}

function ActiveBar() {
  return (
    <Box
      aria-hidden="true"
      sx={{
        position: "absolute",
        left: 0,
        top: 6,
        bottom: 6,
        width: 3,
        borderRadius: "0 3px 3px 0",
        bgcolor: "primary.main",
      }}
    />
  );
}

/**
 * Bottom tab bar for small viewports, where a 76px rail would take a fifth of
 * the screen. Capacity is five destinations, which is the most the capability
 * flags can produce.
 */
export function NavBottomBar({ destinations }: { destinations: NavDestination[] }) {
  return (
    <Box
      component="nav"
      aria-label="Primary"
      sx={{
        display: { xs: "flex", md: "none" },
        position: "fixed",
        left: 0,
        right: 0,
        bottom: 0,
        zIndex: (theme) => theme.zIndex.appBar,
        bgcolor: (theme) => (theme.vars ?? theme).palette.surface.container,
        borderTop: "1px solid",
        borderColor: "divider",
        pb: "env(safe-area-inset-bottom)",
        "--nav-active-bg": "transparent",
      }}
    >
      {destinations.map((d) => (
        <ButtonBase
          key={d.id}
          component={RouterLink}
          to={d.to}
          title={d.title}
          aria-current={d.active ? "page" : undefined}
          sx={{ ...itemSx(d.active), flex: 1, minWidth: 0, height: BOTTOM_BAR_HEIGHT }}
        >
          {d.active && (
            <Box
              aria-hidden="true"
              sx={{
                position: "absolute",
                top: 0,
                left: "22%",
                right: "22%",
                height: 2.5,
                borderRadius: "0 0 3px 3px",
                bgcolor: "primary.main",
              }}
            />
          )}
          <NavIcon id={d.id} />
          <NavLabel>{d.label}</NavLabel>
        </ButtonBase>
      ))}
    </Box>
  );
}
