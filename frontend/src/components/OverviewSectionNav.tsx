import Box from "@mui/material/Box";
import Link from "@mui/material/Link";
import Typography from "@mui/material/Typography";
import type { MouseEvent } from "react";
import { Link as RouterLink, useLocation, useNavigate } from "react-router-dom";
import { overviewTypography } from "../theme/overview";

const sections = [
  { id: "needs-attention-heading", label: "Needs attention" },
  { id: "job-ledger-heading", label: "Job ledger" },
] as const;

function focusSection(id: string) {
  const target = document.getElementById(id);
  if (!target) return;
  const reducedMotion = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
  target.focus({ preventScroll: true });
  target.scrollIntoView({
    behavior: reducedMotion ? "auto" : "smooth",
    block: "start",
  });
}

export function OverviewSectionNav() {
  const location = useLocation();
  const navigate = useNavigate();

  function handleClick(event: MouseEvent<HTMLAnchorElement>, id: string) {
    event.preventDefault();
    navigate({
      pathname: location.pathname,
      search: location.search,
      hash: `#${id}`,
    });
    requestAnimationFrame(() => focusSection(id));
  }

  return (
    <Box
      component="nav"
      aria-label="Overview sections"
      sx={{
        width: { xs: "100%", sm: "fit-content" },
        maxWidth: "100%",
        minHeight: 48,
        display: "flex",
        alignItems: "center",
        flexWrap: "wrap",
        gap: 0.5,
        px: 1.5,
        border: "1px solid",
        borderColor: "divider",
        borderRadius: "4px",
        bgcolor: "surface.container",
      }}
    >
      <Typography variant="label" color="text.secondary" sx={{ mr: 0.5, ...overviewTypography.description }}>
        Jump to
      </Typography>
      {sections.map((section) => (
        <Link
          key={section.id}
          component={RouterLink}
          to={{ pathname: location.pathname, search: location.search, hash: `#${section.id}` }}
          onClick={(event) => handleClick(event, section.id)}
          underline="hover"
          sx={{
            minHeight: 44,
            display: "inline-flex",
            alignItems: "center",
            px: 1,
            borderRadius: "2px",
            color: "primary.main",
            fontSize: "14px",
            lineHeight: "20px",
            fontWeight: 650,
            "&:hover": { bgcolor: "surface.containerHigh" },
            "&:focus-visible": {
              outline: "2px solid",
              outlineColor: "primary.main",
              outlineOffset: -2,
            },
          }}
        >
          {section.label}
        </Link>
      ))}
    </Box>
  );
}
