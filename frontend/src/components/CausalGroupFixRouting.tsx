import Box from "@mui/material/Box";
import Link from "@mui/material/Link";
import Typography from "@mui/material/Typography";
import { Link as RouterLink } from "react-router-dom";
import type { CausalGroupFixTarget } from "../lib/patternFixGuidance";
import { testRunPath } from "../lib/routes";
import { overviewTypography } from "../theme/overview";

// CausalGroupFixRouting points one cause at a failed test that can actually
// start a Fix investigation, and says so plainly when no such test exists.
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
      <Typography color="text.secondary" sx={{ mt: 1, ...overviewTypography.description }}>
        No failed test in these builds has a verified source path, so a Fix investigation cannot start from this cause.
      </Typography>
    );
  }

  return (
    <Box sx={{ mt: 1 }}>
      <Link
        component={RouterLink}
        to={testRunPath(jobID, target.testName, target.buildID)}
        aria-label={`Start a Fix investigation from ${target.testName} in build ${target.buildID}`}
        underline="none"
        sx={{
          minHeight: { xs: 44, sm: 32 },
          display: "inline-flex",
          alignItems: "center",
          px: 1,
          borderRadius: "4px",
          bgcolor: "action.selected",
          color: "primary.main",
          ...overviewTypography.data,
          "&:hover": { bgcolor: "surface.containerHigh" },
          "&:focus-visible": {
            outline: "2px solid",
            outlineColor: "primary.main",
            outlineOffset: 2,
          },
        }}
      >
        Start a Fix investigation
      </Link>
      <Typography color="text.secondary" sx={{ mt: 0.5, ...overviewTypography.description }}>
        {target.testName} in build {target.buildID}
      </Typography>
    </Box>
  );
}
