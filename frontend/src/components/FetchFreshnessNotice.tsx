import Alert from "@mui/material/Alert";
import AlertTitle from "@mui/material/AlertTitle";
import Typography from "@mui/material/Typography";
import type { JobFetchStatus, JobFetchSummary } from "../types/dashboard";
import { fetchFreshnessDetail, jobFetchFreshnessDetail } from "../lib/fetchFreshness";

export function FetchFreshnessNotice({ fetch }: { fetch?: JobFetchSummary }) {
  const detail = fetchFreshnessDetail(fetch);
  if (!detail) return null;
  return (
    <Alert severity="warning">
      <AlertTitle>Published with incomplete job data</AlertTitle>
      {detail}
    </Alert>
  );
}

export function JobFreshnessNotice({ fetch, compact = false }: { fetch?: JobFetchStatus; compact?: boolean }) {
  const detail = jobFetchFreshnessDetail(fetch);
  if (!detail) return null;
  return (
    <Typography variant="caption" component="div" title={detail} sx={{ mt: 0.5, color: "warning.main" }}>
      Carried forward{compact ? "" : `. ${detail}`}
    </Typography>
  );
}
