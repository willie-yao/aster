import ExpandMore from "@mui/icons-material/ExpandMore";
import OpenInNew from "@mui/icons-material/OpenInNew";
import Box from "@mui/material/Box";
import Breadcrumbs from "@mui/material/Breadcrumbs";
import Link from "@mui/material/Link";
import Stack from "@mui/material/Stack";
import Typography from "@mui/material/Typography";
import { useId, useState } from "react";
import { Link as RouterLink, useParams } from "react-router-dom";
import { DetailSectionBand } from "../components/DetailSectionBand";
import { EscalationPanel } from "../components/EscalationPanel";
import { ErrorState } from "../components/ErrorState";
import { LoadingState } from "../components/LoadingState";
import { RunMetadata } from "../components/RunMetadata";
import { StatusChip } from "../components/StatusChip";
import { useCapabilities } from "../hooks/useCapabilities";
import { usePullRequestDetail, useSharedFailures } from "../hooks/useData";
import { useManifest } from "../hooks/useManifest";
import {
  attributionLabel,
  attributionTone,
  checkState,
  checkStatusLabel,
  checkSummaryLine,
  needsInvestigation,
  shortSHA,
  staleCheckCount,
  unexplainedCount,
} from "../lib/pullRequests";
import {
  getEscalation,
  startEscalation,
  type EscalationRef,
} from "../lib/pullRequestEscalation";
import { findSharedFailureFor } from "../lib/sharedFailures";
import { jobPath, pullRequestsPath, sharedFailurePath } from "../lib/routes";
import { formatDuration } from "../lib/utils";
import { soft, accentLabelSx } from "../theme";
import { overviewTypography } from "../theme/overview";
import type {
  FailureAttribution,
  PullRequestCheck,
  PullRequestDetail,
  PullRequestFailure,
  SharedFailure,
} from "../types/pullRequests";

function formatTimestamp(value: string | undefined): string {
  if (!value) return "Not available";
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? "Not available" : date.toLocaleString();
}

function checkDuration(check: PullRequestCheck): string {
  if (!check.finished) return "Running";
  const started = Date.parse(check.started);
  const finished = Date.parse(check.finished);
  if (Number.isNaN(started) || Number.isNaN(finished)) return "Not available";
  return formatDuration((finished - started) / 1000);
}

function StaleBadge() {
  return (
    <Box
      component="span"
      title="This build tested an older head than the pull request's current one"
      sx={(theme) => ({
        px: 0.75,
        py: 0.125,
        borderRadius: "4px",
        border: "1px solid",
        borderColor: soft(theme, "warning", 0.24),
        bgcolor: soft(theme, "warning", 0.1),
        ...accentLabelSx(theme, "warning"),
        fontSize: "0.6875rem",
        fontWeight: 600,
        whiteSpace: "nowrap",
      })}
    >
      Stale
    </Box>
  );
}

function OptionalBadge() {
  return (
    <Box
      component="span"
      title="This presubmit does not block merge"
      sx={{
        px: 0.75,
        py: 0.125,
        borderRadius: "4px",
        border: "1px solid",
        borderColor: "divider",
        bgcolor: "surface.containerHigh",
        color: "text.secondary",
        fontSize: "0.6875rem",
        fontWeight: 600,
        whiteSpace: "nowrap",
      }}
    >
      Optional
    </Box>
  );
}

// AttributionBanner states what the observed baseline says about a failure. It
// leads the failure body because "already failing on main" changes whether the
// rest is worth reading. When the same failure is on other pull requests, it
// links to the shared view rather than leaving the reader at a peer's page.
function AttributionBanner({
  attribution,
  cluster,
}: {
  attribution: FailureAttribution;
  cluster?: SharedFailure;
}) {
  const tone = attributionTone(attribution.verdict);
  const neutral = tone === "default";
  return (
    <Box
      sx={{
        mt: 1,
        p: 1.25,
        borderRadius: "4px",
        border: "1px solid",
        borderColor: neutral ? "divider" : (theme) => soft(theme, tone, 0.24),
        bgcolor: neutral ? "surface.containerHigh" : (theme) => soft(theme, tone, 0.08),
      }}
    >
      <Box sx={{ display: "flex", alignItems: "center", gap: 0.75, flexWrap: "wrap" }}>
        <Typography
          component="span"
          sx={(theme) => ({
            ...(tone === "default"
              ? { color: "text.secondary" }
              : accentLabelSx(theme, tone)),
            ...overviewTypography.tableHeading,
            fontWeight: 700,
          })}
        >
          {attributionLabel(attribution.verdict)}
        </Typography>
        <Typography component="span" color="textSecondary" sx={overviewTypography.description}>
          {attribution.confidence} confidence
        </Typography>
      </Box>
      <Typography color="textPrimary" sx={{ mt: 0.5, ...overviewTypography.secondaryBody }}>
        {attribution.summary}
      </Typography>
      {attribution.evidence?.map((item, index) => (
        <Typography
          key={`${item.kind}-${index}`}
          color="textSecondary"
          sx={{ mt: 0.5, ...overviewTypography.description }}
        >
          {item.detail}
        </Typography>
      ))}
      {cluster && (
        <Link
          component={RouterLink}
          to={sharedFailurePath(cluster.id)}
          underline="none"
          sx={{ display: "inline-block", mt: 0.75, ...overviewTypography.description }}
        >
          Investigate this across all {cluster.pull_requests.length} pull requests
        </Link>
      )}
    </Box>
  );
}

// PullRequestEscalation binds the shared panel to one pull request failure.
function PullRequestEscalation({
  refValue,
  enabled,
}: {
  refValue: EscalationRef;
  enabled: boolean;
}) {
  return (
    <EscalationPanel
      enabled={enabled}
      subjectKey={`${refValue.pullNumber}\u0000${refValue.jobID}\u0000${refValue.buildID}\u0000${refValue.testName}`}
      load={() => getEscalation(refValue)}
      start={(key) => startEscalation(refValue, key)}
      disclaimer="This analysis explains the failure from build artifacts. It does not establish that the pull request caused it."
    />
  );
}

// FailureItem reveals the full failure output on demand. It is only a
// disclosure when there is a body to reveal, so a click never does nothing.
function FailureItem({
  failure,
  cluster,
  escalation,
}: {
  failure: PullRequestFailure;
  cluster?: SharedFailure;
  escalation?: { pullNumber: number; jobID: string; buildID: string; enabled: boolean };
}) {
  const [open, setOpen] = useState(false);
  const body = failure.failure_body?.trim();
  const bodyID = `failure-body-${useId()}`;

  const heading = (
    <>
      <Typography
        title={failure.name}
        sx={{ minWidth: 0, overflowWrap: "anywhere", color: "text.primary", ...overviewTypography.jobIdentifier }}
      >
        {failure.name}
      </Typography>
      {body && (
        <Box
          sx={{
            display: "flex",
            alignItems: "center",
            gap: 0.25,
            flexShrink: 0,
            color: "text.secondary",
            ...overviewTypography.description,
          }}
        >
          {open ? "Hide output" : "Show output"}
          <ExpandMore
            sx={{
              fontSize: 18,
              transition: "transform 140ms ease",
              transform: open ? "rotate(180deg)" : "none",
            }}
          />
        </Box>
      )}
    </>
  );

  return (
    <Box sx={{ minWidth: 0, borderTop: "1px solid", borderColor: "divider" }}>
      {body ? (
        <Box
          component="button"
          type="button"
          onClick={() => setOpen((value) => !value)}
          aria-expanded={open}
          aria-controls={bodyID}
          sx={{
            width: "100%",
            display: "flex",
            alignItems: "center",
            justifyContent: "space-between",
            gap: 1.5,
            px: { xs: 1.5, sm: 2 },
            py: 1.25,
            border: "none",
            bgcolor: "transparent",
            textAlign: "left",
            font: "inherit",
            color: "inherit",
            cursor: "pointer",
            transition: "background-color 140ms ease",
            "&:hover": { bgcolor: "surface.containerHigh" },
            "&:focus-visible": {
              outline: "2px solid",
              outlineColor: "primary.main",
              outlineOffset: -2,
            },
          }}
        >
          {heading}
        </Box>
      ) : (
        <Box sx={{ display: "flex", alignItems: "center", gap: 1.5, px: { xs: 1.5, sm: 2 }, py: 1.25 }}>
          {heading}
        </Box>
      )}

      <Box sx={{ px: { xs: 1.5, sm: 2 }, pb: 1.5 }}>
        {failure.attribution && (
          <AttributionBanner attribution={failure.attribution} cluster={cluster} />
        )}
        {escalation && needsInvestigation(failure) && (
          <PullRequestEscalation
            enabled={escalation.enabled}
            refValue={{
              pullNumber: escalation.pullNumber,
              jobID: escalation.jobID,
              buildID: escalation.buildID,
              testName: failure.name,
            }}
          />
        )}
        {failure.failure_message && (
          <Box
            component="pre"
            sx={{
              m: 0,
              mt: 1,
              p: 1.5,
              borderRadius: "4px",
              bgcolor: (theme) => soft(theme, "error", 0.08),
              color: "error.main",
              fontFamily: "monospace",
              fontSize: "0.75rem",
              lineHeight: 1.6,
              whiteSpace: "pre-wrap",
              overflowX: "auto",
              maxHeight: 220,
            }}
          >
            {failure.failure_message}
          </Box>
        )}
        {body && open && (
          <Box
            id={bodyID}
            component="pre"
            sx={{
              m: 0,
              mt: failure.failure_message ? 1 : 0,
              p: 1.5,
              borderRadius: "4px",
              bgcolor: "surface.containerHigh",
              color: "text.primary",
              fontFamily: "monospace",
              fontSize: "0.75rem",
              lineHeight: 1.6,
              whiteSpace: "pre-wrap",
              overflowX: "auto",
              maxHeight: 420,
              border: "1px solid",
              borderColor: "divider",
            }}
          >
            {body}
          </Box>
        )}
      </Box>
    </Box>
  );
}

// JobName links into the job dashboard only when presubmits are published as
// jobs. Without source.include_presubmits there is no job detail file for a
// presubmit, so a link would dead-end.
function JobName({ check, linkToJob }: { check: PullRequestCheck; linkToJob: boolean }) {
  if (!linkToJob) {
    return (
      <Typography
        component="span"
        title={check.job_name}
        sx={{ minWidth: 0, color: "text.primary", overflowWrap: "anywhere", ...overviewTypography.jobIdentifier }}
      >
        {check.job_name}
      </Typography>
    );
  }
  return (
    <Link
      component={RouterLink}
      to={jobPath(check.job_id)}
      underline="none"
      title={check.job_name}
      sx={{
        minWidth: 0,
        color: "text.primary",
        overflowWrap: "anywhere",
        ...overviewTypography.jobIdentifier,
        "&:hover": { color: "primary.main", textDecoration: "underline" },
        "&:focus-visible": {
          outline: "2px solid",
          outlineColor: "primary.main",
          outlineOffset: 1,
        },
      }}
    >
      {check.job_name}
    </Link>
  );
}

function ExternalLink({ href, label }: { href: string; label: string }) {
  return (
    <Link
      href={href}
      target="_blank"
      rel="noopener noreferrer"
      underline="none"
      sx={{ display: "inline-flex", alignItems: "center", gap: 0.5, whiteSpace: "nowrap", ...overviewTypography.description }}
    >
      {label}
      <OpenInNew sx={{ fontSize: 13 }} />
    </Link>
  );
}

function CheckCard({
  check,
  linkToJob,
  baseRef,
  clusters,
  escalation,
}: {
  check: PullRequestCheck;
  linkToJob: boolean;
  baseRef: string;
  clusters: SharedFailure[];
  escalation?: { pullNumber: number; enabled: boolean };
}) {
  const state = checkState(check);
  const failures = check.failures ?? [];
  return (
    <Box
      component="article"
      sx={{
        minWidth: 0,
        bgcolor: "surface.container",
        border: "1px solid",
        borderColor: "divider",
        borderRadius: "4px",
      }}
    >
      <Box
        sx={{
          display: "grid",
          gridTemplateColumns: { xs: "minmax(0, 1fr)", sm: "minmax(0, 1fr) auto" },
          alignItems: "center",
          columnGap: 1.5,
          rowGap: 0.75,
          px: { xs: 1.5, sm: 2 },
          py: 1.25,
          bgcolor: "surface.containerHigh",
          borderBottom: failures.length > 0 ? "1px solid" : "none",
          borderColor: "divider",
        }}
      >
        <Box sx={{ minWidth: 0 }}>
          <Box sx={{ display: "flex", alignItems: "center", gap: 0.75, flexWrap: "wrap", minWidth: 0 }}>
            <JobName check={check} linkToJob={linkToJob} />
            {check.stale && <StaleBadge />}
            {check.optional && <OptionalBadge />}
          </Box>
          <Typography color="textSecondary" sx={{ mt: 0.25, ...overviewTypography.description }}>
            {checkSummaryLine(check)} · build {check.build_id} · {checkDuration(check)}
            {check.tested_sha ? ` · ${shortSHA(check.tested_sha)}` : ""}
          </Typography>
        </Box>
        <Box sx={{ display: "flex", alignItems: "center", gap: 1.5, flexWrap: "wrap", justifySelf: { xs: "start", sm: "end" } }}>
          {check.web_url && <ExternalLink href={check.web_url} label="Artifacts" />}
          {check.build_log_url && <ExternalLink href={check.build_log_url} label="Build log" />}
          <StatusChip
            status={state === "RUNNING" ? "RUNNING" : state}
            label={checkStatusLabel(check)}
            sx={{ height: 26, fontSize: "13px" }}
          />
        </Box>
      </Box>

      {failures.map((failure, index) => (
        <FailureItem
          key={`${failure.name}-${index}`}
          failure={failure}
          cluster={findSharedFailureFor(clusters, baseRef, check.job_name, failure.name)}
          escalation={
            escalation && !check.stale
              ? {
                  pullNumber: escalation.pullNumber,
                  jobID: check.job_id,
                  buildID: check.build_id,
                  enabled: escalation.enabled,
                }
              : undefined
          }
        />
      ))}

      {check.failures_truncated && (
        <Typography
          color="textSecondary"
          sx={{ px: { xs: 1.5, sm: 2 }, py: 1, borderTop: "1px solid", borderColor: "divider", ...overviewTypography.description }}
        >
          Showing the first {failures.length} of {check.tests_failed} failing tests.
        </Typography>
      )}
    </Box>
  );
}

function ChecksSection({
  detail,
  linkToJob,
  clusters,
  escalationEnabled,
}: {
  detail: PullRequestDetail;
  linkToJob: boolean;
  clusters: SharedFailure[];
  escalationEnabled: boolean;
}) {
  const failing = detail.checks.filter((check) => checkState(check) === "FAILING");
  const stale = staleCheckCount(detail.checks);
  const unexplained = unexplainedCount(detail.checks);
  const metadata = [
    `${detail.checks.length} observed`,
    failing.length > 0 ? `${failing.length} failing` : null,
    unexplained > 0 ? `${unexplained} needing investigation` : null,
    stale > 0 ? `${stale} stale` : null,
  ]
    .filter(Boolean)
    .join(" · ");

  if (detail.checks.length === 0) {
    return (
      <Box component="section" sx={{ minWidth: 0, bgcolor: "surface.container" }}>
        <DetailSectionBand title="Presubmit checks" metadata="None observed" />
        <Box sx={{ px: { xs: 1.5, sm: 2 }, py: 3, borderTop: "1px solid", borderColor: "divider" }}>
          <Typography color="textSecondary" sx={overviewTypography.secondaryBody}>
            No presubmit builds were found for this pull request. Prow removes build
            artifacts after its retention window, so GitHub may still show statuses
            for runs whose artifacts are gone.
          </Typography>
        </Box>
      </Box>
    );
  }

  return (
    <Box component="section" sx={{ minWidth: 0 }}>
      <DetailSectionBand title="Presubmit checks" metadata={metadata} />
      <Stack spacing={1.5} sx={{ mt: 1.5 }}>
        {detail.checks.map((check) => (
          <CheckCard
            key={`${check.job_id}-${check.build_id}`}
            check={check}
            linkToJob={linkToJob}
            baseRef={detail.base_ref}
            clusters={clusters}
            escalation={{ pullNumber: detail.number, enabled: escalationEnabled }}
          />
        ))}
      </Stack>
    </Box>
  );
}

export function PullRequestDetailPage() {
  const { number } = useParams<{ number: string }>();
  const { data, loading, error } = usePullRequestDetail(number);
  const manifest = useManifest();
  const linkToJob = manifest.source?.include_presubmits ?? false;
  const pullRequestsEnabled = manifest.pull_requests?.enabled ?? false;
  // Shared failures are supporting context, so a missing or failed load simply
  // omits the cluster links rather than blocking the page.
  const { data: shared } = useSharedFailures(pullRequestsEnabled);
  const { features } = useCapabilities();
  const escalationEnabled = features.pull_request_escalation ?? false;

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
          #{number}
        </Typography>
      </Breadcrumbs>
    </>
  );

  if (loading) return <LoadingState />;
  if (error) {
    return (
      <ErrorState
        title="Failed to load pull request"
        message={error}
        onRetry={() => window.location.reload()}
      />
    );
  }
  if (!data) return null;

  const stateLabel =
    data.ci_state === "UNKNOWN"
      ? "No runs"
      : data.ci_state === "PENDING"
        ? "Pending"
        : data.ci_state === "PASSING"
          ? "Passing"
          : "Failing";

  return (
    <Stack spacing={{ xs: 2.5, sm: 3.5 }} sx={{ minWidth: 0, maxWidth: "100%", overflowX: "clip" }}>
      {breadcrumbs}

      <Box sx={{ minWidth: 0 }}>
        <Box sx={{ display: "flex", alignItems: "center", gap: 1, flexWrap: "wrap" }}>
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
            {data.title || `Pull request #${data.number}`}
          </Typography>
          <StatusChip
            status={data.ci_state === "PENDING" ? "RUNNING" : data.ci_state}
            label={stateLabel}
            sx={{ height: 26, fontSize: "13px" }}
          />
        </Box>
        <Typography component="p" color="textSecondary" sx={{ m: 0, mt: 0.75, ...overviewTypography.secondaryBody }}>
          {data.repo} #{data.number}
          {data.author ? ` opened by ${data.author}` : ""}
          {" · "}
          <Link
            href={data.html_url}
            target="_blank"
            rel="noopener noreferrer"
            underline="none"
            sx={{ display: "inline-flex", alignItems: "center", gap: 0.5 }}
          >
            View on GitHub
            <OpenInNew sx={{ fontSize: 13 }} />
          </Link>
        </Typography>
      </Box>

      <RunMetadata
        status={stateLabel}
        statusColor={
          data.ci_state === "FAILING"
            ? "error.main"
            : data.ci_state === "PASSING"
              ? "success.main"
              : data.ci_state === "PENDING"
                ? "warning.main"
                : "text.secondary"
        }
        items={[
          { label: "Base branch", value: data.base_ref || "Not available" },
          { label: "Head commit", value: shortSHA(data.head_sha) || "Not available" },
          { label: "Checks observed", value: String(data.checks_observed) },
          { label: "Checks failing", value: String(data.checks_failing) },
          { label: "Failing tests", value: String(data.failing_tests) },
          { label: "Updated", value: formatTimestamp(data.updated_at) },
        ]}
        links={[]}
      />

      <ChecksSection
        detail={data}
        linkToJob={linkToJob}
        clusters={shared?.failures ?? []}
        escalationEnabled={escalationEnabled}
      />
    </Stack>
  );
}
