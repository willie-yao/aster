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
import { ProfileMenu } from "./ProfileMenu";
import { FetchStatusControl, FetchStatusStrip } from "./FetchStatus";
import { useManifest } from "../hooks/useManifest";
import { useCapabilities } from "../hooks/useCapabilities";
import { useFetchStatus } from "../hooks/useFetchStatus";
import { FetchStatusContext } from "../hooks/useSharedFetchStatus";
import { usePageDocumentTitle } from "../lib/pageMetadata";
import { soft } from "../theme";

// Primary top-nav tab: pill highlight for the active section, with aria-current
// set only on the exact current page.
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
        px: { xs: 0.875, sm: 1.5 },
        py: 0.5,
        minWidth: 0,
        borderRadius: 999,
        fontSize: "0.8125rem",
        fontWeight: active ? 700 : 600,
        textTransform: "none",
        whiteSpace: "nowrap",
        color: active ? "primary.main" : "text.secondary",
        bgcolor: (theme) => (active ? soft(theme, "primary", 0.12) : "transparent"),
        transition: "color 150ms ease, background-color 150ms ease",
        "&:hover": {
          color: active ? "primary.main" : "text.primary",
          bgcolor: (theme) =>
            active ? soft(theme, "primary", 0.16) : (theme.vars ?? theme).palette.surface.containerHigh,
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
  const tracesActive = location.pathname === "/analysis-traces";
  const usageActive = location.pathname === "/ai-usage";
  const overviewActive = !flakyActive && !tracesActive && !usageActive;

  return (
    <FetchStatusContext.Provider value={fetchStatus}>
    <Box sx={{ minHeight: "100vh", bgcolor: "background.default", color: "text.primary" }}>
      <AppBar
        position="sticky"
        color="transparent"
        elevation={0}
        sx={{
          bgcolor: (theme) => (theme.vars ?? theme).palette.surface.glass,
          backgroundImage: "none",
          backdropFilter: "blur(12px)",
          WebkitBackdropFilter: "blur(12px)",
          borderBottom: "1px solid",
          borderColor: "divider",
          width: "100%",
          maxWidth: "100vw",
        }}
      >
        <Toolbar
          disableGutters
          sx={{
            minHeight: { xs: "auto !important", lg: "64px !important" },
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
                borderRadius: "9px",
                flexShrink: 0,
                bgcolor: (theme) => (theme.vars ?? theme).palette.surface.containerHigh,
                border: "1px solid",
                borderColor: "divider",
                boxShadow: (theme) => `0 0 16px -6px ${(theme.vars ?? theme).palette.primary.main}`,
              }}
            >
              <SvgIcon
                viewBox="0 0 24 24"
                fill="none"
                sx={{ fontSize: 20, color: "primary.main", fill: "none" }}
              >
                <path
                  d="M12 3.7 19.65 18.4 12 15.7l-7.65 2.7L12 3.7Z"
                  stroke="currentColor"
                  strokeWidth={1.85}
                  strokeLinejoin="round"
                />
                <path
                  d="M12 6.7c.35 2.13 1.45 3.25 3.55 3.6-2.1.35-3.2 1.47-3.55 3.6-.35-2.13-1.45-3.25-3.55-3.6 2.1-.35 3.2-1.47 3.55-3.6Z"
                  fill="currentColor"
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
              label="Failure Analysis"
              active={flakyActive}
              current={location.pathname === "/flaky"}
            />
            {features.analysis_traces && (
              <NavTab to="/analysis-traces" label="Traces" active={tracesActive} current={tracesActive} />
            )}
            {features.ai_usage && (
              <NavTab to="/ai-usage" label="Usage" active={usageActive} current={usageActive} />
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
                sx={{ color: "text.secondary", "&:hover": { color: "text.primary" } }}
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
      <Container component="main" maxWidth="xl" sx={{ minWidth: 0, py: 3 }}>
        <Outlet />
      </Container>
    </Box>
    </FetchStatusContext.Provider>
  );
}
