import { useMemo, useState } from "react";
import Box from "@mui/material/Box";
import Breadcrumbs from "@mui/material/Breadcrumbs";
import Button from "@mui/material/Button";
import Chip from "@mui/material/Chip";
import Collapse from "@mui/material/Collapse";
import Link from "@mui/material/Link";
import Typography from "@mui/material/Typography";
import { ChevronRight, HourglassEmpty, OpenInNew } from "@mui/icons-material";
import { Link as RouterLink, useParams, useSearchParams } from "react-router-dom";
import { useJobDetail } from "../hooks/useData";
import { formatDuration, formatPercent, timeAgo } from "../lib/utils";
import type { BuildResult, TestCase } from "../types/dashboard";
import { RunTimeline } from "../components/RunTimeline";
import { TestResultsGrid } from "../components/TestResultsGrid";
import { TestCaseTable } from "../components/TestCaseTable";
import { PatternBanner } from "../components/PatternBanner";
import { StatusChip } from "../components/StatusChip";
import { SectionHeading } from "../components/SectionHeading";
import { Panel } from "../components/Panel";
import { LoadingState } from "../components/LoadingState";
import { ErrorState } from "../components/ErrorState";
import { soft } from "../theme";

function passRateColor(rate: number): "success" | "warning" | "error" {
  if (rate >= 0.9) return "success";
  if (rate <= 0.3) return "error";
  return "warning";
}

// Derive the job's overall status from its recent pass rate, matching the
// backend thresholds in aggregator.computeOverallStatus (>=0.9 PASSING,
// <=0.3 FAILING, else FLAKY).
function deriveJobStatus(rate: number | null): "PASSING" | "FLAKY" | "FAILING" | null {
  if (rate === null) return null;
  if (rate >= 0.9) return "PASSING";
  if (rate <= 0.3) return "FAILING";
  return "FLAKY";
}

interface StatTile {
  label: string;
  value: string;
  color?: string;
}

// A single KPI tile: uppercase label over a large mono metric.
function MetricTile({ tile }: { tile: StatTile }) {
  return (
    <Panel sx={{ borderRadius: "12px", px: 1.75, py: 1.25 }}>
      <Typography
        variant="label"
        color="text.secondary"
        sx={{ display: "block", textTransform: "uppercase", fontSize: "0.625rem" }}
      >
        {tile.label}
      </Typography>
      <Typography
        variant="stat"
        component="span"
        sx={{ fontSize: "1.375rem", color: tile.color ?? "text.primary" }}
      >
        {tile.value}
      </Typography>
    </Panel>
  );
}

// Metadata for a single build. "row" spreads the fields into a horizontal
// strip; "column" packs them two-up to fill the sidebar rail.
function RunDetailsPanel({ run, orientation }: { run: BuildResult; orientation: "row" | "column" }) {
  const isPending = run.result === "PENDING";
  return (
    <Panel component="section" sx={{ borderRadius: 3, p: { xs: 2, sm: 3 } }}>
      <Box sx={{ mb: 2, display: "flex", alignItems: "center", gap: 1.5, flexWrap: "wrap" }}>
        <Typography variant="headline" component="h3" sx={{ fontSize: "1rem" }}>
          Run Details
        </Typography>
        {isPending ? (
          <Chip
            size="small"
            label="In Progress"
            sx={{
              bgcolor: (t) => soft(t, "primary", 0.15),
              color: "primary.main",
              fontWeight: 600,
            }}
          />
        ) : (
          <StatusChip
            status={run.passed ? "passed" : "failed"}
            label={run.passed ? "Passed" : "Failed"}
          />
        )}
        <Box sx={{ ml: "auto", display: "flex", alignItems: "center", gap: 2, flexWrap: "wrap" }}>
          {run.prow_url && (
            <Link
              href={run.prow_url}
              target="_blank"
              rel="noopener noreferrer"
              sx={{ display: "inline-flex", alignItems: "center", gap: 0.5, color: "primary.main", fontSize: "0.875rem" }}
            >
              View in Prow <OpenInNew sx={{ fontSize: 16 }} />
            </Link>
          )}
          {run.build_log_url && (
            <Link
              href={run.build_log_url}
              target="_blank"
              rel="noopener noreferrer"
              sx={{ display: "inline-flex", alignItems: "center", gap: 0.5, color: "primary.main", fontSize: "0.875rem" }}
            >
              Build Log <OpenInNew sx={{ fontSize: 16 }} />
            </Link>
          )}
        </Box>
      </Box>

      <Box
        sx={{
          display: "grid",
          gridTemplateColumns:
            orientation === "row"
              ? { xs: "1fr 1fr", sm: "repeat(3, minmax(0, 1fr))", lg: "repeat(5, minmax(0, 1fr))" }
              : { xs: "1fr", sm: "repeat(2, minmax(0, 1fr))" },
          columnGap: 4,
          rowGap: 1.5,
        }}
      >
        <Box>
          <Typography variant="label" color="text.secondary">
            Build ID
          </Typography>
          <Typography variant="data" component="p" color="text.primary" sx={{ overflowWrap: "anywhere" }}>
            {run.build_id}
          </Typography>
        </Box>
        <Box>
          <Typography variant="label" color="text.secondary">
            Started
          </Typography>
          <Typography variant="body2" color="text.primary">
            {new Date(run.started).toLocaleString()}
          </Typography>
        </Box>
        <Box>
          <Typography variant="label" color="text.secondary">
            Finished
          </Typography>
          <Typography variant="body2" color="text.primary">
            {isPending ? "Still running…" : new Date(run.finished).toLocaleString()}
          </Typography>
        </Box>
        <Box>
          <Typography variant="label" color="text.secondary">
            Duration
          </Typography>
          <Typography variant="data" component="p" color="text.primary">
            {isPending ? "—" : formatDuration(run.duration_seconds)}
          </Typography>
        </Box>
        <Box>
          <Typography variant="label" color="text.secondary">
            Commit
          </Typography>
          <Typography variant="data" component="p" color="text.primary">
            {run.commit ? run.commit.slice(0, 8) : "—"}
          </Typography>
        </Box>
      </Box>
    </Panel>
  );
}

export function JobDetailPage() {
  const { jobName: jobID } = useParams<{ jobName: string }>();
  const [searchParams, setSearchParams] = useSearchParams();
  const [gridOpen, setGridOpen] = useState(false);
  const { data, loading, error } = useJobDetail(jobID);

  const runs = useMemo(() => data?.runs ?? [], [data]);
  const displayName = data?.name ?? jobID ?? "";

  const selectedBuildId =
    searchParams.get("run") ?? runs[0]?.build_id ?? undefined;

  const selectedRun: BuildResult | undefined = useMemo(() => {
    if (!selectedBuildId) return undefined;
    return runs.find((r) => r.build_id === selectedBuildId);
  }, [runs, selectedBuildId]);

  const testCases: TestCase[] = selectedRun?.test_cases ?? [];

  // Pass rate over the most recent 10 runs.
  const passRateRecent = useMemo(() => {
    if (runs.length === 0) return null;
    const recent = runs.slice(0, 10);
    return recent.filter((r) => r.passed).length / recent.length;
  }, [runs]);

  // Average duration across completed runs.
  const avgDuration = useMemo(() => {
    const done = runs.filter(
      (r) => r.result !== "PENDING" && r.duration_seconds != null,
    );
    if (done.length === 0) return null;
    return done.reduce((sum, r) => sum + r.duration_seconds, 0) / done.length;
  }, [runs]);

  function handleSelectRun(buildId: string) {
    setSearchParams({ run: buildId });
  }

  if (loading) {
    return <LoadingState />;
  }

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

  const rateColor = passRateRecent !== null ? passRateColor(passRateRecent) : "primary";
  const statTiles: StatTile[] = [
    {
      label: "Pass rate",
      value: passRateRecent !== null ? formatPercent(passRateRecent) : "—",
      color: `${rateColor}.main`,
    },
    { label: "Total runs", value: String(runs.length) },
    {
      label: "Avg duration",
      value: avgDuration !== null ? formatDuration(avgDuration) : "—",
    },
  ];

  const statsRow = (
    <Box
      sx={{
        display: "grid",
        gridTemplateColumns: { xs: "repeat(2, 1fr)", sm: "repeat(3, 1fr)" },
        gap: 1.5,
        maxWidth: 560,
      }}
    >
      {statTiles.map((tile) => (
        <MetricTile key={tile.label} tile={tile} />
      ))}
    </Box>
  );

  const statsColumn = (
    <Box sx={{ display: "grid", gridTemplateColumns: "1fr", gap: 1.5 }}>
      {statTiles.map((tile) => (
        <MetricTile key={tile.label} tile={tile} />
      ))}
    </Box>
  );

  const runHistorySection = (
    <Box component="section">
      <SectionHeading title="Run History" />
      <RunTimeline
        runs={runs}
        selectedBuildId={selectedBuildId}
        onSelect={handleSelectRun}
      />
    </Box>
  );

  const gridSection = (
    <Box component="section">
      <Button
        type="button"
        variant="text"
        onClick={() => setGridOpen(!gridOpen)}
        sx={{
          minWidth: 0,
          p: 0,
          color: "text.primary",
          textTransform: "none",
          gap: 1.25,
          "&:hover": { color: "primary.main", bgcolor: "transparent" },
        }}
      >
        <Box sx={{ width: 4, height: 18, borderRadius: 999, bgcolor: "primary.main", flexShrink: 0 }} />
        <Typography variant="headline" component="span">
          Test Results Grid
        </Typography>
        <ChevronRight
          sx={{
            fontSize: 22,
            color: "text.secondary",
            transition: (t) => t.transitions.create("transform", { duration: t.transitions.duration.short }),
            transform: gridOpen ? "rotate(90deg)" : "rotate(0deg)",
          }}
        />
      </Button>
      <Collapse in={gridOpen} timeout="auto" unmountOnExit>
        <Box sx={{ pt: 1.5 }}>
          <TestResultsGrid runs={runs} jobID={jobID!} />
        </Box>
      </Collapse>
    </Box>
  );

  return (
    <Box sx={{ display: "flex", flexDirection: "column", gap: { xs: 3, sm: 4 } }}>
      <Breadcrumbs separator="›" sx={{ color: "text.secondary", fontSize: "0.875rem" }}>
        <Link
          component={RouterLink}
          to="/"
          underline="none"
          sx={{ color: "text.secondary", "&:hover": { color: "primary.main" } }}
        >
          Dashboard
        </Link>
        <Typography variant="inherit" color="text.primary">
          {displayName}
        </Typography>
      </Breadcrumbs>

      <Box>
        <Box sx={{ display: "flex", alignItems: "center", gap: 1.5, flexWrap: "wrap" }}>
          <Typography
            variant="h5"
            component="h1"
            sx={{ fontWeight: 700, color: "text.primary", fontSize: { xs: "1.25rem", sm: "1.5rem" } }}
          >
            {displayName}
          </Typography>
          {(() => {
            const jobStatus = deriveJobStatus(passRateRecent);
            return jobStatus ? <StatusChip status={jobStatus} /> : null;
          })()}
        </Box>
        {lastRun && (
          <Typography
            variant="data"
            color="text.secondary"
            sx={{ display: "block", mt: 0.75, fontSize: "0.75rem" }}
          >
            Last run {timeAgo(lastRun.started)}
          </Typography>
        )}
      </Box>

      {!hasRuns ? (
        <>
          {pattern && (
            <Box sx={{ maxWidth: 860 }}>
              <PatternBanner pattern={pattern} jobID={jobID} />
            </Box>
          )}
          {statsRow}
          <Panel sx={{ borderRadius: 3, p: 6, textAlign: "center" }}>
            <Typography variant="headline" sx={{ fontSize: "1rem", color: "text.primary" }}>
              No runs found
            </Typography>
            <Typography variant="body2" color="text.secondary" sx={{ mt: 0.5 }}>
              This job has no recorded builds in the current window.
            </Typography>
          </Panel>
        </>
      ) : (
        <>
          {pattern ? (
            <Box
              sx={{
                display: "grid",
                gridTemplateColumns: { xs: "1fr", lg: "minmax(0, 1.5fr) minmax(300px, 1fr)" },
                gap: 2,
                alignItems: "start",
              }}
            >
              <PatternBanner pattern={pattern} jobID={jobID} />
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
                {statsColumn}
                {runHistorySection}
                {selectedRun && <RunDetailsPanel run={selectedRun} orientation="column" />}
              </Box>
            </Box>
          ) : (
            <>
              {statsRow}
              {runHistorySection}
            </>
          )}

          {gridSection}

          {selectedRun && (
            <>
              {!pattern && <RunDetailsPanel run={selectedRun} orientation="row" />}
              {testCases.length > 0 ? (
                <Box component="section">
                  <SectionHeading title="Test Cases" />
                  <TestCaseTable
                    testCases={testCases}
                    jobID={jobID}
                    buildId={selectedRun.build_id}
                    buildLogUrl={selectedRun.build_log_url}
                    webUrl={selectedRun.web_url}
                  />
                </Box>
              ) : (
                <Panel component="section" sx={{ borderRadius: 3, p: 4, textAlign: "center" }}>
                  {selectedRun.result === "PENDING" ? (
                    <Box sx={{ display: "flex", alignItems: "center", justifyContent: "center", gap: 1, color: "text.secondary" }}>
                      <HourglassEmpty sx={{ fontSize: 20 }} />
                      <Typography color="text.secondary">
                        This build is still running — test results will appear when it completes.
                      </Typography>
                    </Box>
                  ) : (
                    <Typography color="text.secondary">
                      No test cases available for this run.
                    </Typography>
                  )}
                </Panel>
              )}
            </>
          )}
        </>
      )}
    </Box>
  );
}
