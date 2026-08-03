import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import Chip from "@mui/material/Chip";
import Link from "@mui/material/Link";
import Stack from "@mui/material/Stack";
import Typography from "@mui/material/Typography";
import { ErrorOutlined, HourglassEmpty, OpenInNew, Troubleshoot } from "@mui/icons-material";
import { Link as RouterLink } from "react-router-dom";
import type { BuildResult, TestCase } from "../types/dashboard";
import type { FetchStatusResponse } from "../types/fetchStatus";
import { buildActionsReady, buildAnalysisState, buildFailureActionID, type BuildAnalysisState } from "../lib/buildFailures";
import { buildFailurePath } from "../lib/routes";
import { FailureActions } from "./FailureActions";
import { useCapabilities } from "../hooks/useCapabilities";
import { AiAnalysisPanel } from "./AiAnalysisPanel";
import { LabeledBlock } from "./LabeledBlock";
import { Panel } from "./Panel";
import { RichText } from "./RichText";
import { soft } from "../theme";

const stateText: Record<Exclude<BuildAnalysisState, "succeeded">, { title: string; detail: string }> = {
  pending: { title: "Build analysis pending", detail: "Build analyses are active, but aggregate progress cannot identify this specific run." },
  unavailable: { title: "Build analysis unavailable", detail: "No accepted build analysis is available for this run." },
  stale: { title: "Build analysis status stale", detail: "The latest analysis progress could not be confirmed." },
};

export function BuildFailurePanel({
  jobID,
  run,
  failure,
  fetchStatus,
  showDetailLink = true,
}: {
  jobID: string;
  run: BuildResult;
  failure: TestCase;
  fetchStatus: FetchStatusResponse | null;
  showDetailLink?: boolean;
}) {
  const state = buildAnalysisState(failure, fetchStatus);
  const { features } = useCapabilities();
  const fileCtx = {
    buildLogUrl: run.build_log_url,
    webUrl: run.web_url,
    fileLinks: failure.ai_analysis?.file_links,
  };
  const chatRef = failure.ai_analysis ? {
    job_id: jobID,
    build_id: run.build_id,
    test_name: failure.name,
    source: "build" as const,
    suite_name: failure.suite_name,
    class_name: failure.class_name,
    analysis_generated_at: failure.ai_analysis.generated_at,
  } : undefined;
  const pendingState = state === "succeeded" ? "unavailable" : state;
  const actionsReady = buildActionsReady(failure.ai_analysis, features.analysis_critique_version);
  const telemetry = failure.ai_analysis ? [
    failure.ai_analysis.cache_hit ? "Cache hit" : null,
    failure.ai_analysis.tool_calls != null ? `${failure.ai_analysis.tool_calls} tool calls` : null,
    failure.ai_analysis.gcs_bytes != null ? `${Math.round(failure.ai_analysis.gcs_bytes / 1024)} KB evidence` : null,
    failure.ai_analysis.elapsed_ms != null ? `${Math.round(failure.ai_analysis.elapsed_ms / 1000)}s` : null,
  ].filter((value): value is string => Boolean(value)) : [];

  return (
    <Panel component="section" sx={{ borderRadius: 3, p: { xs: 2, sm: 3 } }}>
      <Stack spacing={2.5}>
        <Stack direction={{ xs: "column", sm: "row" }} spacing={1.5} sx={{ alignItems: { sm: "center" } }}>
          <Stack direction="row" spacing={1} sx={{ alignItems: "center" }}>
            <Troubleshoot color="error" />
            <Typography component="h2" variant="headline">Build Failure</Typography>
          </Stack>
          <Chip size="small" color="error" variant="outlined" label={`Build ${run.build_id}`} />
          <Stack direction="row" spacing={1} sx={{ ml: { sm: "auto" }, flexWrap: "wrap" }}>
            {showDetailLink && (
              <Button component={RouterLink} to={buildFailurePath(jobID, run.build_id)} size="small">
                Open details
              </Button>
            )}
            {run.build_log_url && (
              <Link href={run.build_log_url} target="_blank" rel="noopener noreferrer" sx={{ display: "inline-flex", alignItems: "center", gap: 0.5 }}>
                Build log <OpenInNew sx={{ fontSize: 16 }} />
              </Link>
            )}
          </Stack>
        </Stack>

        {failure.ai_summary?.summary && (
          <LabeledBlock label="Summary" accent="primary">
            <Typography variant="body2"><RichText text={failure.ai_summary.summary} fileCtx={fileCtx} /></Typography>
          </LabeledBlock>
        )}

        {failure.ai_analysis ? (
          <>
            {telemetry.length > 0 && <Stack direction="row" spacing={1} sx={{ flexWrap: "wrap" }}>{telemetry.map((item) => <Chip key={item} size="small" label={item} />)}</Stack>}
            <AiAnalysisPanel
              analysis={failure.ai_analysis}
              fileCtx={fileCtx}
              chatRef={chatRef}
            />
            {actionsReady && <FailureActions failureID={buildFailureActionID(jobID, run.build_id)} resolvable={false} />}
          </>
        ) : (
          <Box role="status" sx={{ borderRadius: 2, p: 2, bgcolor: (theme) => soft(theme, pendingState === "unavailable" ? "warning" : "primary", 0.08) }}>
            <Stack direction="row" spacing={1.25} sx={{ alignItems: "flex-start" }}>
              {pendingState === "unavailable" || pendingState === "stale" ? <ErrorOutlined color="warning" /> : <HourglassEmpty color="primary" />}
              <Box>
                <Typography variant="subtitle2" sx={{ fontWeight: 700 }}>{stateText[pendingState].title}</Typography>
                <Typography variant="body2" color="text.secondary">{failure.ai_summary?.summary ?? stateText[pendingState].detail}</Typography>
              </Box>
            </Stack>
          </Box>
        )}
      </Stack>
    </Panel>
  );
}
