import Box from "@mui/material/Box";
import Breadcrumbs from "@mui/material/Breadcrumbs";
import Link from "@mui/material/Link";
import Stack from "@mui/material/Stack";
import Typography from "@mui/material/Typography";
import { Link as RouterLink, useParams } from "react-router-dom";
import { BuildFailurePanel } from "../components/BuildFailurePanel";
import { ErrorState } from "../components/ErrorState";
import { LoadingState } from "../components/LoadingState";
import { RunMetadata } from "../components/RunMetadata";
import { useJobDetail } from "../hooks/useData";
import { useManifest } from "../hooks/useManifest";
import { useSharedFetchStatus } from "../hooks/useSharedFetchStatus";
import { buildFailure as findBuildFailure } from "../lib/buildFailures";
import { jobPath, jobRunPath } from "../lib/routes";
import { formatDuration, shortJobName } from "../lib/utils";
import type { BuildResult } from "../types/dashboard";
import { overviewTypography } from "../theme/overview";

function runStatus(run: BuildResult): {
  label: string;
  color: "warning.main" | "success.main" | "error.main";
} {
  if (run.result === "PENDING") return { label: "Running", color: "warning.main" };
  if (run.passed) return { label: "Passed", color: "success.main" };
  return { label: "Failed", color: "error.main" };
}

function formatTimestamp(value: string): string {
  const date = new Date(value);
  return value && !Number.isNaN(date.getTime()) ? date.toLocaleString() : "Not available";
}

export function BuildFailurePage() {
  const { jobName: jobID, buildId } = useParams<{ jobName: string; buildId: string }>();
  const { data, loading, error } = useJobDetail(jobID);
  const manifest = useManifest();
  const fetchStatus = useSharedFetchStatus();
  if (loading) return <LoadingState />;
  if (error) return <ErrorState title="Failed to load build failure" message={error} onRetry={() => window.location.reload()} />;
  if (!data) return null;

  const canonicalJobID = data.job_id ?? jobID ?? "";
  const shortenedName = shortJobName(
    data.name ?? canonicalJobID,
    manifest.short_name_prefix ?? "",
  );
  const displayName = shortenedName || canonicalJobID;
  const run = data.runs.find((candidate) => candidate.build_id === buildId);
  const failure = findBuildFailure(run?.test_cases);
  const jobDestination = run
    ? jobRunPath(canonicalJobID, run.build_id)
    : jobPath(canonicalJobID);

  const breadcrumbs = (
    <>
      <Box component="nav" aria-label="Breadcrumb" sx={{ display: { xs: "block", sm: "none" } }}>
        <Link
          component={RouterLink}
          to={jobDestination}
          underline="none"
          sx={{ minHeight: 44, display: "inline-flex", alignItems: "center", fontSize: "13px", fontWeight: 650 }}
        >
          ← {displayName}
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
        <Link component={RouterLink} to={jobDestination} underline="none" color="textSecondary">
          {displayName}
        </Link>
        <Typography color="textPrimary" noWrap>
          Build {buildId ?? "failure"}
        </Typography>
      </Breadcrumbs>
    </>
  );

  if (!run || !failure) {
    return (
      <Stack spacing={{ xs: 2.5, sm: 3.5 }} sx={{ minWidth: 0, maxWidth: "100%", overflowX: "clip" }}>
        {breadcrumbs}
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
            Build failure not found
          </Typography>
          <Typography color="textSecondary" sx={{ mt: 0.5, ...overviewTypography.secondaryBody }}>
            The requested build failure is not present in the current window.
          </Typography>
        </Box>
      </Stack>
    );
  }

  const status = runStatus(run);
  const runMetadata = (
    <RunMetadata
      stacked
      status={status.label}
      statusColor={status.color}
      items={[
        { label: "Result", value: run.result || status.label },
        { label: "Build ID", value: run.build_id },
        { label: "Started", value: formatTimestamp(run.started) },
        { label: "Finished", value: formatTimestamp(run.finished) },
        {
          label: "Duration",
          value: run.result === "PENDING" ? "Running" : formatDuration(run.duration_seconds),
        },
        { label: "Commit", value: run.commit || "Not available" },
      ]}
      links={[
        ...(run.prow_url ? [{ label: "View in Prow", href: run.prow_url }] : []),
        ...(run.build_log_url ? [{ label: "Build log", href: run.build_log_url }] : []),
      ]}
    />
  );

  return (
    <Stack spacing={{ xs: 2.5, sm: 3.5 }} sx={{ minWidth: 0, maxWidth: "100%", overflowX: "clip" }}>
      {breadcrumbs}
      <Box sx={{ minWidth: 0 }}>
        <Typography
          component="h1"
          sx={{
            ...overviewTypography.pageHeadline,
            fontSize: { xs: "26px", sm: "30px" },
            lineHeight: { xs: "33px", sm: "38px" },
            fontWeight: 720,
            color: "text.primary",
          }}
        >
          Build failure
        </Typography>
        <Typography
          component="p"
          color="textSecondary"
          sx={{ m: 0, mt: 0.75, ...overviewTypography.secondaryBody }}
        >
          Failed before JUnit results
        </Typography>
      </Box>
      <BuildFailurePanel
        jobID={canonicalJobID}
        run={run}
        failure={failure}
        fetchStatus={fetchStatus}
        showDetailLink={false}
        briefingTitle="Analysis briefing"
        mobileBriefingTitle="Analysis briefing"
        beforeActions={runMetadata}
      />
    </Stack>
  );
}
