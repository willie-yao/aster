import OpenInNew from "@mui/icons-material/OpenInNew";
import Box from "@mui/material/Box";
import Breadcrumbs from "@mui/material/Breadcrumbs";
import Link from "@mui/material/Link";
import Stack from "@mui/material/Stack";
import Typography from "@mui/material/Typography";
import { Link as RouterLink, useParams } from "react-router-dom";
import { DetailSectionBand } from "../components/DetailSectionBand";
import { ErrorState } from "../components/ErrorState";
import { EscalationPanel } from "../components/EscalationPanel";
import { LoadingState } from "../components/LoadingState";
import { RunMetadata } from "../components/RunMetadata";
import { useCapabilities } from "../hooks/useCapabilities";
import { useSharedFailures } from "../hooks/useData";
import { useManifest } from "../hooks/useManifest";
import { attributionLabel } from "../lib/pullRequests";
import { jobPath, pullRequestPath, pullRequestsPath } from "../lib/routes";
import {
  evidenceMember,
  findSharedFailure,
  sharedFailureBlockedReason,
  sharedFailureScope,
  sharedFailureSubject,
} from "../lib/sharedFailures";
import {
  getSharedFailureEscalation,
  startSharedFailureEscalation,
} from "../lib/sharedFailureEscalation";
import { overviewTypography } from "../theme/overview";
import type { SharedFailure, SharedFailureMember } from "../types/pullRequests";

function formatTimestamp(value: string | undefined): string {
  if (!value) return "Not available";
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? "Not available" : date.toLocaleString();
}

function MemberRow({
  member,
  wouldSupplyEvidence,
}: {
  member: SharedFailureMember;
  wouldSupplyEvidence: boolean;
}) {
  return (
    <Box
      component="article"
      sx={{
        minWidth: 0,
        display: "grid",
        gridTemplateColumns: { xs: "minmax(0, 1fr)", sm: "minmax(0, 1fr) auto" },
        alignItems: "center",
        columnGap: 1.5,
        rowGap: 0.5,
        px: { xs: 1.5, sm: 2 },
        py: 1.25,
        borderTop: "1px solid",
        borderColor: "divider",
      }}
    >
      <Box sx={{ minWidth: 0 }}>
        <Box sx={{ display: "flex", alignItems: "center", gap: 0.75, flexWrap: "wrap", minWidth: 0 }}>
          <Link
            component={RouterLink}
            to={pullRequestPath(member.number)}
            underline="none"
            sx={{ ...overviewTypography.jobIdentifier, color: "text.primary" }}
          >
            #{member.number}
          </Link>
          <Typography
            title={member.title}
            color="textPrimary"
            sx={{ minWidth: 0, overflowWrap: "anywhere", ...overviewTypography.secondaryBody }}
          >
            {member.title || "Untitled pull request"}
          </Typography>
        </Box>
        <Typography color="textSecondary" sx={{ mt: 0.25, ...overviewTypography.description }}>
          {member.author ? `by ${member.author} · ` : ""}
          build {member.build_id}
          {member.verdict ? ` · ${attributionLabel(member.verdict)}` : ""}
          {member.stale ? " · tested an older head" : ""}
          {wouldSupplyEvidence ? " · newest usable build" : ""}
        </Typography>
      </Box>
      <Box sx={{ display: "flex", alignItems: "center", gap: 1.5, flexWrap: "wrap", justifySelf: { xs: "start", sm: "end" } }}>
        {member.web_url && (
          <Link
            href={member.web_url}
            target="_blank"
            rel="noopener noreferrer"
            underline="none"
            sx={{ display: "inline-flex", alignItems: "center", gap: 0.5, whiteSpace: "nowrap", ...overviewTypography.description }}
          >
            Artifacts
            <OpenInNew sx={{ fontSize: 13 }} />
          </Link>
        )}
        {member.html_url && (
          <Link
            href={member.html_url}
            target="_blank"
            rel="noopener noreferrer"
            underline="none"
            sx={{ display: "inline-flex", alignItems: "center", gap: 0.5, whiteSpace: "nowrap", ...overviewTypography.description }}
          >
            GitHub
            <OpenInNew sx={{ fontSize: 13 }} />
          </Link>
        )}
      </Box>
    </Box>
  );
}

function SharedFailureEscalation({ failure, enabled }: { failure: SharedFailure; enabled: boolean }) {
  // Without the capability there is no analysis to offer, so explaining why one
  // is unavailable would describe a control this deploy never had.
  if (!enabled) return null;
  const blocked = sharedFailureBlockedReason(failure);
  if (blocked) {
    return (
      <Typography color="textSecondary" sx={{ mt: 1, ...overviewTypography.description }}>
        {blocked}
      </Typography>
    );
  }
  return (
    <EscalationPanel
      enabled={enabled}
      subjectKey={failure.id}
      load={() => getSharedFailureEscalation(failure.id)}
      start={(key) => startSharedFailureEscalation(failure.id, key)}
      disclaimer="This analysis explains the failure from one build's artifacts. It does not establish that any pull request caused it."
    />
  );
}

export function SharedFailurePage() {
  const { id } = useParams<{ id: string }>();
  const manifest = useManifest();
  const enabled = manifest.pull_requests?.enabled ?? false;
  const linkToJob = manifest.source?.include_presubmits ?? false;
  const { data, loading, error } = useSharedFailures(enabled);
  const { features } = useCapabilities();
  const escalationEnabled = features.shared_failure_escalation ?? false;

  const breadcrumbs = (
    <>
      <Box component="nav" aria-label="Breadcrumb" sx={{ display: { xs: "block", sm: "none" } }}>
        <Link
          component={RouterLink}
          to={pullRequestsPath()}
          underline="none"
          sx={{ minHeight: 44, display: "inline-flex", alignItems: "center", fontSize: "13px", fontWeight: 650 }}
        >
          ← Pull requests
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
        <Link component={RouterLink} to={pullRequestsPath()} underline="none" color="textSecondary">
          Pull requests
        </Link>
        <Typography color="textPrimary" noWrap>
          Shared failure
        </Typography>
      </Breadcrumbs>
    </>
  );

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
        title="Failed to load shared failures"
        message={error}
        onRetry={() => window.location.reload()}
      />
    );
  }

  const failure = findSharedFailure(data?.failures, id);
  if (!failure) {
    return (
      <ErrorState
        title="Shared failure not found"
        message="This failure is no longer reported by several open pull requests, so it is not in the current pass."
      />
    );
  }

  const evidence = evidenceMember(failure);

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
            overflowWrap: "anywhere",
          }}
        >
          {sharedFailureSubject(failure)}
        </Typography>
        <Typography component="p" color="textSecondary" sx={{ m: 0, mt: 0.75, ...overviewTypography.secondaryBody }}>
          Failing on {sharedFailureScope(failure)}
          {" · "}
          {linkToJob ? (
            <Link component={RouterLink} to={jobPath(failure.job_id)} underline="none">
              {failure.job_name}
            </Link>
          ) : (
            failure.job_name
          )}
        </Typography>
      </Box>

      <RunMetadata
        status={`${failure.pull_requests.length} pull requests`}
        statusColor="error.main"
        items={[
          { label: "Job", value: failure.job_name },
          { label: "Base branch", value: failure.base_ref || "Not available" },
          { label: "Oldest build seen", value: formatTimestamp(failure.oldest_build_started) },
          { label: "Newest build seen", value: formatTimestamp(failure.newest_build_started) },
        ]}
        links={[]}
      />

      <Box component="section" sx={{ minWidth: 0 }}>
        <DetailSectionBand
          title="Why this is shared"
          metadata={
            escalationEnabled && failure.escalatable
              ? "One analysis for every affected pull request"
              : undefined
          }
        />
        <Box sx={{ px: { xs: 1.5, sm: 2 }, py: 1.5, borderTop: "1px solid", borderColor: "divider" }}>
          <Typography color="textSecondary" sx={overviewTypography.secondaryBody}>
            The same failure on several open pull requests usually has a cause
            they share rather than a cause in any one change. These pull
            requests were correlated only by base branch, job, and test, so they
            are not established to be independent: one may be stacked on
            another. The build window above covers only the builds this pass
            observed, so it is not when the failure started.
          </Typography>
          <SharedFailureEscalation failure={failure} enabled={escalationEnabled} />
        </Box>
      </Box>

      <Box component="section" sx={{ minWidth: 0 }}>
        <DetailSectionBand
          title="Affected pull requests"
          metadata={
            evidence
              ? `${failure.pull_requests.length} observed · #${evidence.number} has the newest usable build`
              : `${failure.pull_requests.length} observed`
          }
        />
        <Box sx={{ bgcolor: "surface.container", border: "1px solid", borderColor: "divider", borderRadius: "4px", mt: 1.5 }}>
          {failure.pull_requests.map((member) => (
            <MemberRow
              key={member.number}
              member={member}
              wouldSupplyEvidence={evidence?.number === member.number}
            />
          ))}
        </Box>
      </Box>
    </Stack>
  );
}
