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
import { useState } from "react";
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
        minHeight: { xs: 44, lg: 36 },
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
  const [dismissedFetchStrip, setDismissedFetchStrip] = useState<string | null>(null);
  usePageDocumentTitle(location.pathname, manifest.branding.title);
  const flakyActive = location.pathname === "/flaky" || location.pathname.startsWith("/flaky/");
  const pullRequestsActive = location.pathname === "/pull-requests" || location.pathname.startsWith("/pull-requests/");
  const tracesActive = location.pathname === "/analysis-traces";
  const usageActive = location.pathname === "/ai-usage";
  const overviewActive = !flakyActive && !pullRequestsActive && !tracesActive && !usageActive;
  const pullRequestsEnabled = manifest.pull_requests?.enabled ?? false;

  return (
    <FetchStatusContext.Provider value={fetchStatus}>
    <Box sx={{ minHeight: "100vh", bgcolor: "background.default", color: "text.primary" }}>
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
            minHeight: { xs: "auto !important", lg: "56px !important" },
            px: { xs: 2, sm: 3 },
            py: { xs: 1, lg: 0 },
            display: "grid",
            gridTemplateColumns: {
              xs: "minmax(0, 1fr) auto",
              lg: "minmax(0, auto) auto minmax(0, 1fr)",
            },
            gridTemplateAreas: {
              xs: '"brand controls" "nav nav"',
              lg: '"brand nav controls"',
            },
            columnGap: { xs: 1.5, sm: 2 },
            rowGap: { xs: 0.75, lg: 0 },
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
              gap: 1.5,
              minWidth: 0,
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
                maxWidth: { sm: "min(48vw, 28rem)", lg: "min(28vw, 32rem)" },
              }}
            >
              {manifest.branding.title}
            </Typography>
          </MuiLink>

          <Box
            component="nav"
            aria-label="Primary"
            sx={{
              display: "flex",
              gridArea: "nav",
              alignItems: "center",
              gap: 0.5,
              justifySelf: { xs: "stretch", lg: "start" },
              justifyContent: { xs: "center", lg: "flex-start" },
              width: { xs: "100%", lg: "auto" },
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
            {features.analysis_traces && (
              <NavTab to="/analysis-traces" label="Analysis Traces" active={tracesActive} current={tracesActive} />
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
      <Container component="main" maxWidth="xl" sx={{ minWidth: 0, py: { xs: 2, sm: 3 } }}>
        <RouteErrorBoundary resetKey={`${location.pathname}${location.search}`}>
          <Outlet />
        </RouteErrorBoundary>
      </Container>
    </Box>
    </FetchStatusContext.Provider>
  );
}
