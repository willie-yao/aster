import Box from "@mui/material/Box";
import Typography from "@mui/material/Typography";
import { RichText } from "./RichText";
import type { FileToUrlContext } from "../lib/utils";
import type { PatternCausalGroupRemediation } from "../types/dashboard";
import { overviewTypography } from "../theme/overview";

// CausalGroupReportedFix shows the action a cause's own member analysis
// proposed, so a cause is never presented with nothing to act on. It is framed
// as a report rather than an offer: Aster verifies a target only through the
// remediation investigation, so this text is a lead for the maintainer rather
// than something the dashboard will carry out. It renders from published data
// alone, which is why it does not depend on any deployment capability.
export function CausalGroupReportedFix({
  remediation,
  fileCtx,
}: {
  remediation?: PatternCausalGroupRemediation;
  fileCtx?: FileToUrlContext;
}) {
  const fix = remediation?.suggested_fix?.trim();
  if (!fix) return null;

  return (
    <Box sx={{ mt: 1.5, minWidth: 0 }}>
      <Typography component="h5" color="textSecondary" sx={overviewTypography.eyebrow}>
        Unverified suggested fix
        {remediation?.build_id ? ` from build ${remediation.build_id}` : ""}
      </Typography>
      <Typography component="div" color="textSecondary" sx={{ mt: 0.5, ...overviewTypography.description }}>
        <RichText text={fix} steps fileCtx={fileCtx} />
      </Typography>
    </Box>
  );
}
