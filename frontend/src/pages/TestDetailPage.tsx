import { useMemo, useState } from "react";
import Box from "@mui/material/Box";
import Breadcrumbs from "@mui/material/Breadcrumbs";
import ButtonBase from "@mui/material/ButtonBase";
import Collapse from "@mui/material/Collapse";
import Link from "@mui/material/Link";
import Stack from "@mui/material/Stack";
import Typography from "@mui/material/Typography";
import Accordion from "@mui/material/Accordion";
import AccordionDetails from "@mui/material/AccordionDetails";
import AccordionSummary from "@mui/material/AccordionSummary";
import { useTheme } from "@mui/material/styles";
import {
  Assignment,
  AutoAwesome,
  ChevronRight,
  Cloud,
  Dns,
  Inventory2,
  OpenInNew,
  Place,
} from "@mui/icons-material";
import { Link as RouterLink, useParams, useSearchParams } from "react-router-dom";
import { useJobDetail } from "../hooks/useData";
import { useCapabilities } from "../hooks/useCapabilities";
import { useManifest } from "../hooks/useManifest";
import { persistentAfter } from "../lib/attention";
import { jobPath, jobRunPath, testRunPath } from "../lib/routes";
import {
  formatDuration,
  highlightStackTrace,
  meetsConfidenceFloor,
  shortJobName,
} from "../lib/utils";
import { parseTestDisplayName } from "../lib/detailTitles";
import { withJobDetailParam } from "../lib/jobDetail";
import { summarizeTestHistory } from "../lib/testDetail";
import { patternLifecycleActive } from "../lib/actionEligibility";
import { RichText } from "../components/RichText";
import { RunHistory } from "../components/RunHistory";
import { DetailSectionBand } from "../components/DetailSectionBand";
import { AiAnalysisPanel } from "../components/AiAnalysisPanel";
import { LoadingState } from "../components/LoadingState";
import { ErrorState } from "../components/ErrorState";
import { TechnicalIdentity } from "../components/TechnicalIdentity";
import { MetricStrip, type MetricStripItem } from "../components/MetricStrip";
import { RunMetadata } from "../components/RunMetadata";
import { RuntimeTrend } from "../components/RuntimeTrend";
import { AnalysisBriefing } from "../components/AnalysisBriefing";
import { overviewTypography } from "../theme/overview";
import type { BuildResult, PatternAnalysis, TestCase } from "../types/dashboard";
import { summarizeRuntime, testRuntimePoints } from "../lib/runtimeTrend";

function normalizeMessage(message: string): string {
  return message
    .replace(/0x[0-9a-fA-F]+/gu, "…")
    .replace(/[0-9a-f]{8,}/giu, "…")
    .replace(/\d+/gu, "…")
    .replace(/…[.…]+/gu, "…")
    .trim();
}

function firstSentence(value: string): string {
  const match = value.trim().match(/^.*?[.!?](?:\s|$)/u);
  return match?.[0].trim() || value.trim();
}

interface TestOccurrence {
  run: BuildResult;
  testCase: TestCase | null;
}

interface FailureGroup {
  normalizedMessage: string;
  sampleMessage: string;
  count: number;
}

type DisplayStatus = TestCase["status"] | "absent";

function statusPresentation(status: DisplayStatus) {
  switch (status) {
    case "passed":
      return { label: "Passed", color: "success.main" } as const;
    case "failed":
      return { label: "Failed", color: "error.main" } as const;
    case "skipped":
      return { label: "Skipped", color: "warning.main" } as const;
    case "absent":
      return { label: "Absent", color: "text.secondary" } as const;
  }
}

function statusMetricColor(
  status: DisplayStatus,
): "success.main" | "error.main" | "warning.main" | "text.primary" {
  if (status === "passed") return "success.main";
  if (status === "failed") return "error.main";
  if (status === "skipped") return "warning.main";
  return "text.primary";
}

const preSx = {
  m: 0,
  whiteSpace: "pre-wrap",
  overflowX: "auto",
  fontFamily: overviewTypography.data.fontFamily,
  fontSize: "13px",
  lineHeight: "20px",
} as const;

const evidenceLinkSx = {
  minHeight: { xs: 44, sm: 36 },
  display: "inline-flex",
  alignItems: "center",
  gap: 0.5,
  fontSize: "13px",
  fontWeight: 650,
} as const;

export function TestDetailPage() {
  const theme = useTheme();
  const { features } = useCapabilities();
  const manifest = useManifest();
  const { jobName: jobID, testName: encodedTestName } = useParams<{
    jobName: string;
    testName: string;
  }>();
  const testName = encodedTestName ? decodeURIComponent(encodedTestName) : "";
  const parsedTitle = parseTestDisplayName(testName);
  const { data, loading, error } = useJobDetail(jobID);
  const [searchParams, setSearchParams] = useSearchParams();
  const [stackOpenFor, setStackOpenFor] = useState<string | null>(null);

  const canonicalJobID = data?.job_id ?? jobID ?? "";
  const jobDisplayName = shortJobName(
    data?.name ?? canonicalJobID,
    manifest.short_name_prefix ?? "",
  ) || canonicalJobID;

  const occurrences = useMemo<TestOccurrence[]>(() => {
    if (!data) return [];
    return [...(data.runs ?? [])]
      .sort(
        (left, right) =>
          new Date(left.started).getTime() - new Date(right.started).getTime(),
      )
      .map((run) => ({
        run,
        testCase:
          (run.test_cases ?? []).find((testCase) => testCase.name === testName) ??
          null,
      }));
  }, [data, testName]);

  const latestOccurrence = useMemo(() => {
    for (let index = occurrences.length - 1; index >= 0; index -= 1) {
      if (occurrences[index].testCase) return occurrences[index];
    }
    return null;
  }, [occurrences]);
  const runtimeSummary = useMemo(
    () => summarizeRuntime(testRuntimePoints(data?.runs ?? [], testName)),
    [data?.runs, testName],
  );

  const requestedBuildID = searchParams.get("run");
  const effectiveSelectedID =
    requestedBuildID &&
    occurrences.some((occurrence) => occurrence.run.build_id === requestedBuildID)
      ? requestedBuildID
      : latestOccurrence?.run.build_id ?? null;
  const selectedOccurrence = useMemo(
    () =>
      occurrences.find(
        (occurrence) => occurrence.run.build_id === effectiveSelectedID,
      ) ?? null,
    [effectiveSelectedID, occurrences],
  );

  const stackOpen = Boolean(
    effectiveSelectedID && stackOpenFor === effectiveSelectedID,
  );

  const presentOccurrences = occurrences.filter(
    (occurrence) => occurrence.testCase !== null,
  );
  const failedOccurrences = presentOccurrences.filter(
    (occurrence) => occurrence.testCase?.status === "failed",
  );
  const historySummary = summarizeTestHistory(occurrences, persistentAfter(manifest));
  const classification = historySummary.classification;

  const failureGroups = useMemo<FailureGroup[]>(() => {
    const groups = new Map<string, { sample: string; count: number }>();
    for (const occurrence of failedOccurrences) {
      const message = occurrence.testCase?.failure_message;
      if (!message) continue;
      const normalized = normalizeMessage(message);
      const current = groups.get(normalized);
      if (current) current.count += 1;
      else groups.set(normalized, { sample: message, count: 1 });
    }
    return Array.from(groups.entries())
      .map(([normalizedMessage, value]) => ({
        normalizedMessage,
        sampleMessage: value.sample,
        count: value.count,
      }))
      .sort((left, right) => right.count - left.count);
  }, [failedOccurrences]);

  if (loading) return <LoadingState />;

  if (error) {
    return (
      <ErrorState
        title="Failed to load test details"
        message={error}
        onRetry={() => window.location.reload()}
      />
    );
  }

  if (!data) return null;

  const testFound = occurrences.some((occurrence) => occurrence.testCase !== null);
  if (!testFound) {
    return (
      <Stack spacing={3}>
        <Breadcrumbs separator="›" aria-label="Breadcrumb">
          <Link component={RouterLink} to="/" color="textSecondary" underline="hover">
            Overview
          </Link>
          <Link
            component={RouterLink}
            to={jobPath(canonicalJobID)}
            color="textSecondary"
            underline="hover"
          >
            {jobDisplayName}
          </Link>
          <Typography color="textPrimary" noWrap>
            {parsedTitle.displayName}
          </Typography>
        </Breadcrumbs>
        <Box
          component="section"
          sx={{
            bgcolor: "surface.container",
            borderBlock: "1px solid",
            borderColor: "divider",
            px: 2,
            py: 4,
            textAlign: "center",
          }}
        >
          <Typography component="h1" sx={overviewTypography.majorHeading}>
            Test not found
          </Typography>
          <Typography color="textSecondary" sx={{ mt: 0.5, ...overviewTypography.secondaryBody }}>
            This test is not present in the current job window.
          </Typography>
        </Box>
      </Stack>
    );
  }

  const selectedRun = selectedOccurrence?.run ?? null;
  const selectedTestCase = selectedOccurrence?.testCase ?? null;
  const latestTestCase = latestOccurrence?.testCase ?? null;
  const selectedStatus: DisplayStatus = selectedTestCase?.status ?? "absent";
  const statusView = statusPresentation(selectedStatus);
  const failureRate = historySummary.failureRate;
  const matchingFailures = selectedTestCase?.failure_message
    ? failureGroups.find(
        (group) =>
          group.normalizedMessage ===
          normalizeMessage(selectedTestCase.failure_message ?? ""),
      )?.count ?? failedOccurrences.length
    : failedOccurrences.length;
  const selectedMetadataCase = selectedTestCase ?? latestTestCase;

  const fixPatterns: PatternAnalysis[] = selectedRun
    ? (data.pattern_analyses ?? []).filter(
        (pattern) =>
          (!data.pattern_refresh || data.pattern_refresh.state === "current") &&
          patternLifecycleActive(pattern.lifecycle) &&
          pattern.systemic &&
          Boolean(pattern.id) &&
          Boolean(pattern.content_hash) &&
          Boolean(pattern.suggested_fix) &&
          meetsConfidenceFloor(
            pattern.confidence,
            features.chat_fix_min_confidence ?? "high",
          ) &&
          Boolean(pattern.shared_builds?.includes(selectedRun.build_id)),
      )
    : [];

  const selectedFileContext = selectedTestCase
    ? {
        buildLogUrl: selectedRun?.build_log_url,
        clusterArtifacts: selectedTestCase.cluster_artifacts,
        webUrl: selectedRun?.web_url,
        fileLinks: selectedTestCase.ai_analysis?.file_links,
      }
    : {};

  const traceHref =
    selectedRun && selectedTestCase && features.analysis_traces
      ? `/analysis-traces?job_id=${encodeURIComponent(canonicalJobID)}` +
        `&build_id=${encodeURIComponent(selectedRun.build_id)}` +
        `&test_name=${encodeURIComponent(testName)}`
      : undefined;

  const metricItems: MetricStripItem[] = [
    {
      label: "Result",
      value: statusView.label,
      color: statusMetricColor(selectedStatus),
    },
    {
      label: "Failure rate",
      value:
        failureRate !== null ? `${Math.round(failureRate * 100)}%` : "Not available",
      color:
        failureRate === null
          ? "text.primary"
          : failureRate > 0
            ? "error.main"
            : "success.main",
    },
    {
      label: "Runs observed",
      value: presentOccurrences.length.toLocaleString(),
    },
    {
      label: "Selected duration",
      value: selectedTestCase
        ? formatDuration(selectedTestCase.duration_seconds)
        : "Not present",
    },
    {
      label: "Median duration",
      value:
        runtimeSummary.medianSeconds !== null
          ? formatDuration(runtimeSummary.medianSeconds)
          : "Not available",
    },
    {
      label: "95th percentile",
      value:
        runtimeSummary.p95Seconds !== null
          ? formatDuration(runtimeSummary.p95Seconds)
          : "Not available",
    },
  ];

  const technicalItems = [
    {
      label: "Canonical test name",
      value: testName,
      copyLabel: "Copy canonical test name",
    },
    {
      label: "Structured labels",
      value:
        parsedTitle.labels.length > 0
          ? parsedTitle.labels.join(" ")
          : "No structured labels",
    },
    ...(parsedTitle.removedPrefixes.length > 0
      ? [
          {
            label:
              parsedTitle.removedPrefixes.length === 1
                ? "Removed suite prefix"
                : "Removed suite prefixes",
            value: parsedTitle.removedPrefixes.join(" · "),
          },
        ]
      : []),
    ...(selectedMetadataCase?.suite_name
      ? [{ label: "Suite", value: selectedMetadataCase.suite_name }]
      : []),
    ...(selectedMetadataCase?.class_name
      ? [{ label: "Class", value: selectedMetadataCase.class_name }]
      : []),
  ];
  const technicalSummary = [
    `${parsedTitle.labels.length} ${parsedTitle.labels.length === 1 ? "label" : "labels"}`,
    `${parsedTitle.removedPrefixes.length} ${parsedTitle.removedPrefixes.length === 1 ? "suite prefix" : "suite prefixes"}`,
    "canonical name",
  ].join(" · ");

  function selectRun(buildID: string) {
    setSearchParams(withJobDetailParam(searchParams, "run", buildID));
  }

  const runHistory = (
    <RunHistory
      runs={data.runs ?? []}
      selectedBuildId={effectiveSelectedID ?? undefined}
      onSelect={selectRun}
      metadata={[
        `${historySummary.failed} failed`,
        `${historySummary.passed} passed`,
        ...(historySummary.skipped > 0
          ? [`${historySummary.skipped} skipped`]
          : []),
        ...(historySummary.absent > 0
          ? [`${historySummary.absent} absent`]
          : []),
      ].join(" · ")}
      colorFn={(run) => {
        const palette = (theme.vars ?? theme).palette;
        const testCase = (run.test_cases ?? []).find(
          (candidate) => candidate.name === testName,
        );
        if (!testCase) return palette.text.disabled;
        if (testCase.status === "passed") return palette.success.main;
        if (testCase.status === "failed") return palette.error.main;
        return palette.warning.main;
      }}
      resultLabelFn={(run) => {
        const testCase = (run.test_cases ?? []).find(
          (candidate) => candidate.name === testName,
        );
        return testCase ? statusPresentation(testCase.status).label : "Absent";
      }}
    />
  );

  const runMetadata = selectedRun ? (
    <RunMetadata
      status={statusView.label}
      statusColor={
        selectedStatus === "passed"
          ? "success.main"
          : selectedStatus === "failed"
            ? "error.main"
            : selectedStatus === "skipped"
              ? "warning.main"
              : "text.secondary"
      }
      items={[
        { label: "Build ID", value: selectedRun.build_id },
        { label: "Started", value: new Date(selectedRun.started).toLocaleString() },
        {
          label: "Duration",
          value: selectedTestCase
            ? formatDuration(selectedTestCase.duration_seconds)
            : "Not present",
        },
        {
          label: "JUnit",
          value: selectedTestCase?.junit_file || "Not present",
        },
      ]}
      links={[
        ...(selectedRun.prow_url
          ? [{ label: "View in Prow", href: selectedRun.prow_url }]
          : []),
        ...(selectedRun.build_log_url
          ? [{ label: "Build log", href: selectedRun.build_log_url }]
          : []),
      ]}
    />
  ) : (
    <Box component="section" sx={{ bgcolor: "surface.container", borderBottom: "1px solid", borderColor: "divider" }}>
      <DetailSectionBand title="Run metadata" metadata="Unavailable" />
      <Typography color="textSecondary" sx={{ px: 1.5, py: 2, ...overviewTypography.secondaryBody }}>
        Select a published run to inspect its metadata.
      </Typography>
    </Box>
  );

  const analysisBriefing = selectedTestCase?.ai_analysis ? (
    <AnalysisBriefing
      title="Analysis briefing"
      icon={<AutoAwesome aria-hidden sx={{ fontSize: 18, color: "primary.main" }} />}
      metadata={`${selectedTestCase.ai_analysis.severity} severity · ${matchingFailures} ${matchingFailures === 1 ? "matching failure" : "matching failures"}`}
      summary={(
        <RichText
          text={
            selectedTestCase.ai_summary?.summary ??
            firstSentence(selectedTestCase.ai_analysis.root_cause)
          }
          fileCtx={selectedFileContext}
        />
      )}
      details={(
        <AiAnalysisPanel
          analysis={selectedTestCase.ai_analysis}
          fileCtx={selectedFileContext}
          traceHref={traceHref}
          fixPatterns={fixPatterns}
          chatRef={{
            job_id: canonicalJobID,
            build_id: selectedRun?.build_id ?? "",
            test_name: selectedTestCase.name,
            suite_name: selectedTestCase.suite_name,
            class_name: selectedTestCase.class_name,
            junit_file: selectedTestCase.junit_file,
            analysis_generated_at: selectedTestCase.ai_analysis.generated_at,
          }}
          appearance="detail"
          severityInHeader
        />
      )}
      collapseDetailsOnMobile={false}
    />
  ) : selectedTestCase?.ai_summary ? (
    <AnalysisBriefing
      title="Analysis briefing"
      icon={<AutoAwesome aria-hidden sx={{ fontSize: 18, color: "primary.main" }} />}
      metadata="Summary only"
      summary={(
        <RichText
          text={selectedTestCase.ai_summary.summary}
          fileCtx={selectedFileContext}
        />
      )}
      collapseDetailsOnMobile={false}
    />
  ) : (
    <Box component="section" sx={{ bgcolor: "surface.container", borderBottom: "1px solid", borderColor: "divider" }}>
      <DetailSectionBand title="Analysis briefing" metadata="Unavailable" />
      <Typography color="textSecondary" sx={{ px: 1.5, py: 2, ...overviewTypography.secondaryBody }}>
        {selectedTestCase?.status === "passed"
          ? "No failure analysis is needed for this passing result."
          : selectedTestCase
            ? "No accepted analysis is available for this result."
            : "Select a run where this test was reported to inspect its analysis."}
      </Typography>
    </Box>
  );

  const stackLineCount = selectedTestCase?.failure_body
    ? selectedTestCase.failure_body.split("\n").length
    : 0;
  const clusterArtifacts = selectedTestCase?.cluster_artifacts;
  const controllerLogEntries = Object.entries(
    selectedRun?.controller_log_urls ?? {},
  );
  const controllerLogsFallback =
    selectedRun?.web_url && controllerLogEntries.length === 0
      ? `${selectedRun.web_url.replace(/\/+$/u, "")}/artifacts/clusters/bootstrap/logs/`
      : null;
  const evidenceLinkCount = [
    selectedTestCase?.failure_location_url,
    selectedRun?.web_url,
    ...(selectedRun?.junit_urls ?? []),
    clusterArtifacts?.provider_activity_log,
    clusterArtifacts?.bootstrap_resources_url,
    ...Object.values(clusterArtifacts?.pod_log_dirs ?? {}),
    ...controllerLogEntries.map(([, url]) => url),
    controllerLogsFallback,
    ...((clusterArtifacts?.machines ?? []).flatMap((machine) =>
      Object.values(machine.logs),
    )),
  ].filter(Boolean).length;

  const evidenceSection =
    selectedTestCase && selectedRun &&
    (evidenceLinkCount > 0 || selectedTestCase.failure_location) ? (
      <Box component="section" sx={{ bgcolor: "surface.container", borderBottom: "1px solid", borderColor: "divider" }}>
        <DetailSectionBand
          title="Files and evidence"
          metadata={`${evidenceLinkCount} ${evidenceLinkCount === 1 ? "link" : "links"}`}
        />
        <Box
          sx={{
            minHeight: 44,
            display: "flex",
            alignItems: "center",
            flexWrap: "wrap",
            gap: 2,
            px: 1.5,
            py: 0.5,
            borderTop: "1px solid",
            borderColor: "divider",
          }}
        >
          {selectedTestCase.failure_location_url ? (
            <Link
              href={selectedTestCase.failure_location_url}
              target="_blank"
              rel="noopener noreferrer"
              sx={evidenceLinkSx}
            >
              <Place sx={{ fontSize: 15 }} /> Failure location <OpenInNew sx={{ fontSize: 14 }} />
            </Link>
          ) : selectedTestCase.failure_location ? (
            <Typography component="code" color="textSecondary" sx={{ ...overviewTypography.data, overflowWrap: "anywhere" }}>
              {selectedTestCase.failure_location}
            </Typography>
          ) : null}
          {selectedRun.web_url && (
            <Link href={selectedRun.web_url} target="_blank" rel="noopener noreferrer" sx={evidenceLinkSx}>
              <Inventory2 sx={{ fontSize: 15 }} /> Run artifacts <OpenInNew sx={{ fontSize: 14 }} />
            </Link>
          )}
          {(selectedRun.junit_urls ?? []).map((url, index) => (
            <Link key={url} href={url} target="_blank" rel="noopener noreferrer" sx={evidenceLinkSx}>
              <Assignment sx={{ fontSize: 15 }} /> JUnit artifact {index + 1} <OpenInNew sx={{ fontSize: 14 }} />
            </Link>
          ))}
          {clusterArtifacts?.provider_activity_log && (
            <Link
              href={clusterArtifacts.provider_activity_log}
              target="_blank"
              rel="noopener noreferrer"
              sx={evidenceLinkSx}
            >
              <Cloud sx={{ fontSize: 15 }} /> Provider activity <OpenInNew sx={{ fontSize: 14 }} />
            </Link>
          )}
          {clusterArtifacts?.bootstrap_resources_url && (
            <Link
              href={clusterArtifacts.bootstrap_resources_url}
              target="_blank"
              rel="noopener noreferrer"
              sx={evidenceLinkSx}
            >
              <Assignment sx={{ fontSize: 15 }} /> Cluster resources <OpenInNew sx={{ fontSize: 14 }} />
            </Link>
          )}
          {Object.entries(clusterArtifacts?.pod_log_dirs ?? {}).map(
            ([directory, url]) => (
              <Link key={directory} href={url} target="_blank" rel="noopener noreferrer" sx={evidenceLinkSx}>
                <Inventory2 sx={{ fontSize: 15 }} /> {directory} <OpenInNew sx={{ fontSize: 14 }} />
              </Link>
            ),
          )}
          {controllerLogEntries.map(([controller, url]) => (
              <Link key={controller} href={url} target="_blank" rel="noopener noreferrer" sx={evidenceLinkSx}>
                <Dns sx={{ fontSize: 15 }} /> {controller} <OpenInNew sx={{ fontSize: 14 }} />
              </Link>
            ),
          )}
          {controllerLogsFallback && (
            <Link
              href={controllerLogsFallback}
              target="_blank"
              rel="noopener noreferrer"
              sx={evidenceLinkSx}
            >
              <Dns sx={{ fontSize: 15 }} /> Controller logs <OpenInNew sx={{ fontSize: 14 }} />
            </Link>
          )}
        </Box>
        {clusterArtifacts?.machines && clusterArtifacts.machines.length > 0 && (
          <Accordion
            disableGutters
            elevation={0}
            square
            sx={{
              bgcolor: "transparent",
              borderTop: "1px solid",
              borderColor: "divider",
              "&:before": { display: "none" },
            }}
          >
            <AccordionSummary
              expandIcon={<ChevronRight sx={{ fontSize: 18 }} />}
              sx={{
                minHeight: 44,
                px: 1.5,
                "& .MuiAccordionSummary-content": { my: 0.75 },
                "& .MuiAccordionSummary-expandIconWrapper.Mui-expanded": {
                  transform: "rotate(90deg)",
                },
              }}
            >
              <Typography sx={{ ...overviewTypography.secondaryBody, fontWeight: 650 }}>
                Machine logs ({clusterArtifacts.machines.length} machines)
              </Typography>
            </AccordionSummary>
            <AccordionDetails sx={{ px: 1.5, pt: 0 }}>
              <Stack spacing={1}>
                {clusterArtifacts.machines.map((machine) => (
                  <Box key={machine.name}>
                    <Typography component="code" color="textSecondary" sx={overviewTypography.data}>
                      {machine.name}
                    </Typography>
                    <Box sx={{ display: "flex", flexWrap: "wrap", gap: 1.5, mt: 0.5 }}>
                      {Object.entries(machine.logs).map(([logType, url]) => (
                        <Link key={logType} href={url} target="_blank" rel="noopener noreferrer" sx={evidenceLinkSx}>
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
    ) : null;

  const headerMetadata = [
    selectedMetadataCase?.suite_name || "Kubernetes test",
    selectedRun
      ? `selected run ${new Date(selectedRun.started).toLocaleDateString()}`
      : null,
    classification,
  ].filter(Boolean).join(" · ");

  return (
    <Stack
      spacing={{ xs: 2.5, sm: 3.5 }}
      sx={{ minWidth: 0, maxWidth: "100%", overflowX: "clip" }}
    >
      <Box
        component="nav"
        aria-label="Breadcrumb"
        sx={{ display: { xs: "block", sm: "none" } }}
      >
        <Link
          component={RouterLink}
          to={
            effectiveSelectedID
              ? jobRunPath(canonicalJobID, effectiveSelectedID)
              : jobPath(canonicalJobID)
          }
          underline="none"
          sx={{ fontSize: "13px", fontWeight: 650 }}
        >
          ← {jobDisplayName}
        </Link>
      </Box>
      <Breadcrumbs
        separator="›"
        aria-label="Breadcrumb"
        sx={{ display: { xs: "none", sm: "flex" }, ...overviewTypography.description }}
      >
        <Link component={RouterLink} to="/" underline="none" color="textSecondary">
          Overview
        </Link>
        <Link
          component={RouterLink}
          to={
            effectiveSelectedID
              ? jobRunPath(canonicalJobID, effectiveSelectedID)
              : jobPath(canonicalJobID)
          }
          underline="none"
          color="textSecondary"
        >
          {jobDisplayName}
        </Link>
        <Typography color="textPrimary" noWrap sx={{ maxWidth: 420 }}>
          {parsedTitle.displayName}
        </Typography>
      </Breadcrumbs>

      <Box
        sx={{
          display: "grid",
          gridTemplateColumns: { xs: "minmax(0, 1fr)", sm: "minmax(0, 1fr) auto" },
          alignItems: "start",
          gap: { xs: 1, sm: 2 },
        }}
      >
        <Box sx={{ minWidth: 0 }}>
          <Typography
            component="h1"
            title={testName}
            aria-label={parsedTitle.usedFallback ? testName : undefined}
            sx={{
              ...overviewTypography.pageHeadline,
              color: "text.primary",
              fontSize: parsedTitle.usedFallback
                ? { xs: "22px", sm: "26px", md: "28px" }
                : { xs: "26px", sm: "30px" },
              lineHeight: parsedTitle.usedFallback
                ? { xs: "29px", sm: "34px", md: "36px" }
                : { xs: "33px", sm: "38px" },
              fontWeight: 720,
              display: parsedTitle.usedFallback ? "-webkit-box" : "block",
              WebkitBoxOrient: parsedTitle.usedFallback ? "vertical" : undefined,
              WebkitLineClamp: parsedTitle.usedFallback ? { xs: 3, sm: 2 } : undefined,
              overflow: "hidden",
              overflowWrap: "anywhere",
            }}
          >
            {parsedTitle.displayName}
          </Typography>
          <Typography
            component="p"
            color="textSecondary"
            sx={{ m: 0, mt: 0.75, ...overviewTypography.secondaryBody }}
          >
            {headerMetadata}
          </Typography>
        </Box>
        <Box
          role="status"
          sx={{
            minHeight: 34,
            display: "inline-flex",
            alignItems: "center",
            gap: 1,
            color: statusView.color,
            fontSize: "14px",
            lineHeight: "20px",
            fontWeight: 700,
            whiteSpace: "nowrap",
          }}
        >
          <Box component="span" sx={{ width: 8, height: 8, borderRadius: "2px", bgcolor: "currentColor" }} />
          {statusView.label}
        </Box>
      </Box>

      <TechnicalIdentity
        summary={technicalSummary}
        items={technicalItems}
      />

      <MetricStrip items={metricItems} label="Test metrics" />

      <Box
        sx={{
          display: "grid",
          minWidth: 0,
          gridTemplateColumns: {
            xs: "minmax(0, 1fr)",
            lg: "minmax(0, 1.5fr) minmax(360px, 0.85fr)",
          },
          gap: 2,
          alignItems: "start",
        }}
      >
        <Stack spacing={2} sx={{ minWidth: 0 }}>
          {analysisBriefing}

          {failureGroups.length > 0 && (
            <Box component="section" sx={{ bgcolor: "surface.container", borderBottom: "1px solid", borderColor: "divider" }}>
              <DetailSectionBand
                title="Failure patterns"
                metadata={`${failureGroups.length} ${failureGroups.length === 1 ? "pattern" : "patterns"}`}
              />
              {failureGroups.map((group) => (
                <Box
                  key={group.normalizedMessage}
                  sx={{
                    display: "grid",
                    gridTemplateColumns: "96px minmax(0, 1fr)",
                    gap: 1.5,
                    px: 1.5,
                    py: 1,
                    borderTop: "1px solid",
                    borderColor: "divider",
                  }}
                >
                  <Typography color="error" sx={{ ...overviewTypography.data, fontWeight: 700 }}>
                    {group.count} of {failedOccurrences.length}
                  </Typography>
                  <Typography
                    color="textSecondary"
                    title={group.sampleMessage}
                    sx={{
                      ...overviewTypography.description,
                      display: "-webkit-box",
                      WebkitBoxOrient: "vertical",
                      WebkitLineClamp: 2,
                      overflow: "hidden",
                    }}
                  >
                    {group.sampleMessage}
                  </Typography>
                </Box>
              ))}
            </Box>
          )}

          {selectedTestCase?.failure_message && (
            <Box component="section" sx={{ bgcolor: "surface.container", borderBottom: "1px solid", borderColor: "divider" }}>
              <DetailSectionBand title="Failure evidence" metadata="Selected run" />
              <Box
                component="pre"
                sx={{
                  ...preSx,
                  px: 1.5,
                  py: 1.5,
                  borderTop: "1px solid",
                  borderColor: "divider",
                  color: "error.main",
                  bgcolor: "surface.containerHigh",
                }}
              >
                {selectedTestCase.failure_message}
              </Box>
            </Box>
          )}

          {selectedTestCase?.failure_body && (
            <Box component="section" sx={{ bgcolor: "surface.container", borderBottom: "1px solid", borderColor: "divider" }}>
              <DetailSectionBand
                title="Stack trace"
                metadata={`${stackOpen ? "Expanded" : "Collapsed"} · ${stackLineCount} ${stackLineCount === 1 ? "line" : "lines"}`}
              />
              <ButtonBase
                type="button"
                onClick={() =>
                  setStackOpenFor(stackOpen ? null : effectiveSelectedID)
                }
                aria-expanded={stackOpen}
                aria-controls="test-stack-trace"
                sx={{
                  width: "100%",
                  minHeight: 44,
                  px: 1.5,
                  py: 0.75,
                  justifyContent: "flex-start",
                  color: "text.primary",
                  textAlign: "left",
                  borderTop: "1px solid",
                  borderColor: "divider",
                  "&:hover": { bgcolor: "surface.containerHigh" },
                  "&.Mui-focusVisible": {
                    outline: "2px solid",
                    outlineColor: "primary.main",
                    outlineOffset: -2,
                  },
                }}
              >
                <Typography sx={{ ...overviewTypography.secondaryBody, fontWeight: 650 }}>
                  {stackOpen ? "Hide stack trace" : "Show stack trace"}
                </Typography>
                <ChevronRight
                  sx={{
                    ml: "auto",
                    color: "text.secondary",
                    transform: stackOpen ? "rotate(90deg)" : "rotate(0deg)",
                    transition: (currentTheme) =>
                      currentTheme.transitions.create("transform", {
                        duration: currentTheme.transitions.duration.shortest,
                      }),
                    "@media (prefers-reduced-motion: reduce)": { transition: "none" },
                  }}
                />
              </ButtonBase>
              <Collapse in={stackOpen} timeout="auto" unmountOnExit>
                <Box
                  id="test-stack-trace"
                  component="pre"
                  sx={{
                    ...preSx,
                    px: 1.5,
                    py: 1.5,
                    borderTop: "1px solid",
                    borderColor: "divider",
                    color: "text.secondary",
                  }}
                >
                  {highlightStackTrace(selectedTestCase.failure_body)}
                </Box>
              </Collapse>
            </Box>
          )}

          {evidenceSection}
        </Stack>

        <Stack
          spacing={2}
          sx={{
            minWidth: 0,
            position: { lg: "sticky" },
            top: { lg: 80 },
            alignSelf: "start",
          }}
        >
          {runHistory}
          <RuntimeTrend
            summary={runtimeSummary}
            subject={parsedTitle.displayName}
            runHref={(buildID) => testRunPath(canonicalJobID, testName, buildID)}
          />
          {runMetadata}
        </Stack>
      </Box>
    </Stack>
  );
}
