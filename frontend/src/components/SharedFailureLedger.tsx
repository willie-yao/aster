import Box from "@mui/material/Box";
import Link from "@mui/material/Link";
import Typography from "@mui/material/Typography";
import { Link as RouterLink } from "react-router-dom";
import { sharedFailurePath } from "../lib/routes";
import { sharedFailureScope, sharedFailureSubject } from "../lib/sharedFailures";
import { overviewLayout, overviewTypography } from "../theme/overview";
import type { SharedFailure } from "../types/pullRequests";

// SharedFailureLedger lists the failures hitting several open pull requests at
// once. It leads the triage page because a failure on many pull requests is
// nobody's to fix from their own page, and this is where it can be.
export function SharedFailureLedger({ failures }: { failures: SharedFailure[] }) {
  if (failures.length === 0) return null;

  return (
    <Box component="section" aria-labelledby="shared-failure-heading">
      <Box
        sx={{
          minHeight: overviewLayout.majorBandMinHeight,
          display: "flex",
          alignItems: "center",
          justifyContent: "space-between",
          gap: 1.5,
          px: 1.5,
          bgcolor: "surface.containerHigh",
          borderBlock: "1px solid",
          borderColor: "divider",
          boxShadow: "inset 3px 0 0 var(--mui-palette-error-main)",
        }}
      >
        <Typography
          id="shared-failure-heading"
          variant="headline"
          component="h2"
          sx={overviewTypography.majorHeading}
        >
          Shared failures
        </Typography>
        <Typography variant="data" color="textSecondary" sx={overviewTypography.data}>
          {failures.length === 1 ? "1 failure" : `${failures.length} failures`}
        </Typography>
      </Box>

      <Box sx={{ bgcolor: "surface.container", borderBottom: "1px solid", borderColor: "divider" }}>
        {failures.map((failure) => (
          <Link
            key={failure.id}
            component={RouterLink}
            to={sharedFailurePath(failure.id)}
            underline="none"
            sx={{
              display: "grid",
              gridTemplateColumns: { xs: "minmax(0, 1fr)", sm: "minmax(0, 1fr) auto" },
              alignItems: "center",
              columnGap: 1.5,
              rowGap: 0.25,
              px: { xs: 1.5, sm: 2 },
              py: 1.25,
              borderTop: "1px solid",
              borderColor: "divider",
              color: "inherit",
              transition: "background-color 140ms ease",
              "&:hover": { bgcolor: "surface.containerHigh" },
              "&:focus-visible": {
                outline: "2px solid",
                outlineColor: "primary.main",
                outlineOffset: -2,
              },
            }}
          >
            <Box sx={{ minWidth: 0 }}>
              <Typography
                title={sharedFailureSubject(failure)}
                color="textPrimary"
                sx={{ minWidth: 0, overflowWrap: "anywhere", ...overviewTypography.jobIdentifier }}
              >
                {sharedFailureSubject(failure)}
              </Typography>
              <Typography color="textSecondary" sx={{ mt: 0.25, ...overviewTypography.description }}>
                {failure.job_name} · {sharedFailureScope(failure)}
              </Typography>
            </Box>
            <Typography
              color="error"
              sx={{ justifySelf: { xs: "start", sm: "end" }, whiteSpace: "nowrap", ...overviewTypography.data }}
            >
              {failure.pull_requests.length} PRs
            </Typography>
          </Link>
        ))}
      </Box>
    </Box>
  );
}
