import Box from "@mui/material/Box";
import Typography from "@mui/material/Typography";
import { RichText } from "./RichText";
import { CausalGroupFixNotice } from "./CausalGroupFixRouting";
import { AnalysisChat } from "./AnalysisChat";
import type { FileToUrlContext } from "../lib/utils";
import type { CauseAnalysisChatReference } from "../types/analysisChat";
import type { CausalGroupFixTarget, CausalGroupNextStepSource } from "../lib/patternFixGuidance";
import type { AnalysisCauseLocation, PatternCausalGroup } from "../types/dashboard";
import { overviewTypography } from "../theme/overview";

// CausalGroupNextStep is the one place a cause offers something to act on: the
// remediation its own member analyses reported, the route to the failure a fix
// proposal starts from, and a chat grounded in the cause's member builds. Each
// part has its own gate, so the section renders whichever the deployment has.
export function CausalGroupNextStep({
  group,
  jobID,
  fileCtx,
  nextStep,
  chat,
  routing,
}: {
  group: PatternCausalGroup;
  jobID?: string;
  fileCtx?: FileToUrlContext;
  // The remediation from the newest affected build, which the page derives so
  // the cause agrees with the Test detail it links to. Defaults to the
  // remediation the engine carried on the group.
  nextStep?: CausalGroupNextStepSource | null;
  chat?: {
    ref: CauseAnalysisChatReference;
    baselineBuildID?: string;
    fileCtx: FileToUrlContext;
    preparedFinding?: boolean;
    onPreparedResolved?: (ready: boolean) => void;
  };
  // Omitted where no chat session on this deployment could start a fix.
  routing?: {
    target: CausalGroupFixTarget | null;
    externalCause?: AnalysisCauseLocation | null;
    evidencePresent?: boolean;
  };
}) {
  const carried = group.remediation;
  const source = nextStep ?? (carried ? { suggestedFix: carried.suggested_fix.trim(), buildID: carried.build_id } : null);
  const suggested = source?.suggestedFix;
  // Routing has nothing to explain without a job, which is the same condition
  // CausalGroupFixNotice returns null on.
  const routable = routing && jobID ? routing : undefined;
  // The route itself now lives in the card's action bar, so routing only counts
  // as content here when the notice will actually say something: a dependency
  // owns the cause, or there is no route to offer. Otherwise a cause with a
  // route but no remediation and no chat would render a bare heading.
  const explains = Boolean(routable && (routable.externalCause || !routable.target));
  if (!suggested && !explains && !chat) return null;

  return (
    <Box sx={{ mt: 1.5, minWidth: 0 }}>
      <Typography component="h5" sx={overviewTypography.subsectionHeading}>
        Next step
      </Typography>
      {suggested && (
        <Box sx={{ mt: 0.75, minWidth: 0 }}>
          <Typography
            component="div"
            sx={{ ...overviewTypography.primaryBody, color: "text.primary", fontWeight: 500 }}
          >
            <RichText text={suggested} steps fileCtx={fileCtx} />
          </Typography>
          {source?.buildID && (
            <Typography
              component="p"
              color="textSecondary"
              sx={{ m: 0, mt: 0.5, ...overviewTypography.description }}
            >
              Suggested remediation from build {source.buildID}
            </Typography>
          )}
        </Box>
      )}
      {routable && (
        <CausalGroupFixNotice
          jobID={jobID}
          target={routable.target}
          externalCause={routable.externalCause}
          evidencePresent={routable.evidencePresent}
        />
      )}
      {chat && (
        <Box sx={{ mt: 1.5 }}>
          <AnalysisChat
            key={`${chat.ref.job_id}\u0000${chat.ref.pattern_id}\u0000${chat.ref.causal_group_id}\u0000${chat.ref.causal_group_hash}`}
            analysisRef={chat.ref}
            baselineBuildID={chat.baselineBuildID}
            fileCtx={chat.fileCtx}
            preparedFinding={chat.preparedFinding}
            onPreparedResolved={chat.onPreparedResolved}
            fixTarget={routable?.target ?? undefined}
          />
        </Box>
      )}
    </Box>
  );
}
