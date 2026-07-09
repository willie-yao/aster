import { useMemo, useState } from "react";
import Box from "@mui/material/Box";
import Breadcrumbs from "@mui/material/Breadcrumbs";
import Button from "@mui/material/Button";
import Chip from "@mui/material/Chip";
import Collapse from "@mui/material/Collapse";
import Link from "@mui/material/Link";
import Typography from "@mui/material/Typography";
import { ChevronRight, OpenInNew } from "@mui/icons-material";
import { Link as RouterLink, useParams, useSearchParams } from "react-router-dom";
import { useJobDetail } from "../hooks/useData";
import { formatDuration, formatPercent, timeAgo } from "../lib/utils";
import type { BuildResult, TestCase } from "../types/dashboard";
import { RunTimeline } from "../components/RunTimeline";
import { TestResultsGrid } from "../components/TestResultsGrid";
import { TestCaseTable } from "../components/TestCaseTable";
import { PatternBanner } from "../components/PatternBanner";
import { Panel } from "../components/Panel";
import { LoadingState } from "../components/LoadingState";
import { ErrorState } from "../components/ErrorState";
import { dotColorFor, soft } from "../theme";

function passRateColor(rate: number): "success" | "warning" | "error" {
  if (rate >= 0.9) return "success";
  if (rate <= 0.3) return "error";
  return "warning";
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
        <Typography
          variant="h5"
          component="h1"
          sx={{ fontWeight: 700, color: "text.primary", fontSize: { xs: "1.25rem", sm: "1.5rem" } }}
        >
          {displayName}
        </Typography>
        {lastRun && (
          <Typography
            variant="data"
            color="text.secondary"
            sx={{ display: "block", mt: 0.75, fontSize: "0.75rem" }}
          >
            Last run {timeAgo(lastRun.started)}
          </Typography>
        )}
        <Box
          sx={{
            mt: 2,
            display: "grid",
            gridTemplateColumns: { xs: "repeat(2, 1fr)", sm: "repeat(3, 1fr)" },
            gap: 1.5,
            maxWidth: 560,
          }}
        >
          {(() => {
            const rateColor = passRateRecent !== null ? passRateColor(passRateRecent) : "primary";
            const tiles: { label: string; value: string; color?: string }[] = [
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
            return tiles.map((tile) => (
              <Panel key={tile.label} sx={{ borderRadius: "12px", px: 1.75, py: 1.25 }}>
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
            ));
          })()}
        </Box>
      </Box>

      {pattern && <PatternBanner pattern={pattern} jobID={jobID} />}

      {runs.length === 0 ? (
        <Panel sx={{ borderRadius: 3, p: 6, textAlign: "center" }}>
          <Typography variant="headline" sx={{ fontSize: "1rem", color: "text.primary" }}>
            No runs found
          </Typography>
          <Typography variant="body2" color="text.secondary" sx={{ mt: 0.5 }}>
            This job has no recorded builds in the current window.
          </Typography>
        </Panel>
      ) : (
        <>
          <Box component="section">
            <Typography variant="headline" component="h2" sx={{ mb: 1.5 }}>
              Run History
            </Typography>
            <RunTimeline
              runs={runs}
              selectedBuildId={selectedBuildId}
              onSelect={handleSelectRun}
            />
          </Box>

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
                gap: 1,
                "&:hover": { color: "primary.main", bgcolor: "transparent" },
              }}
            >
              <ChevronRight
                sx={{
                  fontSize: 22,
                  transition: (t) => t.transitions.create("transform", { duration: t.transitions.duration.short }),
                  transform: gridOpen ? "rotate(90deg)" : "rotate(0deg)",
                }}
              />
              <Typography variant="headline" component="span">
                Test Results Grid
              </Typography>
            </Button>
            <Collapse in={gridOpen} timeout="auto" unmountOnExit>
              <Box sx={{ pt: 1.5 }}>
                <TestResultsGrid runs={runs} jobID={jobID!} />
              </Box>
            </Collapse>
          </Box>

          {selectedRun && (
            <Panel component="section" sx={{ borderRadius: 3, p: { xs: 2, sm: 3 } }}>
              <Box sx={{ mb: 2, display: "flex", alignItems: "center", gap: 1.5 }}>
                <Typography variant="headline" component="h3" sx={{ fontSize: "1rem" }}>
                  Run Details
                </Typography>
                {selectedRun.result === "PENDING" ? (
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
                  <Box
                    sx={{
                      width: 10,
                      height: 10,
                      borderRadius: "50%",
                      bgcolor: (t) => dotColorFor(t, selectedRun.passed, selectedRun.result),
                    }}
                  />
                )}
              </Box>

              <Box
                sx={{
                  display: "grid",
                  gridTemplateColumns: { xs: "1fr", sm: "repeat(2, minmax(0, 1fr))", lg: "repeat(3, minmax(0, 1fr))" },
                  columnGap: 4,
                  rowGap: 1.5,
                }}
              >
                <Box>
                  <Typography variant="label" color="text.secondary">
                    Build ID
                  </Typography>
                  <Typography variant="data" component="p" color="text.primary">
                    {selectedRun.build_id}
                  </Typography>
                </Box>
                <Box>
                  <Typography variant="label" color="text.secondary">
                    Started
                  </Typography>
                  <Typography variant="body2" color="text.primary">
                    {new Date(selectedRun.started).toLocaleString()}
                  </Typography>
                </Box>
                <Box>
                  <Typography variant="label" color="text.secondary">
                    Finished
                  </Typography>
                  <Typography variant="body2" color="text.primary">
                    {selectedRun.result === "PENDING"
                      ? "Still running…"
                      : new Date(selectedRun.finished).toLocaleString()}
                  </Typography>
                </Box>
                <Box>
                  <Typography variant="label" color="text.secondary">
                    Duration
                  </Typography>
                  <Typography variant="data" component="p" color="text.primary">
                    {selectedRun.result === "PENDING"
                      ? "—"
                      : formatDuration(selectedRun.duration_seconds)}
                  </Typography>
                </Box>
                <Box>
                  <Typography variant="label" color="text.secondary">
                    Commit
                  </Typography>
                  <Typography variant="data" component="p" color="text.primary">
                    {selectedRun.commit
                      ? selectedRun.commit.slice(0, 8)
                      : "—"}
                  </Typography>
                </Box>
                <Box sx={{ display: "flex", alignItems: "flex-end", gap: 1.5, flexWrap: "wrap" }}>
                  {selectedRun.prow_url && (
                    <Link
                      href={selectedRun.prow_url}
                      target="_blank"
                      rel="noopener noreferrer"
                      sx={{ display: "inline-flex", alignItems: "center", gap: 0.5, color: "primary.main" }}
                    >
                      View in Prow <OpenInNew sx={{ fontSize: 16 }} />
                    </Link>
                  )}
                  {selectedRun.build_log_url && (
                    <Link
                      href={selectedRun.build_log_url}
                      target="_blank"
                      rel="noopener noreferrer"
                      sx={{ display: "inline-flex", alignItems: "center", gap: 0.5, color: "primary.main" }}
                    >
                      Build Log <OpenInNew sx={{ fontSize: 16 }} />
                    </Link>
                  )}
                </Box>
              </Box>
            </Panel>
          )}

          {selectedRun && testCases.length > 0 && (
            <Box component="section">
              <Typography variant="headline" component="h2" sx={{ mb: 1.5 }}>
                Test Cases
              </Typography>
              <TestCaseTable
                testCases={testCases}
                jobID={jobID}
                buildId={selectedRun?.build_id}
                buildLogUrl={selectedRun?.build_log_url}
                webUrl={selectedRun?.web_url}
              />
            </Box>
          )}

          {selectedRun && testCases.length === 0 && (
            <Panel component="section" sx={{ borderRadius: 3, p: 4, textAlign: "center" }}>
              <Typography color="text.secondary">
                {selectedRun.result === "PENDING"
                  ? "⏳ This build is still running — test results will appear when it completes."
                  : "No test cases available for this run."}
              </Typography>
            </Panel>
          )}
        </>
      )}
    </Box>
  );
}
