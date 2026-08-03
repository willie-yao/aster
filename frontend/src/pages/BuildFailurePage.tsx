import Breadcrumbs from "@mui/material/Breadcrumbs";
import Link from "@mui/material/Link";
import Stack from "@mui/material/Stack";
import Typography from "@mui/material/Typography";
import { Link as RouterLink, useParams } from "react-router-dom";
import { BuildFailurePanel } from "../components/BuildFailurePanel";
import { ErrorState } from "../components/ErrorState";
import { LoadingState } from "../components/LoadingState";
import { Panel } from "../components/Panel";
import { useJobDetail } from "../hooks/useData";
import { useSharedFetchStatus } from "../hooks/useSharedFetchStatus";
import { buildFailure as findBuildFailure } from "../lib/buildFailures";
import { jobRunPath } from "../lib/routes";

export function BuildFailurePage() {
  const { jobName: jobID, buildId } = useParams<{ jobName: string; buildId: string }>();
  const { data, loading, error } = useJobDetail(jobID);
  const fetchStatus = useSharedFetchStatus();
  if (loading) return <LoadingState />;
  if (error) return <ErrorState title="Failed to load build failure" message={error} onRetry={() => window.location.reload()} />;
  if (!data) return null;

  const run = data.runs.find((candidate) => candidate.build_id === buildId);
  const failure = findBuildFailure(run?.test_cases);
  if (!run || !failure) {
    return <Panel sx={{ p: 4, textAlign: "center" }}><Typography color="text.secondary">Build failure not found in the current window.</Typography></Panel>;
  }

  return (
    <Stack spacing={{ xs: 3, sm: 4 }}>
      <Breadcrumbs separator="›" sx={{ fontSize: "0.875rem" }}>
        <Link component={RouterLink} to="/" color="text.secondary" underline="hover">Dashboard</Link>
        <Link component={RouterLink} to={jobRunPath(jobID ?? "", run.build_id)} color="text.secondary" underline="hover">{data.name}</Link>
        <Typography color="text.primary">Build {run.build_id}</Typography>
      </Breadcrumbs>
      <Stack spacing={0.75}>
        <Typography component="h1" variant="h5" sx={{ fontWeight: 700 }}>Build failure analysis</Typography>
        <Typography variant="body2" color="text.secondary">This failure occurred before a failed JUnit test case was reported.</Typography>
      </Stack>
      <BuildFailurePanel jobID={jobID ?? ""} run={run} failure={failure} fetchStatus={fetchStatus} showDetailLink={false} />
    </Stack>
  );
}
