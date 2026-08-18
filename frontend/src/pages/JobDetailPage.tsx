import { useMemo, useState, type ReactNode } from "react";
import Box from "@mui/material/Box";
import Breadcrumbs from "@mui/material/Breadcrumbs";
import ButtonBase from "@mui/material/ButtonBase";
import Collapse from "@mui/material/Collapse";
import Link from "@mui/material/Link";
import Stack from "@mui/material/Stack";
import Typography from "@mui/material/Typography";
import {
  ChevronRight,
  ErrorOutlined,
  HourglassEmpty,
  OpenInNew,
  WarningAmber,
} from "@mui/icons-material";
import { Link as RouterLink, useParams, useSearchParams } from "react-router-dom";
import { useJobDetail } from "../hooks/useData";
import {
  formatDuration,
  formatPercent,
  shortJobName,
  timeAgo,
} from "../lib/utils";
import type { BuildResult } from "../types/dashboard";
import { RunHistory } from "../components/RunHistory";
import { TestResultsGrid } from "../components/TestResultsGrid";
import { TestCaseTable } from "../components/TestCaseTable";
import { PatternBanner } from "../components/PatternBanner";
import { DetailSectionBand } from "../components/DetailSectionBand";
import { LoadingState } from "../components/LoadingState";
import { ErrorState } from "../components/ErrorState";
import { emptyTestResultsPresentation } from "../lib/testResults";
import { BuildFailurePanel } from "../components/BuildFailurePanel";
import { buildFailure as findBuildFailure } from "../lib/buildFailures";
import { useSharedFetchStatus } from "../hooks/useSharedFetchStatus";
import { useManifest } from "../hooks/useManifest";
import { MetricStrip, type MetricStripItem } from "../components/MetricStrip";
import { TechnicalIdentity } from "../components/TechnicalIdentity";
import { RunMetadata } from "../components/RunMetadata";
import { ResultLedger } from "../components/ResultLedger";
import { RuntimeTrend } from "../components/RuntimeTrend";
import { overviewTypography } from "../theme/overview";
import {
  currentJobStatus,
  executedResultTests,
  filterResultTests,
  normalizeResultLedgerFilter,
  recentJobPassRate,
  sortResultTests,
  summarizeResultTests,
  withJobDetailParam,
} from "../lib/jobDetail";
import { jobRuntimePoints, summarizeRuntime } from "../lib/runtimeTrend";
import { initialProgressiveCount, nextProgressiveCount } from "../lib/progressive";

function passRateColor(
  rate: number,
): "success.main" | "warning.main" | "error.main" {
  if (rate >= 0.9) return "success.main";
  if (rate <= 0.3) return "error.main";
  return "warning.main";
}

function statusPresentation(status: "UNKNOWN" | "RUNNING" | "PASSING" | "FAILING") {
  switch (status) {
    case "PASSING":
      return { label: "Passing", color: "success.main" } as const;
    case "RUNNING":
      return { label: "Running", color: "warning.main" } as const;
    case "FAILING":
      return { label: "Failing", color: "error.main" } as const;
    case "UNKNOWN":
      return { label: "Unknown", color: "text.primary" } as const;
  }
}

function runResultLabel(run: BuildResult): string {
  if (run.result === "PENDING") return "Running";
  return run.passed ? "Passed" : "Failed";
}

function runResultColor(run: BuildResult): "warning.main" | "success.main" | "error.main" {
  if (run.result === "PENDING") return "warning.main";
  return run.passed ? "success.main" : "error.main";
}

export function JobDetailPrimaryLayout({
  patternAnalysis,
  buildFailureAnalysis,
  runHistory,
  runMetadata,
}: {
  patternAnalysis?: ReactNode;
  buildFailureAnalysis?: ReactNode;
  runHistory: ReactNode;
  runMetadata: ReactNode;
}) {
  if (!patternAnalysis && !buildFailureAnalysis) {
    return (
      <Box
        sx={{
          display: "grid",
          gridTemplateColumns: {
            xs: "minmax(0, 1fr)",
            lg: "minmax(0, 1fr) minmax(360px, 0.8fr)",
          },
          gap: 2,
          minWidth: 0,
          alignItems: "start",
        }}
      >
        {runHistory}
        {runMetadata}
      </Box>
    );
  }

  return (
    <Box
      sx={{
        display: "grid",
        gridTemplateColumns: {
          xs: "minmax(0, 1fr)",
          lg: "minmax(0, 1.5fr) minmax(360px, 0.85fr)",
        },
        gap: 2,
        minWidth: 0,
        alignItems: "start",
      }}
    >
      <Stack spacing={2} sx={{ minWidth: 0 }}>
        {patternAnalysis}
        {buildFailureAnalysis}
      </Stack>
      <Box
        sx={{
          display: "flex",
          flexDirection: "column",
          gap: 2,
          minWidth: 0,
          position: { lg: "sticky" },
          top: { lg: 80 },
          alignSelf: "start",
        }}
      >
        {runHistory}
        {runMetadata}
      </Box>
    </Box>
  );
}

export function JobDetailPage() {
  const { jobName: jobID } = useParams<{ jobName: string }>();
  const [searchParams, setSearchParams] = useSearchParams();
  const [gridOpen, setGridOpen] = useState(false);
  const { data, loading, error } = useJobDetail(jobID);
  const fetchStatus = useSharedFetchStatus();
  const manifest = useManifest();

  const runs = useMemo(() => data?.runs ?? [], [data]);
  const canonicalJobID = data?.job_id ?? jobID ?? "";
  const shortenedName = shortJobName(
    data?.name ?? canonicalJobID,
    manifest.short_name_prefix ?? "",
  );
  const displayName = shortenedName || canonicalJobID;

  const selectedBuildId = searchParams.get("run") ?? runs[0]?.build_id;
  const selectedRun = useMemo(
    () => runs.find((run) => run.build_id === selectedBuildId),
    [runs, selectedBuildId],
  );
  const resultSummary = useMemo(
    () => summarizeResultTests(selectedRun?.test_cases ?? []),
    [selectedRun],
  );
  const selectedTestCases = resultSummary.visible;
  const buildFailure = findBuildFailure(selectedRun?.test_cases ?? []);
  const hasJUnitCases = runs.some(
    (run) => executedResultTests(run.test_cases).length > 0,
  );
  const emptyTestResults = selectedRun
    ? emptyTestResultsPresentation(selectedRun)
    : null;

  const passRateRecent = useMemo(
    () => data?.pass_rate_recent ?? recentJobPassRate(runs),
    [data?.pass_rate_recent, runs],
  );

  const runtimeSummary = useMemo(
    () => summarizeRuntime(jobRuntimePoints(runs)),
    [runs],
  );

  const selectedFilter = normalizeResultLedgerFilter(searchParams.get("results"));
  const resultQuery = searchParams.get("test") ?? "";
  const visibleTestCases = useMemo(
    () =>
      sortResultTests(
        filterResultTests(selectedTestCases, selectedFilter, resultQuery),
      ),
    [resultQuery, selectedFilter, selectedTestCases],
  );
  const resultWindowKey = `${selectedBuildId ?? ""}:${selectedFilter}:${resultQuery}`;
  const resultBatchSize = 50;
  const [resultWindow, setResultWindow] = useState({ key: "", count: resultBatchSize });
  const renderedResultCount =
    resultWindow.key === resultWindowKey
      ? Math.min(visibleTestCases.length, resultWindow.count)
      : initialProgressiveCount(visibleTestCases.length, resultBatchSize);
  const renderedTestCases = visibleTestCases.slice(0, renderedResultCount);

  function showMoreResults() {
    setResultWindow({
      key: resultWindowKey,
      count: nextProgressiveCount(
        renderedResultCount,
        visibleTestCases.length,
        resultBatchSize,
      ),
    });
  }

  function updateSearchParam(
    name: string,
    value: string | null,
    options?: { replace?: boolean },
  ) {
    setSearchParams(withJobDetailParam(searchParams, name, value), options);
  }

  function handleSelectRun(buildID: string) {
    updateSearchParam("run", buildID);
  }

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

  const lastRun = runs[0] ?? null;
  const pattern = data.pattern_analyses?.[0];
  const hasRuns = runs.length > 0;
  const currentStatusView = statusPresentation(currentJobStatus(data.current_status, runs));
  const recoveryStreak = pattern?.lifecycle?.recovery_streak;
  const metricItems: MetricStripItem[] = [
    {
      label: "Current",
      value: currentStatusView.label,
      color: currentStatusView.color,
    },
    {
      label: "Last 10 runs",
      value: passRateRecent !== null ? formatPercent(passRateRecent) : "Not available",
      color: passRateRecent !== null ? passRateColor(passRateRecent) : "text.primary",
    },
    ...(recoveryStreak !== undefined
      ? [{ label: "Recovery streak", value: recoveryStreak.toLocaleString() } satisfies MetricStripItem]
      : []),
    { label: "Runs", value: runs.length.toLocaleString() },
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
    {
      label: "Last run",
      value: lastRun ? timeAgo(lastRun.started) : "No runs",
    },
  ];

  const runHistory = (
    <RunHistory
      runs={runs}
      selectedBuildId={selectedBuildId}
      onSelect={handleSelectRun}
      metadata={`${runs.length} recent ${runs.length === 1 ? "run" : "runs"}`}
    />
  );

  const runMetadata = selectedRun ? (
    <RunMetadata
      status={runResultLabel(selectedRun)}
      statusColor={runResultColor(selectedRun)}
      items={[
        { label: "Build ID", value: selectedRun.build_id },
        {
          label: "Started",
          value: new Date(selectedRun.started).toLocaleString(),
        },
        {
          label: "Duration",
          value:
            selectedRun.result === "PENDING"
              ? "Running"
              : formatDuration(selectedRun.duration_seconds),
        },
        {
          label: "Commit",
          value: selectedRun.commit ? selectedRun.commit.slice(0, 8) : "Not available",
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
  ) : null;

  const crossRunGrid = hasJUnitCases ? (
    <Box
      component="section"
      sx={{
        bgcolor: "surface.container",
        borderBottom: "1px solid",
        borderColor: "divider",
      }}
    >
      <DetailSectionBand
        title="Cross-run test grid"
        metadata={`${runs.length} ${runs.length === 1 ? "run" : "runs"} · executed tests`}
      />
      <ButtonBase
        type="button"
        onClick={() => setGridOpen((value) => !value)}
        aria-expanded={gridOpen}
        aria-controls="cross-run-test-grid"
        sx={{
          width: "100%",
          minHeight: 48,
          px: 1.5,
          py: 0.75,
          justifyContent: "flex-start",
          gap: 1,
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
        <Typography
          component="span"
          sx={{ ...overviewTypography.secondaryBody, fontWeight: 650 }}
        >
          Compare test outcomes across runs
        </Typography>
        <Typography
          component="span"
          color="text.secondary"
          sx={{ ml: "auto", ...overviewTypography.data }}
        >
          {gridOpen ? "Hide grid" : "Show grid"}
        </Typography>
        <ChevronRight
          sx={{
            flexShrink: 0,
            color: "text.secondary",
            transform: gridOpen ? "rotate(90deg)" : "rotate(0deg)",
            transition: (theme) =>
              theme.transitions.create("transform", {
                duration: theme.transitions.duration.shortest,
              }),
            "@media (prefers-reduced-motion: reduce)": { transition: "none" },
          }}
        />
      </ButtonBase>
      <Collapse in={gridOpen} timeout="auto" unmountOnExit>
        <Box id="cross-run-test-grid" sx={{ borderTop: "1px solid", borderColor: "divider" }}>
          <TestResultsGrid runs={runs} jobID={canonicalJobID} />
        </Box>
      </Collapse>
    </Box>
  ) : null;

  const buildFailureBriefing = selectedRun && buildFailure ? (
    <BuildFailurePanel
      jobID={canonicalJobID}
      run={selectedRun}
      failure={buildFailure}
      fetchStatus={fetchStatus}
    />
  ) : null;

  const patternAnalysis = pattern ? (
    <PatternBanner
      pattern={pattern}
      jobID={canonicalJobID}
      runs={runs}
      refreshStatus={data.pattern_refresh}
    />
  ) : null;

  return (
    <Box
      sx={{
        display: "flex",
        flexDirection: "column",
        gap: { xs: 2.5, sm: 3.5 },
        minWidth: 0,
      }}
    >
      <Breadcrumbs
        separator="›"
        aria-label="Breadcrumb"
        sx={{ color: "text.secondary", ...overviewTypography.description }}
      >
        <Link
          component={RouterLink}
          to="/"
          underline="none"
          sx={{ color: "text.secondary", "&:hover": { color: "primary.main" } }}
        >
          Overview
        </Link>
        <Typography variant="inherit" color="text.primary" noWrap>
          {displayName}
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
            sx={{
              ...overviewTypography.pageHeadline,
              fontSize: { xs: "26px", sm: "30px" },
              lineHeight: { xs: "33px", sm: "38px" },
              fontWeight: 720,
              color: "text.primary",
              overflowWrap: "anywhere",
            }}
          >
            {displayName}
          </Typography>
          <Typography
            component="p"
            color="text.secondary"
            sx={{ m: 0, mt: 0.75, ...overviewTypography.secondaryBody }}
          >
            {lastRun ? `Last run ${timeAgo(lastRun.started)} · ` : ""}
            {data.job_type} job
          </Typography>
        </Box>
        {currentStatusView && (
          <Box
            role="status"
            sx={{
              minHeight: 34,
              display: "inline-flex",
              alignItems: "center",
              gap: 1,
              color: currentStatusView.color,
              fontSize: "14px",
              lineHeight: "20px",
              fontWeight: 700,
              whiteSpace: "nowrap",
            }}
          >
            <Box component="span" sx={{ width: 8, height: 8, borderRadius: "2px", bgcolor: "currentColor" }} />
            {currentStatusView.label}
          </Box>
        )}
      </Box>

      <TechnicalIdentity
        desktopInline
        summary="Canonical job ID"
        items={[
          {
            label: "Canonical job ID",
            value: canonicalJobID,
            copyLabel: "Copy canonical job ID",
          },
        ]}
      />

      <MetricStrip items={metricItems} label="Job metrics" />

      {!hasRuns ? (
        <>
          {pattern && (
            <PatternBanner
              pattern={pattern}
              jobID={canonicalJobID}
              runs={runs}
              refreshStatus={data.pattern_refresh}
            />
          )}
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
            <Typography component="h2" sx={overviewTypography.majorHeading}>
              No runs found
            </Typography>
            <Typography color="text.secondary" sx={{ mt: 0.5, ...overviewTypography.secondaryBody }}>
              This job has no recorded builds in the current window.
            </Typography>
          </Box>
        </>
      ) : (
        <>
          <JobDetailPrimaryLayout
            patternAnalysis={patternAnalysis}
            buildFailureAnalysis={buildFailureBriefing}
            runHistory={runHistory}
            runMetadata={runMetadata}
          />

          <RuntimeTrend summary={runtimeSummary} subject={displayName} />

          {crossRunGrid}

          {selectedRun && resultSummary.executed.length > 0 ? (
            <ResultLedger
              filter={selectedFilter}
              query={resultQuery}
              executedCount={resultSummary.executed.length}
              skippedCount={selectedRun.tests_skipped}
              hiddenSuccessfulSetupTeardown={resultSummary.hiddenSuccessfulSetupTeardown}
              matchedCount={visibleTestCases.length}
              renderedCount={renderedResultCount}
              onFilterChange={(filter) => updateSearchParam("results", filter)}
              onQueryChange={(query) =>
                updateSearchParam("test", query || null, { replace: true })
              }
              onShowMore={
                renderedResultCount < visibleTestCases.length
                  ? showMoreResults
                  : undefined
              }
              showMoreCount={Math.min(
                resultBatchSize,
                visibleTestCases.length - renderedResultCount,
              )}
            >
              {visibleTestCases.length > 0 ? (
                <TestCaseTable
                  key={resultWindowKey}
                  testCases={renderedTestCases}
                  jobID={canonicalJobID}
                  buildId={selectedRun.build_id}
                  buildLogUrl={selectedRun.build_log_url}
                  webUrl={selectedRun.web_url}
                />
              ) : (
                <Typography
                  role="status"
                  color="text.secondary"
                  sx={{ px: 1.5, py: 3, ...overviewTypography.secondaryBody }}
                >
                  No executed tests match the current status and text filters.
                </Typography>
              )}
            </ResultLedger>
          ) : selectedRun && !buildFailure ? (
            <Box
              component="section"
              sx={{ bgcolor: "surface.container", borderBottom: "1px solid", borderColor: "divider" }}
            >
              <DetailSectionBand title="Test results" metadata="No executed tests" />
              <Box sx={{ px: 2, py: 4, textAlign: "center" }}>
                {emptyTestResults?.kind === "pending" ? (
                  <Box sx={{ display: "flex", alignItems: "center", justifyContent: "center", gap: 1 }}>
                    <HourglassEmpty sx={{ fontSize: 20, color: "text.secondary" }} />
                    <Typography color="text.secondary" sx={overviewTypography.secondaryBody}>
                      {emptyTestResults.detail}
                    </Typography>
                  </Box>
                ) : (
                  <Box sx={{ display: "flex", flexDirection: "column", alignItems: "center", gap: 1 }}>
                    <Box sx={{ display: "flex", alignItems: "center", gap: 1 }}>
                      {emptyTestResults?.severity === "error" ? (
                        <ErrorOutlined color="error" sx={{ fontSize: 20 }} />
                      ) : emptyTestResults?.severity === "warning" ? (
                        <WarningAmber color="warning" sx={{ fontSize: 20 }} />
                      ) : null}
                      <Typography component="h3" sx={overviewTypography.categoryHeading}>
                        {emptyTestResults?.title ?? "No test cases available"}
                      </Typography>
                    </Box>
                    <Typography color="text.secondary" sx={overviewTypography.secondaryBody}>
                      {emptyTestResults?.detail ?? "No test cases are available for this run."}
                    </Typography>
                    {selectedRun.build_log_url && (
                      <Link
                        href={selectedRun.build_log_url}
                        target="_blank"
                        rel="noopener noreferrer"
                        sx={{ minHeight: 36, display: "inline-flex", alignItems: "center", gap: 0.5 }}
                      >
                        Open build log <OpenInNew sx={{ fontSize: 16 }} />
                      </Link>
                    )}
                  </Box>
                )}
              </Box>
            </Box>
          ) : null}
        </>
      )}
    </Box>
  );
}
