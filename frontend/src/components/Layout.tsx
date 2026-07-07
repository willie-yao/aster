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
import { Link as RouterLink, Outlet, useLocation } from "react-router-dom";
import { SearchBar } from "./SearchBar";
import { ProfileMenu } from "./ProfileMenu";
import { useManifest } from "../hooks/useManifest";

export function Layout() {
  const manifest = useManifest();
  const location = useLocation();
  const { mode, setMode } = useColorScheme();
  const isDark = mode === "dark";
  const flakyActive = location.pathname === "/flaky" || location.pathname.startsWith("/flaky/");

  return (
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
        }}
      >
        <Toolbar
          disableGutters
          sx={{
            minHeight: "64px !important",
            px: { xs: 2, sm: 3 },
            gap: { xs: 1.5, sm: 2 },
          }}
        >
          <MuiLink
            component={RouterLink}
            to="/"
            underline="none"
            color="inherit"
            sx={{
              display: "flex",
              alignItems: "center",
              gap: 1.5,
              minWidth: 0,
              transition: "opacity 150ms ease",
              "&:hover": { opacity: 0.8 },
            }}
          >
            <SvgIcon
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              strokeWidth={2}
              strokeLinecap="round"
              strokeLinejoin="round"
              sx={{ fontSize: 20, color: "primary.main", flexShrink: 0, fill: "none" }}
            >
              <path d="M21 12V7H5a2 2 0 0 1 0-4h14v4" />
              <path d="M3 5v14a2 2 0 0 0 2 2h16v-5" />
              <path d="M18 12a2 2 0 0 0 0 4h4v-4Z" />
            </SvgIcon>
            <Typography
              variant="headline"
              component="h1"
              sx={{
                display: { xs: "none", sm: "block" },
                fontSize: "1.125rem",
                fontWeight: 600,
                letterSpacing: "-0.01em",
                color: "text.primary",
                whiteSpace: "nowrap",
              }}
            >
              {manifest.branding.title}
            </Typography>
          </MuiLink>

          <Box
            sx={{
              ml: "auto",
              display: "flex",
              alignItems: "center",
              justifyContent: "flex-end",
              gap: { xs: 1, sm: 2 },
              flex: { xs: "0 0 auto", sm: "1 1 auto" },
              minWidth: 0,
            }}
          >
            <SearchBar />
            <Box component="nav" sx={{ display: "flex", alignItems: "center" }}>
              <Button
                component={RouterLink}
                to="/flaky"
                size="small"
                sx={{
                  color: flakyActive ? "primary.main" : "text.secondary",
                  fontWeight: flakyActive ? 600 : 500,
                  textTransform: "none",
                  whiteSpace: "nowrap",
                  "&:hover": { color: flakyActive ? "primary.main" : "text.primary" },
                }}
              >
                Test Analysis
              </Button>
            </Box>
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

      <Container component="main" maxWidth="lg" sx={{ py: 3 }}>
        <Outlet />
      </Container>
    </Box>
  );
}
