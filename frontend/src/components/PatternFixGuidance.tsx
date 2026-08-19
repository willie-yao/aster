import InfoOutlined from "@mui/icons-material/InfoOutlined";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import Stack from "@mui/material/Stack";
import Typography from "@mui/material/Typography";
import { useEffect, type MouseEvent } from "react";
import { Link as RouterLink, useLocation } from "react-router-dom";
import { failedTestGridID } from "../lib/patternFixGuidance";
import { jobRunPath } from "../lib/routes";
import { overviewTypography } from "../theme/overview";
import type { AnalysisCauseLocation } from "../types/dashboard";
import { UpstreamCauseNotice } from "./UpstreamCauseNotice";

function revealFailedTestGrid() {
  const target = document.getElementById(failedTestGridID);
  if (target) {
    target.scrollIntoView({ block: "start" });
    return;
  }

  const toggle = document.querySelector<HTMLElement>(
    `[aria-controls="${failedTestGridID}"]`,
  );
  if (!toggle) return;

  if (toggle.getAttribute("aria-expanded") !== "true") toggle.click();
  window.history.replaceState(window.history.state, "", `#${failedTestGridID}`);
  window.requestAnimationFrame(() => {
    window.requestAnimationFrame(() => {
      document.getElementById(failedTestGridID)?.scrollIntoView({ block: "start" });
    });
  });
}

export function PatternFixGuidance({
  jobID,
  buildID,
  externalCause,
}: {
  jobID: string;
  buildID: string;
  externalCause?: AnalysisCauseLocation | null;
}) {
  const location = useLocation();
  const destination = `${jobRunPath(jobID, buildID)}#${failedTestGridID}`;

  useEffect(() => {
    if (location.hash === `#${failedTestGridID}`) revealFailedTestGrid();
  }, [location.hash, location.search]);

  function handleViewFailedTests(event: MouseEvent<HTMLAnchorElement>) {
    const destinationURL = new URL(destination, window.location.origin);
    if (`${location.pathname}${location.search}` !== `${destinationURL.pathname}${destinationURL.search}`) return;
    event.preventDefault();
    revealFailedTestGrid();
  }

  return (
    <Box
      component="aside"
      aria-labelledby="pattern-fix-guidance-title"
      sx={{
        minWidth: 0,
        maxWidth: "100%",
        border: "1px solid",
        borderColor: "divider",
        borderRadius: "4px",
        bgcolor: "surface.containerLow",
        px: { xs: 1.5, sm: 2 },
        py: 1.5,
      }}
    >
      <Stack direction="row" spacing={1} sx={{ alignItems: "flex-start" }}>
        <InfoOutlined aria-hidden sx={{ mt: 0.25, flexShrink: 0, color: "info.main" }} />
        <Box sx={{ minWidth: 0 }}>
          <Typography id="pattern-fix-guidance-title" component="h3" sx={overviewTypography.subsectionHeading}>
            {externalCause ? "Cause is in a dependency" : "Fix investigation unavailable"}
          </Typography>
          {externalCause ? (
            <UpstreamCauseNotice location={externalCause} />
          ) : (
            <Typography color="textSecondary" sx={{ mt: 0.5, ...overviewTypography.secondaryBody }}>
              This recurring result is grouped by cause, so it cannot produce one shared issue or Fix PR.
              No failed JUnit test in the affected builds meets the Fix investigation requirements yet, so no cause can start one either.
            </Typography>
          )}
          <Button
            component={RouterLink}
            to={destination}
            variant="outlined"
            onClick={handleViewFailedTests}
            sx={{ mt: 1.25, minHeight: 44, width: { xs: "100%", sm: "auto" } }}
          >
            View failed tests
          </Button>
          <Typography color="textSecondary" sx={{ mt: 1, ...overviewTypography.description }}>
            {externalCause
              ? "The pattern chat below helps compare causes across builds and confirm the upstream diagnosis against the evidence."
              : "The pattern chat below helps compare causes across builds. A Fix investigation becomes available once an individual failed JUnit test meets every Fix eligibility requirement."}
          </Typography>
        </Box>
      </Stack>
    </Box>
  );
}
