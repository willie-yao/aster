import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import Tooltip from "@mui/material/Tooltip";
import Typography from "@mui/material/Typography";
import { AutoFixHigh, VisibilityOutlined } from "@mui/icons-material";
import { Link as RouterLink } from "react-router-dom";
import { parseTestDisplayName } from "../lib/detailTitles";
import type { CausalGroupFixTarget } from "../lib/patternFixGuidance";
import type { AnalysisCauseLocation } from "../types/dashboard";
import { testRunPath } from "../lib/routes";
import { overviewTypography } from "../theme/overview";
import { UpstreamCauseNotice } from "./UpstreamCauseNotice";

// CausalGroupFixNotice explains where a cause routes, without offering the
// route itself. Ownership is reported whether or not a route exists: a cause
// owned by a dependency still often has a project-side test that can start a
// fix, and hiding that ownership behind the button made an upstream cause look
// identical to one the project can actually fix.
//
// It is the prose half of the pair, so it stays in the card body while
// CausalGroupFixButton moves to the card's action bar.
export function CausalGroupFixNotice({
  jobID,
  target,
  externalCause,
  evidencePresent = true,
}: {
  jobID?: string;
  target: CausalGroupFixTarget | null;
  externalCause?: AnalysisCauseLocation | null;
  // False when this cause's builds have left the analysis window, which is a
  // different dead end from builds that are present but carry no eligible
  // failure, and the only one no rerun of the eligibility rules can change.
  evidencePresent?: boolean;
}) {
  if (!jobID) return null;
  // A cause owned by a dependency is a real diagnosis, not missing evidence, so
  // naming the repository takes precedence over reporting a dead end.
  if (externalCause) return <UpstreamCauseNotice location={externalCause} />;
  if (target) return null;
  return (
    <Typography color="textSecondary" sx={{ mt: 1.5, ...overviewTypography.description }}>
      {evidencePresent
        ? "No failed JUnit test in these builds meets the Fix eligibility requirements, so no fix proposal can start from this cause."
        : "The builds this cause was correlated from have left the analysis window, so no fix proposal can start from it. A later failure of the same cause will produce a fresh, fixable one."}
    </Typography>
  );
}

// CausalGroupFixButton opens the failure that represents one cause, which is
// where a fix proposal can be started. The label names the test it opens, so
// the action stays readable without the surrounding prose; it shrinks with an
// ellipsis so a long test name cannot crowd the rest of the action bar.
export function CausalGroupFixButton({
  jobID,
  target,
  showBuild = false,
  stale = false,
}: {
  jobID?: string;
  target: CausalGroupFixTarget | null;
  // Set when another cause on this briefing routes to the same test, which is
  // the only case where the build is needed to tell two actions apart.
  showBuild?: boolean;
  stale?: boolean;
}) {
  if (!jobID || !target) return null;

  // The same humanized title the test ledger uses, so the action names the test
  // the way the rest of the page does instead of repeating the raw JUnit name.
  const testName = parseTestDisplayName(target.testName).displayName;
  // One suffix backs both the accessible name and the optional visible segment,
  // so the visible label is always a literal prefix of the accessible name.
  const buildSuffix = ` in build ${target.buildID}`;
  // The button navigates, so one label covers both states. A stale route stays
  // reachable but is demoted to a text button, so it never reads as the live
  // action beside the cause's other control.
  const actionLabel = "Open representative failure";
  const subject = `${actionLabel}: ${testName}${buildSuffix}`;

  return (
    <Tooltip title={subject}>
      <Button
        component={RouterLink}
        to={testRunPath(jobID, target.testName, target.buildID)}
        variant={stale ? "text" : "outlined"}
        size="small"
        startIcon={stale ? <VisibilityOutlined aria-hidden /> : <AutoFixHigh aria-hidden />}
        aria-label={subject}
        sx={{
          minHeight: { xs: 44, sm: 32 },
          // A zero basis keeps this item from ever forcing a wrap, so it grows
          // into the space the resolution control leaves and ellipsizes its
          // label instead of dropping onto its own line. minWidth: 0 is what
          // permits shrinking below the label's intrinsic width.
          flex: "1 1 0",
          minWidth: 0,
          maxWidth: "100%",
          justifyContent: "flex-start",
          textAlign: "left",
          textTransform: "none",
          ...overviewTypography.secondaryBody,
          fontWeight: 650,
          color: stale ? "text.secondary" : "primary.main",
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
          {actionLabel}: {testName}
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
