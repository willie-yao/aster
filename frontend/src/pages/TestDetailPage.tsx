import { useMemo, useState, type ReactNode } from "react";
import { Link as RouterLink, useParams, useSearchParams } from "react-router-dom";
import {
  Box,
  Typography,
  Breadcrumbs,
  Link,
  Chip,
  Stack,
  Accordion,
  AccordionSummary,
  AccordionDetails,
} from "@mui/material";
import { useTheme } from "@mui/material/styles";
import {
  AutoAwesome,
  Assignment,
  Inventory2,
  Cloud,
  Dns,
  Place,
  ExpandMore,
  OpenInNew,
} from "@mui/icons-material";
import { useJobDetail } from "../hooks/useData";
import { useCapabilities } from "../hooks/useCapabilities";
import { formatDuration, highlightStackTrace, timeAgo } from "../lib/utils";
import { RichText } from "../components/RichText";
import { RunTimeline } from "../components/RunTimeline";
import { Panel } from "../components/Panel";
import { StatusChip } from "../components/StatusChip";
import { SectionHeading } from "../components/SectionHeading";
import { AiAnalysisPanel } from "../components/AiAnalysisPanel";
import { LoadingState } from "../components/LoadingState";
import { ErrorState } from "../components/ErrorState";
import { soft } from "../theme";
import type { BuildResult, TestCase } from "../types/dashboard";

/** Strip numbers and hex strings to normalize error messages for grouping. */
function normalizeMessage(msg: string): string {
  return msg
    .replace(/0x[0-9a-fA-F]+/g, "…")
    .replace(/[0-9a-f]{8,}/gi, "…")
    .replace(/\d+/g, "…")
    .replace(/…[.…]+/g, "…")
    .trim();
}

interface TestOccurrence {
  run: BuildResult;
  testCase: TestCase | null; // Absent from this run.
}

interface FailureGroup {
  normalizedMessage: string;
  sampleMessage: string;
  count: number;
}

const cap = (s: string) => s.charAt(0).toUpperCase() + s.slice(1);

/** A labelled value used in the run-detail grid. */
function Field({ label, children, mono }: { label: string; children: ReactNode; mono?: boolean }) {
  return (
    <Box>
      <Typography variant="label" color="text.secondary" sx={{ display: "block" }}>
        {label}
      </Typography>
      <Typography variant={mono ? "data" : "body2"} component="p">
        {children}
      </Typography>
    </Box>
  );
}

const preSx = {
  whiteSpace: "pre-wrap",
  fontFamily: "monospace",
  fontSize: "0.75rem",
  lineHeight: 1.6,
  m: 0,
  overflowX: "auto",
} as const;

const artifactLinkSx = {
  display: "inline-flex",
  alignItems: "center",
  gap: 0.5,
  fontSize: "0.75rem",
} as const;

const runLinkSx = {
  display: "inline-flex",
  alignItems: "center",
  gap: 0.5,
  color: "primary.main",
  fontSize: "0.875rem",
} as const;

/**
 * RunMetaPanel shows metadata for the selected run of this test. "column"
 * stacks fields for the sticky rail; "row" spreads them full-width.
 */
function RunMetaPanel({
  run,
  tc,
  orientation,
}: {
  run: BuildResult;
  tc: TestCase;
  orientation: "row" | "column";
}) {
  return (
    <Panel component="section" sx={{ borderRadius: "12px", p: { xs: 2, sm: 3 } }}>
      <Box sx={{ mb: 2, display: "flex", alignItems: "center", gap: 1.5, flexWrap: "wrap" }}>
        <Typography variant="headline" component="h2" sx={{ fontSize: "1rem" }}>
          Run Detail
        </Typography>
        <StatusChip status={tc.status} label={cap(tc.status)} />
        <Box sx={{ ml: "auto", display: "flex", alignItems: "center", gap: 2, flexWrap: "wrap" }}>
          {run.prow_url && (
            <Link href={run.prow_url} target="_blank" rel="noopener noreferrer" underline="hover" sx={runLinkSx}>
              View in Prow <OpenInNew sx={{ fontSize: 16 }} />
            </Link>
          )}
          {run.build_log_url && (
            <Link href={run.build_log_url} target="_blank" rel="noopener noreferrer" underline="hover" sx={runLinkSx}>
              Build Log <OpenInNew sx={{ fontSize: 16 }} />
            </Link>
          )}
        </Box>
      </Box>
      <Box
        sx={{
          display: "grid",
          columnGap: 4,
          rowGap: 1.5,
          gridTemplateColumns:
            orientation === "row"
              ? { xs: "1fr 1fr", sm: "repeat(4, minmax(0, 1fr))" }
              : "1fr",
        }}
      >
        <Field label="Build ID" mono>{run.build_id}</Field>
        <Field label="Started">{new Date(run.started).toLocaleString()}</Field>
        <Field label="Duration" mono>{formatDuration(tc.duration_seconds)}</Field>
        <Field label="Run finished">{timeAgo(run.finished)}</Field>
      </Box>
    </Panel>
  );
}

export function TestDetailPage() {
  const theme = useTheme();
  const { features } = useCapabilities();
  const { jobName: jobID, testName: encodedTestName } = useParams<{
    jobName: string;
    testName: string;
  }>();
  const testName = encodedTestName ? decodeURIComponent(encodedTestName) : "";
  const { data, loading, error } = useJobDetail(jobID);
  const displayName = data?.name ?? jobID ?? "";
  const [searchParams] = useSearchParams();
  const [selectedBuildId, setSelectedBuildId] = useState<string | null>(
    searchParams.get("run")
  );

  // Build per-run test occurrences oldest first for the timeline.
  const occurrences: TestOccurrence[] = useMemo(() => {
    if (!data) return [];
    const sorted = [...(data.runs ?? [])].sort(
      (a, b) => new Date(a.started).getTime() - new Date(b.started).getTime()
    );
    return sorted.map((run) => {
      const tc =
        (run.test_cases ?? []).find((t) => t.name === testName) ?? null;
      return { run, testCase: tc };
    });
  }, [data, testName]);

  // Most recent run containing this test.
  const latestOccurrence = useMemo(() => {
    for (let i = occurrences.length - 1; i >= 0; i--) {
      if (occurrences[i].testCase) return occurrences[i];
    }
    return null;
  }, [occurrences]);

  // Classify based on the latest streak and past pass/fail mix.
  const classification = useMemo(() => {
    if (!latestOccurrence) return null;
    // Count consecutive failures from the latest run backwards.
    let consecutive = 0;
    for (let i = occurrences.length - 1; i >= 0; i--) {
      const tc = occurrences[i].testCase;
      if (!tc) continue; // Skip runs where the test was absent.
      if (tc.status === "failed") consecutive++;
      else break;
    }
    if (consecutive === 0) return null;

    const failedRuns = occurrences.filter(
      (o) => o.testCase?.status === "failed"
    );
    const presentRuns = occurrences.filter((o) => o.testCase !== null);
    const passedRuns = presentRuns.filter(
      (o) => o.testCase!.status === "passed"
    );

    if (consecutive >= 3) return `Persistent (${consecutive}×)`;
    if (failedRuns.length > 1 && passedRuns.length > 0) return "Flaky";
    return "One-off";
  }, [occurrences, latestOccurrence]);

  // Group failure messages after normalizing volatile values.
  const failureGroups: FailureGroup[] = useMemo(() => {
    const failures = occurrences.filter(
      (o) => o.testCase?.status === "failed" && o.testCase?.failure_message
    );
    if (failures.length === 0) return [];

    const groups = new Map<string, { sample: string; count: number }>();
    for (const f of failures) {
      const msg = f.testCase!.failure_message!;
      const key = normalizeMessage(msg);
      const existing = groups.get(key);
      if (existing) {
        existing.count++;
      } else {
        groups.set(key, { sample: msg, count: 1 });
      }
    }

    return Array.from(groups.entries())
      .map(([normalized, { sample, count }]) => ({
        normalizedMessage: normalized,
        sampleMessage: sample,
        count,
      }))
      .sort((a, b) => b.count - a.count);
  }, [occurrences]);

  const totalFailures = occurrences.filter(
    (o) => o.testCase?.status === "failed"
  ).length;

  // Resolve the selected run from the URL or latest occurrence.
  const effectiveSelectedId =
    selectedBuildId ?? latestOccurrence?.run.build_id ?? null;
  const selectedOccurrence = useMemo(() => {
    if (!effectiveSelectedId) return null;
    return (
      occurrences.find((o) => o.run.build_id === effectiveSelectedId) ?? null
    );
  }, [occurrences, effectiveSelectedId]);

  if (loading) return <LoadingState />;

  if (error) {
    return (
      <ErrorState
        title="Failed to load job details"
        message={error}
        onRetry={() => window.location.reload()}
      />
    );
  }

  if (!data) return null;

  const testFound = occurrences.some((o) => o.testCase !== null);
  if (!testFound) {
    return (
      <Stack spacing={4}>
        <Breadcrumbs separator="›" sx={{ fontSize: "0.875rem" }}>
          <Link component={RouterLink} to="/" underline="hover" color="text.secondary">
            Dashboard
          </Link>
          <Link
            component={RouterLink}
            to={`/job/${encodeURIComponent(jobID ?? "")}`}
            underline="hover"
            color="text.secondary"
          >
            {displayName}
          </Link>
          <Typography variant="inherit" color="text.primary" noWrap>
            {testName}
          </Typography>
        </Breadcrumbs>
        <Panel sx={{ borderRadius: "12px", p: 4, textAlign: "center" }}>
          <Typography color="text.secondary">
            Test not found in any run of this job.
          </Typography>
        </Panel>
      </Stack>
    );
  }

  const selectedTc = selectedOccurrence?.testCase ?? null;
  const selectedRun = selectedOccurrence?.run ?? null;
  const displayStatus =
    selectedTc?.status ?? latestOccurrence?.testCase?.status ?? "skipped";
  const clsColor = classification
    ? classification.startsWith("Persistent")
      ? "error"
      : classification === "Flaky"
        ? "warning"
        : undefined
    : undefined;

  const fileCtx = (run: BuildResult | null, tc: TestCase) => ({
    buildLogUrl: run?.build_log_url,
    clusterArtifacts: tc.cluster_artifacts,
    webUrl: run?.web_url,
    fileLinks: tc.ai_analysis?.file_links,
  });

  // Whether the selected run has failure output or artifacts to show below the band.
  const hasEvidence = !!(
    selectedTc &&
    (selectedTc.failure_message ||
      selectedTc.failure_body ||
      selectedTc.failure_location ||
      selectedTc.cluster_artifacts ||
      (selectedTc.ai_summary && !selectedTc.ai_analysis))
  );

  return (
    <Stack spacing={{ xs: 3, sm: 4 }}>
      <Breadcrumbs separator="›" sx={{ fontSize: "0.875rem" }}>
        <Link component={RouterLink} to="/" underline="hover" color="text.secondary">
          Dashboard
        </Link>
        <Link
          component={RouterLink}
          to={`/job/${encodeURIComponent(jobID ?? "")}${effectiveSelectedId ? `?run=${effectiveSelectedId}` : ""}`}
          underline="hover"
          color="text.secondary"
        >
          {displayName}
        </Link>
        <Typography variant="inherit" color="text.primary" noWrap sx={{ maxWidth: 360 }} title={testName}>
          {testName}
        </Typography>
      </Breadcrumbs>

      <Box>
        <Typography
          variant="headline"
          component="h1"
          sx={{ fontSize: { xs: "1.25rem", sm: "1.5rem" }, wordBreak: "break-all" }}
        >
          {testName}
        </Typography>
        <Stack direction="row" spacing={1.5} sx={{ mt: 1.5, flexWrap: "wrap" }}>
          <StatusChip status={displayStatus} label={cap(displayStatus)} />
          {classification && (
            <Chip
              size="small"
              label={classification}
              sx={{
                fontWeight: 600,
                ...(clsColor
                  ? { bgcolor: (t) => soft(t, clsColor, 0.2), color: `${clsColor}.main` }
                  : { bgcolor: "action.selected", color: "text.secondary" }),
              }}
            />
          )}
        </Stack>
      </Box>

      <Box component="section">
        <SectionHeading title="History" />
        <RunTimeline
          runs={data?.runs ?? []}
          selectedBuildId={effectiveSelectedId ?? undefined}
          onSelect={setSelectedBuildId}
          colorFn={(run) => {
            const p = (theme.vars ?? theme).palette;
            const tc = (run.test_cases ?? []).find((t) => t.name === testName);
            if (!tc) return p.text.disabled;
            return tc.status === "passed"
              ? p.success.main
              : tc.status === "failed"
                ? p.error.main
                : p.text.secondary;
          }}
          tooltipFn={(run) => {
            const tc = (run.test_cases ?? []).find((t) => t.name === testName);
            return tc
              ? `#${run.build_id} — ${cap(tc.status)}`
              : `#${run.build_id} — Absent`;
          }}
        />
      </Box>

      {failureGroups.length > 0 && (
        <Box component="section">
          <SectionHeading title="Failure Patterns" />
          <Panel sx={{ borderRadius: "12px", p: 2 }}>
            <Stack spacing={1}>
              {failureGroups.map((group, i) => (
                <Stack key={i} direction="row" spacing={1.5} sx={{ alignItems: "flex-start" }}>
                  <Chip
                    size="small"
                    label={`${group.count} of ${totalFailures}`}
                    sx={{
                      flexShrink: 0,
                      fontWeight: 600,
                      bgcolor: (t) => soft(t, "error", 0.2),
                      color: "error.main",
                    }}
                  />
                  <Typography
                    variant="body2"
                    color="text.secondary"
                    noWrap
                    title={group.sampleMessage}
                  >
                    {group.sampleMessage.length > 120
                      ? group.sampleMessage.slice(0, 120) + "…"
                      : group.sampleMessage}
                  </Typography>
                </Stack>
              ))}
            </Stack>
          </Panel>
        </Box>
      )}

      {selectedRun && selectedTc && (
        <>
          {selectedTc.ai_analysis ? (
            <Box
              sx={{
                display: "grid",
                gridTemplateColumns: { xs: "1fr", lg: "minmax(0, 1.5fr) minmax(300px, 1fr)" },
                gap: 2,
                alignItems: "start",
              }}
            >
              <AiAnalysisPanel
                analysis={selectedTc.ai_analysis}
                fileCtx={fileCtx(selectedRun, selectedTc)}
                traceHref={
                  features.analysis_traces
                    ? `/analysis-traces?job_id=${encodeURIComponent(jobID ?? "")}` +
                      `&build_id=${encodeURIComponent(selectedRun.build_id)}` +
                      `&test_name=${encodeURIComponent(testName)}`
                    : undefined
                }
              />
              <Box
                sx={{
                  minWidth: 0,
                  position: { lg: "sticky" },
                  top: { lg: 80 },
                  alignSelf: "start",
                }}
              >
                <RunMetaPanel run={selectedRun} tc={selectedTc} orientation="column" />
              </Box>
            </Box>
          ) : (
            <RunMetaPanel run={selectedRun} tc={selectedTc} orientation="row" />
          )}

          {hasEvidence && (
            <Panel component="section" sx={{ borderRadius: "12px", p: { xs: 2, sm: 3 } }}>
              <Stack spacing={2.5}>
            {selectedTc.failure_message && (
              <Box
                component="pre"
                sx={{
                  ...preSx,
                  borderRadius: "8px",
                  p: 2,
                  bgcolor: (t) => soft(t, "error", 0.05),
                  color: "error.main",
                }}
              >
                {selectedTc.failure_message}
              </Box>
            )}

            {selectedTc.failure_body && (
              <Accordion
                disableGutters
                elevation={0}
                square
                sx={{
                  bgcolor: "transparent",
                  "&:before": { display: "none" },
                }}
              >
                <AccordionSummary
                  expandIcon={<ExpandMore />}
                  sx={{
                    px: 0,
                    minHeight: 0,
                    "& .MuiAccordionSummary-content": { my: 0 },
                  }}
                >
                  <Typography variant="label" color="text.secondary">
                    Stack Trace
                  </Typography>
                </AccordionSummary>
                <AccordionDetails sx={{ px: 0 }}>
                  <Box component="pre" sx={{ ...preSx, color: "text.secondary" }}>
                    {highlightStackTrace(selectedTc.failure_body)}
                  </Box>
                </AccordionDetails>
              </Accordion>
            )}

            {selectedTc.failure_location && (
              <Stack direction="row" spacing={1} sx={{ alignItems: "center" }}>
                <Place sx={{ fontSize: 16, color: "text.secondary" }} />
                {selectedTc.failure_location_url ? (
                  <Link
                    href={selectedTc.failure_location_url}
                    target="_blank"
                    rel="noopener noreferrer"
                    underline="hover"
                    sx={{ fontFamily: "monospace", fontSize: "0.75rem" }}
                  >
                    {selectedTc.failure_location}
                  </Link>
                ) : (
                  <Typography
                    sx={{ fontFamily: "monospace", fontSize: "0.75rem" }}
                    color="text.secondary"
                  >
                    {selectedTc.failure_location}
                  </Typography>
                )}
              </Stack>
            )}

            {selectedTc.cluster_artifacts && (
              <Box
                sx={{
                  borderRadius: "8px",
                  border: 1,
                  borderColor: "divider",
                  bgcolor: (t) => (t.vars ?? t).palette.surface.container,
                  p: 1.5,
                }}
              >
                <Typography variant="label" sx={{ fontWeight: 600 }}>
                  Debug Artifacts — {selectedTc.cluster_artifacts.cluster_name}
                </Typography>

                <Box sx={{ display: "flex", flexWrap: "wrap", gap: 1.5, mt: 1 }}>
                  {selectedTc.cluster_artifacts.provider_activity_log && (
                    <Link
                      href={selectedTc.cluster_artifacts.provider_activity_log}
                      target="_blank"
                      rel="noopener noreferrer"
                      underline="hover"
                      sx={artifactLinkSx}
                    >
                      <Cloud sx={{ fontSize: 14 }} /> Provider Activity Log
                    </Link>
                  )}
                  {selectedTc.cluster_artifacts.bootstrap_resources_url && (
                    <Link
                      href={selectedTc.cluster_artifacts.bootstrap_resources_url}
                      target="_blank"
                      rel="noopener noreferrer"
                      underline="hover"
                      sx={artifactLinkSx}
                    >
                      <Assignment sx={{ fontSize: 14 }} /> Cluster Resources
                    </Link>
                  )}
                  {selectedTc.cluster_artifacts.pod_log_dirs &&
                    Object.entries(selectedTc.cluster_artifacts.pod_log_dirs).map(
                      ([dir, url]) => (
                        <Link
                          key={dir}
                          href={url}
                          target="_blank"
                          rel="noopener noreferrer"
                          underline="hover"
                          sx={artifactLinkSx}
                        >
                          <Inventory2 sx={{ fontSize: 14 }} /> {dir}
                        </Link>
                      )
                    )}
                  {selectedRun?.web_url && (
                    <Link
                      href={`${selectedRun.web_url}artifacts/clusters/bootstrap/logs/`}
                      target="_blank"
                      rel="noopener noreferrer"
                      underline="hover"
                      sx={artifactLinkSx}
                    >
                      <Dns sx={{ fontSize: 14 }} /> Controller Logs
                    </Link>
                  )}
                </Box>

                {selectedTc.cluster_artifacts.machines &&
                  selectedTc.cluster_artifacts.machines.length > 0 && (
                    <Accordion
                      disableGutters
                      elevation={0}
                      square
                      sx={{
                        bgcolor: "transparent",
                        mt: 1,
                        "&:before": { display: "none" },
                      }}
                    >
                      <AccordionSummary
                        expandIcon={<ExpandMore />}
                        sx={{
                          px: 0,
                          minHeight: 0,
                          "& .MuiAccordionSummary-content": { my: 0, alignItems: "center", gap: 0.5 },
                        }}
                      >
                        <Dns sx={{ fontSize: 14, color: "text.secondary" }} />
                        <Typography variant="label" color="text.secondary">
                          Machine Logs ({selectedTc.cluster_artifacts.machines.length} machines)
                        </Typography>
                      </AccordionSummary>
                      <AccordionDetails sx={{ px: 0 }}>
                        <Stack spacing={1}>
                          {selectedTc.cluster_artifacts.machines.map((m) => (
                            <Box key={m.name} sx={{ pl: 2 }}>
                              <Typography
                                sx={{ fontFamily: "monospace", fontSize: "0.75rem" }}
                                color="text.secondary"
                              >
                                {m.name}
                              </Typography>
                              <Box sx={{ display: "flex", flexWrap: "wrap", gap: 1.5, mt: 0.5 }}>
                                {Object.entries(m.logs).map(([logType, url]) => (
                                  <Link
                                    key={logType}
                                    href={url}
                                    target="_blank"
                                    rel="noopener noreferrer"
                                    underline="hover"
                                    sx={{ fontSize: "0.6875rem" }}
                                  >
                                    {logType}
                                  </Link>
                                ))}
                              </Box>
                            </Box>
                          ))}
                        </Stack>
                      </AccordionDetails>
                    </Accordion>
                  )}
              </Box>
            )}

            {selectedTc.ai_summary && !selectedTc.ai_analysis && (
              <Stack
                direction="row"
                spacing={1}
                sx={{
                  alignItems: "flex-start",
                  borderRadius: "8px",
                  bgcolor: (t) => (t.vars ?? t).palette.surface.container,
                  p: 1.5,
                }}
              >
                <AutoAwesome sx={{ fontSize: 16, color: "primary.main", mt: "2px" }} />
                <Typography
                  variant="caption"
                  color={selectedTc.ai_summary.is_transient ? "text.secondary" : "warning.main"}
                >
                  <RichText
                    text={selectedTc.ai_summary.summary}
                    fileCtx={fileCtx(selectedRun, selectedTc)}
                  />
                </Typography>
              </Stack>
            )}
              </Stack>
            </Panel>
          )}
        </>
      )}

      {selectedRun && !selectedTc && (
        <Panel component="section" sx={{ borderRadius: "12px", p: 4, textAlign: "center" }}>
          <Typography color="text.secondary">
            This test was not present in build #{selectedRun.build_id}.
          </Typography>
        </Panel>
      )}
    </Stack>
  );
}
