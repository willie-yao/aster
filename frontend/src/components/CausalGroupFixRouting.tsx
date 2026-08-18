import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import Typography from "@mui/material/Typography";
import { AutoFixHigh } from "@mui/icons-material";
import { Link as RouterLink } from "react-router-dom";
import type { CausalGroupFixTarget } from "../lib/patternFixGuidance";
import { testRunPath } from "../lib/routes";
import { overviewTypography } from "../theme/overview";

// CausalGroupFixRouting points one cause at a failed test that can actually
// start a Fix investigation, and says so plainly when no such test exists. The
// visible label names the test it opens, so several causes on one briefing stay
// tellable apart without reading the surrounding prose.
export function CausalGroupFixRouting({
  jobID,
  target,
}: {
  jobID?: string;
  target: CausalGroupFixTarget | null;
}) {
  if (!jobID) return null;

  if (!target) {
    return (
      <Typography sx={{ mt: 1.5, color: "text.secondary", ...overviewTypography.description }}>
        No failed JUnit test in these builds meets the Fix investigation requirements, so no Fix investigation can start from this cause.
      </Typography>
    );
  }

  // The full subject also backs the title, so truncating the test name inline
  // never hides which test the action opens.
  const subject = `Open ${target.testName} in build ${target.buildID} for Fix investigation`;

  return (
    <Button
      component={RouterLink}
      to={testRunPath(jobID, target.testName, target.buildID)}
      variant="outlined"
      size="small"
      startIcon={<AutoFixHigh aria-hidden />}
      title={subject}
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
        Open {target.testName}
      </Box>
      <Box component="span" sx={{ flexShrink: 0, pl: 0.5 }}>
        in build {target.buildID}
      </Box>
    </Button>
  );
}
