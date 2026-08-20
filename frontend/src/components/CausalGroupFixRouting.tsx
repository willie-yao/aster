import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import Tooltip from "@mui/material/Tooltip";
import Typography from "@mui/material/Typography";
import { AutoFixHigh } from "@mui/icons-material";
import { Link as RouterLink } from "react-router-dom";
import { parseTestDisplayName } from "../lib/detailTitles";
import type { CausalGroupFixTarget } from "../lib/patternFixGuidance";
import type { AnalysisCauseLocation } from "../types/dashboard";
import { testRunPath } from "../lib/routes";
import { overviewTypography } from "../theme/overview";
import { UpstreamCauseNotice } from "./UpstreamCauseNotice";

// CausalGroupFixRouting points one cause at a failed test that can actually
// start a fix proposal, and says so plainly when no such test exists. The
// visible label names the test it opens, so several causes on one briefing stay
// tellable apart without reading the surrounding prose.
export function CausalGroupFixRouting({
  jobID,
  target,
  showBuild = false,
  externalCause,
}: {
  jobID?: string;
  target: CausalGroupFixTarget | null;
  // Set when another cause on this briefing routes to the same test, which is
  // the only case where the build is needed to tell two actions apart.
  showBuild?: boolean;
  externalCause?: AnalysisCauseLocation | null;
}) {
  if (!jobID) return null;

  if (!target) {
    // A cause owned by a dependency is a real diagnosis, not missing evidence,
    // so name the repository instead of reporting an unexplained dead end.
    if (externalCause) return <UpstreamCauseNotice location={externalCause} />;
    return (
      <Typography color="textSecondary" sx={{ mt: 1.5, ...overviewTypography.description }}>
        No failed JUnit test in these builds meets the Fix eligibility requirements, so no fix proposal can start from this cause.
      </Typography>
    );
  }

  // The same humanized title the test ledger uses, so the action names the test
  // the way the rest of the page does instead of repeating the raw JUnit name.
  const testName = parseTestDisplayName(target.testName).displayName;
  // One suffix backs both the accessible name and the optional visible segment,
  // so the visible label is always a literal prefix of the accessible name.
  const buildSuffix = ` in build ${target.buildID}`;
  const subject = `Fix: ${testName}${buildSuffix}`;

  return (
    <Tooltip title={subject}>
      <Button
        component={RouterLink}
        to={testRunPath(jobID, target.testName, target.buildID)}
        variant="outlined"
        size="small"
        startIcon={<AutoFixHigh aria-hidden />}
        aria-label={subject}
        sx={{
          mt: 1.5,
          minHeight: { xs: 44, sm: 32 },
          maxWidth: "100%",
          width: { xs: "100%", sm: "auto" },
          justifyContent: "flex-start",
          textAlign: "left",
          textTransform: "none",
          ...overviewTypography.secondaryBody,
          fontWeight: 650,
          "&:focus-visible": {
            outline: "2px solid",
            outlineColor: "primary.main",
            outlineOffset: 2,
          },
        }}
      >
        <Box
          component="span"
          sx={{ minWidth: 0, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}
        >
          Fix: {testName}
        </Box>
        {showBuild && (
          // whiteSpace: "pre" keeps the suffix's leading space, so the rendered
          // text carries the same separator the accessible name does.
          <Box component="span" sx={{ flexShrink: 0, whiteSpace: "pre" }}>
            {buildSuffix}
          </Box>
        )}
      </Button>
    </Tooltip>
  );
}
