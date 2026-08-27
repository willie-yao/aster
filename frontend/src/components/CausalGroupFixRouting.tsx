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
  const buildSuffix = ` in build ${target.buildID}`;
  // The icon carries the verb, so the visible label is only the test. Spelling
  // it out as well pushed the label past any width the bar has and forced it to
  // ellipsize even on a wide screen. The accessible name therefore leads with
  // that same visible text and trails the verb, so it still starts with what is
  // on screen and speech input can match it (WCAG 2.5.3). The build suffix is
  // always in the accessible name and visible only when two causes collide,
  // which keeps the visible label a prefix of it either way.
  // A stale route keeps the muted, undecorated treatment, so it never reads as
  // the live action beside the cause's other control.
  const actionLabel = "open representative failure";
  const subject = `${testName}${buildSuffix}`;
  const accessibleName = `${subject}, ${actionLabel}`;

  return (
    <Tooltip title={accessibleName}>
      <Button
        component={RouterLink}
        to={testRunPath(jobID, target.testName, target.buildID)}
        variant="text"
        size="small"
        startIcon={stale ? <VisibilityOutlined aria-hidden /> : <AutoFixHigh aria-hidden />}
        aria-label={accessibleName}
        sx={{
          minHeight: { xs: 44, sm: 32 },
          // Sized to its content so it uses the width it has and ellipsizes
          // only once the row genuinely runs out. minWidth: 0 is what permits
          // shrinking below the label's intrinsic width.
          flex: "0 1 auto",
          minWidth: 0,
          maxWidth: "100%",
          px: 0,
          justifyContent: "flex-start",
          textAlign: "left",
          textTransform: "none",
          ...overviewTypography.secondaryBody,
          fontWeight: 650,
          color: stale ? "text.secondary" : "primary.main",
          // It navigates rather than acting, so it reads as a link and leaves
          // the bordered-control treatment to the resolution button.
          ...(stale
            ? {}
            : {
              textDecoration: "underline",
              textDecorationColor: "color-mix(in srgb, var(--mui-palette-primary-main) 40%, transparent)",
              textUnderlineOffset: 3,
            }),
          "&:hover": { bgcolor: "transparent", textDecorationColor: "currentColor" },
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
          {testName}
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
