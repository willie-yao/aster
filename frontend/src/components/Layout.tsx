import DarkMode from "@mui/icons-material/DarkMode";
import LightMode from "@mui/icons-material/LightMode";
import AppBar from "@mui/material/AppBar";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import Container from "@mui/material/Container";
import IconButton from "@mui/material/IconButton";
import MuiLink from "@mui/material/Link";
import SvgIcon from "@mui/material/SvgIcon";
import Toolbar from "@mui/material/Toolbar";
import Typography from "@mui/material/Typography";
import { useColorScheme } from "@mui/material/styles";
import { useEffect, useRef, useState } from "react";
import { Link as RouterLink, Outlet, useLocation } from "react-router-dom";
import { SearchBar } from "./SearchBar";
import { RouteErrorBoundary } from "./RouteErrorBoundary";
import { ProfileMenu } from "./ProfileMenu";
import { FetchStatusControl, FetchStatusStrip } from "./FetchStatus";
import { useManifest } from "../hooks/useManifest";
import { useCapabilities } from "../hooks/useCapabilities";
import { useFetchStatus } from "../hooks/useFetchStatus";
import { FetchStatusContext } from "../hooks/useSharedFetchStatus";
import { usePageDocumentTitle } from "../lib/pageMetadata";

// Primary top-nav tab with a restrained active indicator.
function NavTab({
  to,
  label,
  active,
  current,
}: {
  to: string;
  label: string;
  active: boolean;
  current: boolean;
}) {
  return (
    <Button
      component={RouterLink}
      to={to}
      size="small"
      aria-current={current ? "page" : undefined}
      sx={{
        position: "relative",
        px: { xs: 1, sm: 1.25 },
        py: 0.75,
        minWidth: 0,
        flexShrink: 0,
        minHeight: { xs: 44, xl: 36 },
        borderRadius: 0,
        fontSize: "0.8125rem",
        fontWeight: active ? 700 : 600,
        whiteSpace: "nowrap",
        color: active ? "text.primary" : "text.secondary",
        bgcolor: "transparent",
        transition: "color 140ms ease, background-color 140ms ease",
        "&::after": {
          content: '""',
          position: "absolute",
          insetInline: 8,
          bottom: 0,
          height: 2,
          bgcolor: active ? "primary.main" : "transparent",
        },
        "&:hover": {
          color: "text.primary",
          bgcolor: "surface.containerHigh",
        },
        "&.Mui-focusVisible": {
          outline: "2px solid",
          outlineColor: "primary.main",
          outlineOffset: -2,
        },
      }}
    >
      {label}
    </Button>
  );
}

export function Layout() {
  const manifest = useManifest();
  const { features } = useCapabilities();
  const location = useLocation();
  const { mode, setMode } = useColorScheme();
  const isDark = mode === "dark";
  const fetchStatus = useFetchStatus();
  const navRef = useRef<HTMLElement>(null);
  const [dismissedFetchStrip, setDismissedFetchStrip] = useState<string | null>(null);
  usePageDocumentTitle(location.pathname, manifest.branding.title);
  const flakyActive = location.pathname === "/flaky" || location.pathname.startsWith("/flaky/");
  const pullRequestsActive = location.pathname === "/pull-requests" || location.pathname.startsWith("/pull-requests/");
  const healthActive = location.pathname === "/analysis-health";
  const usageActive = location.pathname === "/ai-usage";
  const overviewActive = !flakyActive && !pullRequestsActive && !healthActive && !usageActive;
  const pullRequestsEnabled = manifest.pull_requests?.enabled ?? false;

  useEffect(() => {
    navRef.current?.querySelector<HTMLElement>('[aria-current="page"]')?.scrollIntoView({
      block: "nearest",
      inline: "center",
    });
  }, [features.ai_usage, features.analysis_health, location.pathname, pullRequestsEnabled]);

  return (
    <FetchStatusContext.Provider value={fetchStatus}>
    <Box sx={{ minHeight: "100vh", bgcolor: "background.default", color: "text.primary" }}>
      <MuiLink
        href="#main-content"
        sx={{
          position: "fixed",
          top: 8,
          left: 8,
          zIndex: (theme) => theme.zIndex.tooltip,
          px: 1.5,
          py: 1,
          bgcolor: "background.paper",
          color: "primary.main",
          border: "1px solid",
          borderColor: "primary.main",
          borderRadius: "4px",
          fontWeight: 700,
          transform: "translateY(calc(-100% - 16px))",
          transition: "transform 150ms ease",
          "&:focus": { transform: "translateY(0)" },
          "@media (prefers-reduced-motion: reduce)": { transition: "none" },
        }}
      >
        Skip to main content
      </MuiLink>
      <AppBar
        position="sticky"
        color="transparent"
        elevation={0}
        sx={{
          bgcolor: (theme) => (theme.vars ?? theme).palette.surface.container,
          backgroundImage: "none",
          borderBottom: "1px solid",
          borderColor: "divider",
          width: "100%",
          maxWidth: "100vw",
        }}
      >
        <Toolbar
          disableGutters
          sx={{
            minHeight: { xs: "auto !important", xl: "56px !important" },
            px: { xs: 2, sm: 3 },
            py: { xs: 1, xl: 0 },
            display: "grid",
            gridTemplateColumns: {
              xs: "minmax(0, 1fr) auto",
              xl: "minmax(0, auto) auto minmax(0, 1fr)",
            },
            gridTemplateAreas: {
              xs: '"brand controls" "nav nav"',
              xl: '"brand nav controls"',
            },
            columnGap: { xs: 1.5, sm: 2 },
            rowGap: { xs: 0.75, xl: 0 },
            alignItems: "center",
          }}
        >
          <MuiLink
            component={RouterLink}
            to="/"
            aria-label={`${manifest.branding.title} home`}
            underline="none"
            color="inherit"
            sx={{
              display: "flex",
              gridArea: "brand",
              alignItems: "center",
              minHeight: { xs: 44, xl: 32 },
              gap: 1.5,
              minWidth: { xs: 44, sm: 0 },
              maxWidth: "100%",
              transition: "opacity 150ms ease",
              "&:hover": { opacity: 0.8 },
            }}
          >
            <Box
              sx={{
                display: "flex",
                alignItems: "center",
                justifyContent: "center",
                width: 32,
                height: 32,
                flexShrink: 0,
              }}
            >
              <SvgIcon viewBox="0 0 64 64" sx={{ fontSize: 30 }}>
                <defs>
                  <linearGradient id="asterMark" x1="0" y1="0" x2="0.35" y2="1">
                    <stop offset="0" stopColor="var(--mui-palette-brand-from)" />
                    <stop offset="1" stopColor="var(--mui-palette-brand-to)" />
                  </linearGradient>
                </defs>
                <path
                  d="M32 5.1 60 58.9 32 49.02 4 58.9Z M32 18.32 40.78 38.22 32 34.1 23.22 38.22Z"
                  fill="url(#asterMark)"
                  fillRule="evenodd"
                />
              </SvgIcon>
            </Box>
            <Typography
              variant="headline"
              component="span"
              sx={{
                display: { xs: "none", sm: "block" },
                fontSize: "1.125rem",
                fontWeight: 600,
                letterSpacing: "-0.01em",
                color: "text.primary",
                whiteSpace: "nowrap",
                overflow: "hidden",
                textOverflow: "ellipsis",
                maxWidth: { sm: "min(48vw, 28rem)", xl: "min(28vw, 32rem)" },
              }}
            >
              {manifest.branding.title}
            </Typography>
          </MuiLink>

          <Box
            ref={navRef}
            component="nav"
            aria-label="Primary"
            sx={{
              display: "flex",
              gridArea: "nav",
              alignItems: "center",
              gap: 0.5,
              justifySelf: { xs: "stretch", xl: "start" },
              justifyContent: { xs: "flex-start", xl: "flex-start" },
              width: { xs: "100%", xl: "auto" },
              minWidth: 0,
              overflowX: "auto",
              scrollbarWidth: "none",
              "&::-webkit-scrollbar": { display: "none" },
              flexShrink: 0,
            }}
          >
            <NavTab
              to="/"
              label="Overview"
              active={overviewActive}
              current={location.pathname === "/"}
            />
            <NavTab
              to="/flaky"
              label="Failure Trends"
              active={flakyActive}
              current={location.pathname === "/flaky"}
            />
            {pullRequestsEnabled && (
              <NavTab
                to="/pull-requests"
                label="Pull Requests"
                active={pullRequestsActive}
                current={location.pathname === "/pull-requests"}
              />
            )}
            {features.analysis_health && (
              <NavTab to="/analysis-health" label="Analysis Health" active={healthActive} current={healthActive} />
            )}
            {features.ai_usage && (
              <NavTab to="/ai-usage" label="AI Usage" active={usageActive} current={usageActive} />
            )}
          </Box>

          <Box
            sx={{
              gridArea: "controls",
              display: "flex",
              alignItems: "center",
              justifyContent: "flex-end",
              justifySelf: "end",
              gap: { xs: 1, sm: 2 },
              minWidth: 0,
            }}
          >
            <SearchBar />
            <FetchStatusControl response={fetchStatus} />
            {mode !== undefined && (
              <IconButton
                aria-label={`Switch to ${isDark ? "light" : "dark"} mode`}
                onClick={() => setMode(isDark ? "light" : "dark")}
                size="small"
                sx={{
                  width: { xs: 44, sm: 32 },
                  height: { xs: 44, sm: 32 },
                  color: "text.secondary",
                  "&:hover": { color: "text.primary", bgcolor: "surface.containerHigh" },
                }}
              >
                {isDark ? <LightMode fontSize="small" /> : <DarkMode fontSize="small" />}
              </IconButton>
            )}
            <ProfileMenu />
          </Box>
        </Toolbar>
      </AppBar>

      <FetchStatusStrip
        response={fetchStatus}
        dismissedKey={dismissedFetchStrip}
        onDismiss={setDismissedFetchStrip}
      />
      <Container
        id="main-content"
        tabIndex={-1}
        component="main"
        maxWidth="xl"
        sx={{ minWidth: 0, py: { xs: 2, sm: 3 }, "&:focus": { outline: "none" } }}
      >
        <RouteErrorBoundary resetKey={`${location.pathname}${location.search}`}>
          <Outlet />
        </RouteErrorBoundary>
      </Container>
    </Box>
    </FetchStatusContext.Provider>
  );
}
