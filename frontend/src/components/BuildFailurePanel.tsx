import Box from "@mui/material/Box";
import Link from "@mui/material/Link";
import Stack from "@mui/material/Stack";
import Typography from "@mui/material/Typography";
import {
  AutoAwesome,
  ErrorOutlined,
  HourglassEmpty,
} from "@mui/icons-material";
import type { ReactNode } from "react";
import { Link as RouterLink } from "react-router-dom";
import type { BuildResult, TestCase } from "../types/dashboard";
import type { FetchStatusResponse } from "../types/fetchStatus";
import {
  buildAnalysisState,
  buildFailureActionID,
  type BuildAnalysisState,
} from "../lib/buildFailures";
import { buildActionEligibilityHint } from "../lib/actionEligibility";
import { buildFailurePath } from "../lib/routes";
import { FailureActions } from "./FailureActions";
import { useCapabilities } from "../hooks/useCapabilities";
import { AiAnalysisPanel } from "./AiAnalysisPanel";
import { RichText } from "./RichText";
import { AnalysisBriefing } from "./AnalysisBriefing";
import { overviewTypography } from "../theme/overview";

const stateText: Record<
  Exclude<BuildAnalysisState, "succeeded">,
  { title: string; detail: string }
> = {
  pending: {
    title: "Build analysis pending",
    detail:
      "Build analyses are active, but aggregate progress cannot identify this specific run.",
  },
  unavailable: {
    title: "Build analysis unavailable",
    detail: "No accepted build analysis is available for this run.",
  },
  stale: {
    title: "Build analysis status stale",
    detail: "The latest analysis progress could not be confirmed.",
  },
};

export function BuildFailurePanel({
  jobID,
  run,
  failure,
  fetchStatus,
  showDetailLink = true,
  briefingTitle = "Build failure analysis",
  mobileBriefingTitle = "Build failure",
  beforeActions,
}: {
  jobID: string;
  run: BuildResult;
  failure: TestCase;
  fetchStatus: FetchStatusResponse | null;
  showDetailLink?: boolean;
  briefingTitle?: string;
  mobileBriefingTitle?: string;
  beforeActions?: ReactNode;
}) {
  const state = buildAnalysisState(failure, fetchStatus);
  const { features } = useCapabilities();
  const fileCtx = {
    buildLogUrl: run.build_log_url,
    webUrl: run.web_url,
    fileLinks: failure.ai_analysis?.file_links,
  };
  const chatRef = failure.ai_analysis
    ? {
        job_id: jobID,
        build_id: run.build_id,
        test_name: failure.name,
        source: "build" as const,
        suite_name: failure.suite_name,
        class_name: failure.class_name,
        analysis_generated_at: failure.ai_analysis.generated_at,
      }
    : undefined;
  const pendingState = state === "succeeded" ? "unavailable" : state;
  const actionEligibility = buildActionEligibilityHint(
    failure.ai_analysis,
    features.analysis_critique_version,
  );
  const telemetry = failure.ai_analysis
    ? [
        failure.ai_analysis.cache_hit ? "Cache hit" : null,
        failure.ai_analysis.tool_calls != null
          ? `${failure.ai_analysis.tool_calls} tool calls`
          : null,
        failure.ai_analysis.gcs_bytes != null
          ? `${Math.round(failure.ai_analysis.gcs_bytes / 1024)} KB evidence`
          : null,
        failure.ai_analysis.elapsed_ms != null
          ? `${Math.round(failure.ai_analysis.elapsed_ms / 1000)}s`
          : null,
      ].filter((value): value is string => Boolean(value))
    : [];

  const stateNotice = state !== "succeeded" ? (
    <Box
      role="status"
      sx={{
        border: "1px solid",
        borderColor: "divider",
        borderRadius: "4px",
        p: 1.5,
        bgcolor: "surface.containerHigh",
      }}
    >
      <Stack direction="row" spacing={1.25} sx={{ alignItems: "flex-start" }}>
        {pendingState === "unavailable" || pendingState === "stale" ? (
          <ErrorOutlined color="warning" />
        ) : (
          <HourglassEmpty color="primary" />
        )}
        <Box>
          <Typography sx={overviewTypography.subsectionHeading}>
            {stateText[pendingState].title}
          </Typography>
          <Typography color="text.secondary" sx={overviewTypography.secondaryBody}>
            {stateText[pendingState].detail}
          </Typography>
        </Box>
      </Stack>
    </Box>
  ) : null;
  const summary =
    failure.ai_summary?.summary ??
    "This build failed before a failed JUnit test case was reported.";
  const hasMobileDetails = showDetailLink || telemetry.length > 0 || Boolean(failure.ai_analysis);
  const details = (
    <Stack spacing={1.75}>
      {stateNotice && <Box sx={{ display: { xs: "none", md: "block" } }}>{stateNotice}</Box>}
      {showDetailLink && (
        <Link
          component={RouterLink}
          to={buildFailurePath(jobID, run.build_id)}
          sx={{ minHeight: { xs: 44, sm: 36 }, display: "inline-flex", alignItems: "center", alignSelf: "flex-start", fontWeight: 650 }}
        >
          Open build failure details
        </Link>
      )}
      {telemetry.length > 0 && (
        <Typography color="text.secondary" sx={overviewTypography.data}>
          {telemetry.join(" · ")}
        </Typography>
      )}
      {failure.ai_analysis && (
        <AiAnalysisPanel
          analysis={failure.ai_analysis}
          fileCtx={fileCtx}
          chatRef={chatRef}
          appearance="detail"
        />
      )}
    </Stack>
  );
  const actions = (
    <FailureActions
      failureID={buildFailureActionID(jobID, run.build_id)}
      resolvable={false}
      eligibilityHint={actionEligibility}
      appearance="detail"
    />
  );
  const briefing = (
    <AnalysisBriefing
      title={briefingTitle}
      mobileTitle={mobileBriefingTitle}
      icon={<AutoAwesome aria-hidden sx={{ fontSize: 18, color: "primary.main" }} />}
      metadata={`Build ${run.build_id} · ${state === "succeeded" ? `${failure.ai_analysis?.severity ?? "Unknown"} severity` : stateText[pendingState].title}`}
      mobileMetadata={`Build ${run.build_id}`}
      mobileNotice={stateNotice}
      summary={<RichText text={summary} fileCtx={fileCtx} />}
      details={details}
      actions={beforeActions ? undefined : actions}
      collapseDetailsOnMobile={hasMobileDetails}
    />
  );

  if (!beforeActions) return briefing;

  return (
    <Stack spacing={2} sx={{ minWidth: 0 }}>
      {briefing}
      {beforeActions}
      <Box
        sx={{
          bgcolor: "surface.container",
          borderBlock: "1px solid",
          borderColor: "divider",
          px: { xs: 1.5, sm: 2 },
          py: 1,
        }}
      >
        {actions}
      </Box>
    </Stack>
  );
}
