import Box from "@mui/material/Box";
import Typography from "@mui/material/Typography";
import { RichText } from "./RichText";
import { CausalGroupFixRouting } from "./CausalGroupFixRouting";
import { CausalGroupRemediation } from "./CausalGroupRemediation";
import type { FileToUrlContext } from "../lib/utils";
import type { CausalGroupFixTarget } from "../lib/patternFixGuidance";
import type {
  AnalysisCauseLocation,
  PatternCausalGroup,
  PatternRemediationInvestigationSummary,
} from "../types/dashboard";
import { overviewTypography } from "../theme/overview";

// CausalGroupNextStep is the one place a cause offers something to act on: the
// remediation its own member analyses reported, the route to the failure a fix
// proposal starts from, and the investigation that verifies a code target.
// Each part has its own gate, so the section renders whichever the deployment
// has and nothing at all when it has none.
export function CausalGroupNextStep({
  group,
  jobID,
  fileCtx,
  investigation,
  routing,
}: {
  group: PatternCausalGroup;
  jobID?: string;
  fileCtx?: FileToUrlContext;
  // Omitted where the deployment does not run remediation investigations.
  investigation?: {
    summary?: PatternRemediationInvestigationSummary;
    patternID?: string;
    patternHash?: string;
    patternEligible?: boolean;
    chatAvailable?: boolean;
  };
  // Omitted where no chat session on this deployment could start a fix.
  routing?: {
    target: CausalGroupFixTarget | null;
    showBuild?: boolean;
    externalCause?: AnalysisCauseLocation | null;
    stale?: boolean;
    evidencePresent?: boolean;
  };
}) {
  const suggested = group.remediation?.suggested_fix?.trim();
  // Routing has nothing to point at without a job, which is the same condition
  // CausalGroupFixRouting returns null on.
  const routable = routing && jobID ? routing : undefined;
  if (!suggested && !routable && !investigation) return null;

  return (
    <Box sx={{ mt: 1.5, minWidth: 0 }}>
      <Typography component="h5" sx={overviewTypography.eyebrow}>
        Next step
      </Typography>
      {suggested && (
        <Box sx={{ mt: 0.75, minWidth: 0 }}>
          <Typography component="h6" color="textSecondary" sx={overviewTypography.eyebrow}>
            Suggested remediation
            {group.remediation?.build_id ? ` from build ${group.remediation.build_id}` : ""}
          </Typography>
          <Typography
            component="div"
            color="textSecondary"
            sx={{ mt: 0.5, ...overviewTypography.description }}
          >
            <RichText text={suggested} steps fileCtx={fileCtx} />
          </Typography>
        </Box>
      )}
      {routable && (
        <CausalGroupFixRouting
          jobID={jobID}
          target={routable.target}
          showBuild={routable.showBuild}
          externalCause={routable.externalCause}
          stale={routable.stale}
          evidencePresent={routable.evidencePresent}
        />
      )}
      {investigation && (
        <CausalGroupRemediation
          group={group}
          investigation={investigation.summary}
          jobID={jobID}
          patternID={investigation.patternID}
          patternHash={investigation.patternHash}
          patternEligible={investigation.patternEligible}
          chatAvailable={investigation.chatAvailable}
        />
      )}
    </Box>
  );
}
