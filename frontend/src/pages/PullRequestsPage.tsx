import Box from "@mui/material/Box";
import Typography from "@mui/material/Typography";
import { useMemo } from "react";
import { useSearchParams } from "react-router-dom";
import { ErrorState } from "../components/ErrorState";
import { LoadingState } from "../components/LoadingState";
import { PullRequestFilters } from "../components/PullRequestFilters";
import { PullRequestLedger } from "../components/PullRequestLedger";
import { SharedFailureLedger } from "../components/SharedFailureLedger";
import { usePullRequestIndex, useSharedFailures } from "../hooks/useData";
import { useManifest } from "../hooks/useManifest";
import {
  filterPullRequests,
  orderPullRequests,
  pullRequestStateCounts,
  pullRequestStateFromParam,
  withPullRequestState,
  type PullRequestStateFilter,
} from "../lib/pullRequests";
import { orderSharedFailures } from "../lib/sharedFailures";
import { timeAgo } from "../lib/utils";
import { overviewLayout, overviewTypography } from "../theme/overview";

function EmptyState({ repo }: { repo: string }) {
  return (
    <Box
      sx={{
        maxWidth: 720,
        mx: "auto",
        mt: { xs: 4, sm: 8 },
        p: { xs: 3, sm: 5 },
        border: "1px solid",
        borderColor: "divider",
        borderRadius: "6px",
        bgcolor: "surface.container",
        textAlign: "center",
      }}
    >
      <Typography variant="h5" component="h1" sx={{ fontWeight: 700 }}>
        No open pull requests
      </Typography>
      <Typography color="textSecondary" sx={{ mt: 1.5 }}>
        {repo || "The configured repository"} has no open non-draft pull requests,
        or the last refresh could not reach GitHub. Draft pull requests are always
        excluded.
      </Typography>
    </Box>
  );
}

export function PullRequestsPage() {
  const manifest = useManifest();
  const enabled = manifest.pull_requests?.enabled ?? false;
  const { data, loading, error } = usePullRequestIndex(enabled);
  // Shared failures are supplementary, so a missing or failed load simply
  // omits the section rather than blocking the ledger.
  const { data: shared } = useSharedFailures(enabled);
  const [searchParams, setSearchParams] = useSearchParams();
  const stateFilter = pullRequestStateFromParam(searchParams.get("state"));

  const ordered = useMemo(
    () => (data ? orderPullRequests(data.pull_requests) : []),
    [data],
  );
  const counts = useMemo(() => pullRequestStateCounts(ordered), [ordered]);
  const filtered = useMemo(
    () => filterPullRequests(ordered, stateFilter),
    [ordered, stateFilter],
  );
  const sharedFailures = useMemo(
    () => (shared ? orderSharedFailures(shared.failures) : []),
    [shared],
  );

  function updateState(next: PullRequestStateFilter) {
    setSearchParams(withPullRequestState(searchParams, next), { replace: true });
  }

  if (!enabled) {
    return (
      <ErrorState
        title="Pull request triage is not enabled"
        message="Set pull_requests.enabled in project.yaml to publish this view."
      />
    );
  }
  if (loading) return <LoadingState />;
  if (error) {
    return (
      <ErrorState
        title="Failed to load pull requests"
        message={error}
        onRetry={() => window.location.reload()}
      />
    );
  }
  if (!data) return null;
  if (data.pull_requests.length === 0) return <EmptyState repo={data.repo} />;

  return (
    <Box sx={{ display: "flex", flexDirection: "column", gap: { xs: 3, sm: overviewLayout.majorSectionGap } }}>
      <Box
        sx={{
          display: "grid",
          gridTemplateColumns: { xs: "1fr", sm: "minmax(0, 1fr) auto" },
          alignItems: "end",
          gap: 1,
        }}
      >
        <Box sx={{ minWidth: 0 }}>
          <Typography variant="h4" component="h1" sx={overviewTypography.pageHeadline}>
            Pull Request Triage
          </Typography>
          <Typography color="textSecondary" sx={{ mt: 0.75, ...overviewTypography.secondaryBody }}>
            Presubmit results for open pull requests in {data.repo}
          </Typography>
        </Box>
        <Typography variant="data" color="textSecondary" sx={{ justifySelf: "end", ...overviewTypography.data }}>
          Updated {timeAgo(data.generated_at)}
        </Typography>
      </Box>

      <SharedFailureLedger failures={sharedFailures} />

      <Box component="section" aria-labelledby="pull-request-ledger-heading">
        <Box
          sx={{
            minHeight: overviewLayout.majorBandMinHeight,
            display: "flex",
            alignItems: "center",
            px: 1.5,
            bgcolor: "surface.containerHigh",
            borderBlock: "1px solid",
            borderColor: "divider",
            boxShadow: "inset 3px 0 0 var(--mui-palette-primary-main)",
          }}
        >
          <Typography
            id="pull-request-ledger-heading"
            variant="headline"
            component="h2"
            sx={overviewTypography.majorHeading}
          >
            Open pull requests
          </Typography>
        </Box>

        <PullRequestFilters
          stateFilter={stateFilter}
          counts={counts}
          matching={filtered.length}
          onStateChange={updateState}
        />

        {filtered.length === 0 ? (
          <Box sx={{ py: 8, textAlign: "center" }}>
            <Typography color="textSecondary">No pull requests match filters</Typography>
          </Box>
        ) : (
          <PullRequestLedger pulls={filtered} />
        )}
      </Box>
    </Box>
  );
}
