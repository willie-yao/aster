import DarkMode from "@mui/icons-material/DarkMode";
import LightMode from "@mui/icons-material/LightMode";
import AppBar from "@mui/material/AppBar";
import Box from "@mui/material/Box";
import Container from "@mui/material/Container";
import IconButton from "@mui/material/IconButton";
import MuiLink from "@mui/material/Link";
import Toolbar from "@mui/material/Toolbar";
import Typography from "@mui/material/Typography";
import { useColorScheme } from "@mui/material/styles";
import { useState } from "react";
import { Link as RouterLink, Outlet, useLocation } from "react-router-dom";
import { SearchBar } from "./SearchBar";
import { RouteErrorBoundary } from "./RouteErrorBoundary";
import { ProfileMenu } from "./ProfileMenu";
import { FetchStatusControl, FetchStatusStrip } from "./FetchStatus";
import { AsterMark } from "./AsterMark";
import { BOTTOM_BAR_HEIGHT, NavBottomBar, NavRail } from "./NavRail";
import { useManifest } from "../hooks/useManifest";
import { useCapabilities } from "../hooks/useCapabilities";
import { useAuth } from "../hooks/useAuth";
import { useFetchStatus } from "../hooks/useFetchStatus";
import { FetchStatusContext } from "../hooks/useSharedFetchStatus";
import { navDestinations } from "../lib/navigation";
import { usePageDocumentTitle } from "../lib/pageMetadata";

export function Layout() {
  const manifest = useManifest();
  const { features } = useCapabilities();
  const auth = useAuth();
  const location = useLocation();
  const { mode, setMode } = useColorScheme();
  const isDark = mode === "dark";
  const fetchStatus = useFetchStatus();
  const [dismissedFetchStrip, setDismissedFetchStrip] = useState<string | null>(null);
  usePageDocumentTitle(location.pathname, manifest.branding.title);

  const destinations = navDestinations({
    pathname: location.pathname,
    pullRequestsEnabled: manifest.pull_requests?.enabled ?? false,
    analysisHealthEnabled: features.analysis_health ?? false,
    aiUsageEnabled: features.ai_usage ?? false,
    operatorAccess: auth.status === "authenticated",
  });
  const homeLabel = `${manifest.branding.title} home`;

  return (
    <FetchStatusContext.Provider value={fetchStatus}>
    <Box sx={{ display: "flex", minHeight: "100vh", bgcolor: "background.default", color: "text.primary" }}>
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

      <NavRail destinations={destinations} homeLabel={homeLabel} />

      <Box sx={{ flex: 1, minWidth: 0, display: "flex", flexDirection: "column" }}>
      <AppBar
        position="sticky"
        color="transparent"
        elevation={0}
        sx={{
          bgcolor: (theme) => (theme.vars ?? theme).palette.surface.container,
          backgroundImage: "none",
          borderBottom: "1px solid",
          borderColor: "divider",
        }}
      >
        <Toolbar
          disableGutters
          sx={{
            minHeight: { xs: "56px !important", md: "52px !important" },
            px: { xs: 2, sm: 3 },
            display: "flex",
            alignItems: "center",
            gap: { xs: 1, sm: 2 },
          }}
        >
          <MuiLink
            component={RouterLink}
            to="/"
            aria-label={homeLabel}
            underline="none"
            color="inherit"
            sx={{
              display: { xs: "flex", md: "none" },
              alignItems: "center",
              minHeight: 44,
              gap: 1.25,
              minWidth: 44,
              maxWidth: "100%",
              transition: "opacity 150ms ease",
              "@media (prefers-reduced-motion: reduce)": { transition: "none" },
              "&:hover": { opacity: 0.8 },
            }}
          >
            <Box
              sx={{
                display: "flex",
                alignItems: "center",
                justifyContent: "center",
                width: 30,
                height: 30,
                flexShrink: 0,
              }}
            >
              <AsterMark size={28} />
            </Box>
            <Typography
              variant="headline"
              component="span"
              sx={{
                display: { xs: "none", sm: "block" },
                fontSize: "1.0625rem",
                fontWeight: 600,
                letterSpacing: "-0.01em",
                color: "text.primary",
                whiteSpace: "nowrap",
                overflow: "hidden",
                textOverflow: "ellipsis",
                maxWidth: "min(48vw, 24rem)",
              }}
            >
              {manifest.branding.title}
            </Typography>
          </MuiLink>

          <SearchBar />

          <Box
            sx={{
              display: "flex",
              alignItems: "center",
              justifyContent: "flex-end",
              gap: { xs: 1, sm: 2 },
              minWidth: 0,
              flex: 1,
            }}
          >
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
        sx={{
          minWidth: 0,
          py: { xs: 2, sm: 3 },
          // Keep the fixed bottom bar from covering the end of the page.
          pb: {
            xs: `calc(${BOTTOM_BAR_HEIGHT}px + env(safe-area-inset-bottom) + 16px)`,
            md: 3,
          },
          "&:focus": { outline: "none" },
        }}
      >
        <RouteErrorBoundary resetKey={`${location.pathname}${location.search}`}>
          <Outlet />
        </RouteErrorBoundary>
      </Container>
      </Box>

      <NavBottomBar destinations={destinations} />
    </Box>
    </FetchStatusContext.Provider>
  );
}
