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
import HistoryOutlined from "@mui/icons-material/HistoryOutlined";
import type { SvgIconComponent } from "@mui/icons-material";
import { useEffect, useLayoutEffect, useRef, useState } from "react";
import type { PointerEvent, ReactNode } from "react";
import { Link as RouterLink, useLocation } from "react-router-dom";
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
  investigations: HistoryOutlined,
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
    bgcolor: "transparent",
    transition: "color 140ms ease, background-color 140ms ease",
    "@media (prefers-reduced-motion: reduce)": { transition: "none" },
    "&:hover": {
      color: "text.primary",
      bgcolor: active ? "transparent" : "surface.containerHigh",
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
      sx={{ fontSize: "0.6875rem", fontWeight: 600, lineHeight: 1.2, letterSpacing: "0.01em" }}
    >
      {children}
    </Typography>
  );
}

const glideEase = "cubic-bezier(0.22, 1, 0.36, 1)";

type ItemBox = { top: number; height: number };

function boxOf(item: HTMLElement): ItemBox {
  return { top: item.offsetTop, height: item.offsetHeight };
}

/**
 * Tracks where the current and hovered items sit in the rail list so the
 * indicator pills can glide between them. `layoutKey` changes whenever the
 * active destination or the destination set does.
 */
function useGlideTargets(layoutKey: string) {
  const listRef = useRef<HTMLDivElement>(null);
  const [current, setCurrent] = useState<ItemBox | null>(null);
  const [hovered, setHovered] = useState<ItemBox | null>(null);
  const [placed, setPlaced] = useState(false);

  // Measure before paint so the pill never shows in a stale spot, and again on
  // resize, such as zoom or a late font swap.
  useLayoutEffect(() => {
    const list = listRef.current;
    if (!list) return;
    const update = () => {
      const item = list.querySelector<HTMLElement>("[aria-current]");
      setCurrent(item ? boxOf(item) : null);
    };
    update();
    const observer = typeof ResizeObserver === "undefined" ? null : new ResizeObserver(update);
    observer?.observe(list);
    return () => observer?.disconnect();
  }, [layoutKey]);

  // Enable transitions only after the first placement, so the pill does not fly in on load.
  useEffect(() => {
    if (!current || placed) return;
    const frame = requestAnimationFrame(() => setPlaced(true));
    return () => cancelAnimationFrame(frame);
  }, [current, placed]);

  const onPointerOver = (event: PointerEvent) => {
    const item = (event.target as HTMLElement).closest<HTMLElement>("[data-rail-item]");
    if (item) setHovered(boxOf(item));
  };
  const onPointerLeave = () => setHovered(null);

  return { listRef, current, hovered, placed, onPointerOver, onPointerLeave };
}

const pillSx = {
  position: "absolute",
  top: 0,
  left: 6,
  right: 6,
  borderRadius: 1.5,
  pointerEvents: "none",
} as const;

/** Vertical navigation rail. Layout mounts it from `md` up; below that it mounts NavBottomBar instead. */
export function NavRail({
  destinations,
  homeLabel,
  brandLabel,
  search,
  controls,
}: {
  destinations: NavDestination[];
  homeLabel: string;
  brandLabel?: string;
  search?: ReactNode;
  controls?: ReactNode;
}) {
  const signal = destinations.filter((d) => d.scope === "signal");
  const operator = destinations.filter((d) => d.scope === "operator");
  const { pathname } = useLocation();
  const layoutKey = destinations.map((d) => `${d.id}:${d.active}`).join();
  const { listRef, current, hovered, placed, onPointerOver, onPointerLeave } = useGlideTargets(layoutKey);
  const glide = (seconds: number) => `transform ${seconds}s ${glideEase}`;
  // The hover pill rests under the current item while the pointer is elsewhere.
  const ghost = hovered ?? current;

  const item = (d: NavDestination) => (
    <ButtonBase
      key={d.id}
      data-rail-item
      component={RouterLink}
      to={d.to}
      title={d.title}
      aria-current={d.active ? (pathname === d.to ? "page" : "true") : undefined}
      sx={{
        ...itemSx(d.active),
        zIndex: 1,
        height: 52,
        width: "calc(100% - 12px)",
        mx: 0.75,
        borderRadius: 1.5,
        color: d.active ? "primary.contrastText" : "text.secondary",
        transition: `color 260ms ${glideEase}`,
        "& .MuiTypography-root": { fontWeight: d.active ? 700 : 600 },
        "&:hover": { color: d.active ? "primary.contrastText" : "text.primary", bgcolor: "transparent" },
        // Outside the pill, so the ring stays visible on the primary fill.
        "&.Mui-focusVisible": { outline: "2px solid", outlineColor: "primary.main", outlineOffset: 2 },
      }}
    >
      <NavIcon id={d.id} />
      <NavLabel>{d.label}</NavLabel>
    </ButtonBase>
  );

  return (
    <Box
      component="nav"
      aria-label="Primary"
      sx={{
        display: "flex",
        flexDirection: "column",
        flexShrink: 0,
        width: RAIL_WIDTH,
        position: "sticky",
        top: 0,
        height: "100vh",
        py: 1.5,
        overflow: "hidden",
        bgcolor: (theme) => (theme.vars ?? theme).palette.surface.container,
        borderRight: "1px solid",
        borderColor: "divider",
      }}
    >
      <MuiLink
        component={RouterLink}
        to="/"
        // The accessible name must contain the visible label (WCAG 2.5.3), so
        // it is built from the short name shown here, not the full title.
        aria-label={brandLabel ? `${brandLabel} home` : homeLabel}
        title={homeLabel}
        underline="none"
        color="inherit"
        sx={{
          display: "flex",
          flexDirection: "column",
          alignItems: "center",
          gap: 0.25,
          flexShrink: 0,
          mb: 1,
          transition: "opacity 150ms ease",
          "@media (prefers-reduced-motion: reduce)": { transition: "none" },
          "&:hover": { opacity: 0.8 },
        }}
      >
        <AsterMark size={28} />
        {brandLabel && (
          <Typography
            component="span"
            sx={{
              fontSize: "0.6875rem",
              fontWeight: 700,
              letterSpacing: "0.02em",
              maxWidth: "100%",
              px: 0.5,
              overflow: "hidden",
              textOverflow: "ellipsis",
              whiteSpace: "nowrap",
            }}
          >
            {brandLabel}
          </Typography>
        )}
      </MuiLink>

      {search && (
        <>
          <Box sx={{ flexShrink: 0, display: "flex" }}>{search}</Box>
          <Divider sx={{ mx: 1.75, my: 1 }} />
        </>
      )}

      {/* The destination list scrolls so a short viewport cannot trap an item. */}
      <Box
        ref={listRef}
        onPointerOver={onPointerOver}
        onPointerLeave={onPointerLeave}
        sx={{
          flex: 1,
          minHeight: 0,
          overflowY: "auto",
          scrollbarWidth: "thin",
          position: "relative",
          display: "grid",
          alignContent: "start",
          rowGap: 0.75,
          py: 0.5,
          "@media (prefers-reduced-motion: reduce)": {
            "&, & *": { transition: "none !important" },
          },
        }}
      >
        <Box
          aria-hidden="true"
          sx={{
            ...pillSx,
            bgcolor: "surface.containerHighest",
            height: ghost?.height ?? 0,
            transform: `translateY(${ghost?.top ?? 0}px)`,
            opacity: hovered ? 1 : 0,
            transition: placed ? `${glide(0.27)}, opacity 180ms ease` : "none",
          }}
        />
        <Box
          aria-hidden="true"
          sx={{
            ...pillSx,
            bgcolor: "primary.main",
            height: current?.height ?? 0,
            transform: `translateY(${current?.top ?? 0}px)`,
            opacity: current ? 1 : 0,
            transition: placed ? glide(0.45) : "none",
          }}
        />
        {signal.map(item)}
        {operator.length > 0 && (
          <>
            <Divider sx={{ mx: 1.75, my: 0.5 }} />
            {operator.map(item)}
          </>
        )}
      </Box>

      {controls && (
        <Box
          sx={{
            flexShrink: 0,
            pt: 1,
            display: "flex",
            flexDirection: "column",
            alignItems: "center",
            gap: 0.5,
            borderTop: "1px solid",
            borderColor: "divider",
          }}
        >
          {controls}
        </Box>
      )}
    </Box>
  );
}

/**
 * Bottom tab bar for small viewports, where a 76px rail would take a fifth of
 * the screen. Layout mounts it below `md`. Capacity is five destinations, which
 * is the most the capability flags can produce.
 */
export function NavBottomBar({ destinations }: { destinations: NavDestination[] }) {
  const { pathname } = useLocation();
  return (
    <Box
      component="nav"
      aria-label="Primary"
      sx={{
        display: "flex",
        position: "fixed",
        left: 0,
        right: 0,
        bottom: 0,
        zIndex: (theme) => theme.zIndex.appBar,
        bgcolor: (theme) => (theme.vars ?? theme).palette.surface.container,
        borderTop: "1px solid",
        borderColor: "divider",
        pb: "env(safe-area-inset-bottom)",
      }}
    >
      {destinations.map((d) => (
        <ButtonBase
          key={d.id}
          component={RouterLink}
          to={d.to}
          title={d.title}
          aria-current={d.active ? (pathname === d.to ? "page" : "true") : undefined}
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
