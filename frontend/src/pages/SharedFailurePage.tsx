import OpenInNew from "@mui/icons-material/OpenInNew";
import Box from "@mui/material/Box";
import Breadcrumbs from "@mui/material/Breadcrumbs";
import Link from "@mui/material/Link";
import MenuItem from "@mui/material/MenuItem";
import Stack from "@mui/material/Stack";
import TextField from "@mui/material/TextField";
import Typography from "@mui/material/Typography";
import { useState } from "react";
import { Link as RouterLink, useParams } from "react-router-dom";
import { DetailSectionBand } from "../components/DetailSectionBand";
import { ErrorState } from "../components/ErrorState";
import { EscalationPanel } from "../components/EscalationPanel";
import { LoadingState } from "../components/LoadingState";
import { RunMetadata } from "../components/RunMetadata";
import { TechnicalIdentity } from "../components/TechnicalIdentity";
import { TestAnalysisBriefing } from "../components/TestAnalysisBriefing";
import { useCapabilities } from "../hooks/useCapabilities";
import { useDashboard, useJobDetail, useSharedFailures } from "../hooks/useData";
import { useManifest } from "../hooks/useManifest";
import { attributionLabel, isPublishedPresubmitJob } from "../lib/pullRequests";
import { parseTestDisplayName } from "../lib/detailTitles";
import { jobPath, pullRequestPath, pullRequestsPath, testRunPath } from "../lib/routes";
import {
  baseBranchCase,
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
import { fixPatternsForRun, testRunFileContext } from "../lib/testDetail";
import { shortJobName } from "../lib/utils";
import { overviewTypography } from "../theme/overview";
import type {
  SharedFailure,
  SharedFailureMember,
} from "../types/pullRequests";

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
        gridTemplateColumns: "minmax(0, 1fr)",
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
          {wouldSupplyEvidence ? " · representative build" : ""}
        </Typography>
      </Box>
      <Box sx={{ display: "flex", alignItems: "center", gap: 1.5, flexWrap: "wrap" }}>
        {member.web_url && (
          <Link
            href={member.web_url}
            target="_blank"
            rel="noopener noreferrer"
            underline="none"
            sx={{ display: "inline-flex", alignItems: "center", minHeight: { xs: 44, sm: 36 }, gap: 0.5, whiteSpace: "nowrap", ...overviewTypography.description }}
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
            sx={{ display: "inline-flex", alignItems: "center", minHeight: { xs: 44, sm: 36 }, gap: 0.5, whiteSpace: "nowrap", ...overviewTypography.description }}
          >
            GitHub
            <OpenInNew sx={{ fontSize: 13 }} />
          </Link>
        )}
      </Box>
    </Box>
  );
}

function SharedFailureEscalation({ failure, enabled, hasBaseAnalysis }: {
  failure: SharedFailure;
  enabled: boolean;
  hasBaseAnalysis: boolean;
}) {
  // Without the capability there is no analysis to offer, so explaining why one
  // is unavailable would describe a control this deploy never had.
  if (!enabled) return null;
  const blocked = sharedFailureBlockedReason(failure);
  const member = evidenceMember(failure);
  return (
    <Box component="section" sx={{ minWidth: 0, bgcolor: "surface.container" }}>
      <DetailSectionBand title={hasBaseAnalysis ? "Check a pull request build" : "Pull request analysis"} metadata="Read-only investigation" />
      <Box sx={{ px: { xs: 1.5, sm: 2 }, py: 1.5 }}>
        {blocked ? (
          <Typography color="textSecondary" sx={overviewTypography.secondaryBody}>{blocked}</Typography>
        ) : (
          <>
            <Typography color="textSecondary" sx={overviewTypography.secondaryBody}>
              {hasBaseAnalysis
                ? "Analyze a representative PR build to compare its evidence with the base-branch diagnosis."
                : "Analyze a representative pull request build to investigate this failure."}
              {member && ` The current representative is the newest usable build, ${member.build_id} on #${member.number}. Completed results name the build they read.`}
            </Typography>
            <EscalationPanel
              enabled={enabled}
              subjectKey={failure.id}
              title="Representative PR build"
              startLabel="Analyze PR build"
              load={() => getSharedFailureEscalation(failure.id)}
              start={(key) => startSharedFailureEscalation(failure.id, key)}
              disclaimer="This analysis reads one pull request build. It does not verify a shared cause or establish that any pull request caused it."
            />
          </>
        )}
      </Box>
    </Box>
  );
}

function SharedFailureAnalysis({
  failure,
  escalationEnabled,
}: {
  failure: SharedFailure;
  escalationEnabled: boolean;
}) {
  const manifest = useManifest();
  const [selectedJobID, setSelectedJobID] = useState("");
  const bases = failure.base_branch_failures ?? [];
  const base = bases.find((candidate) => candidate.job_id === selectedJobID) ?? bases[0];
  const prefix = manifest.short_name_prefix ?? "";
  const jobName = base ? shortJobName(base.job_name, prefix) : "";
  const { data, loading, error } = useJobDetail(base?.job_id);
  const found = base ? baseBranchCase(data, base, failure.test_name) : undefined;
  const runPath = base ? testRunPath(base.job_id, failure.test_name, base.build_id) : "";
  const runLink = base ? (
    <Link component={RouterLink} to={runPath} underline="none">
      {jobName} build {base.build_id}
    </Link>
  ) : null;
  const analysis = found?.testCase.ai_analysis;
  return (
    <Stack spacing={2} sx={{ minWidth: 0 }}>
      {bases.length > 1 && (
        <TextField
          select
          label="Base-branch job"
          size="small"
          value={base.job_id}
          onChange={(event) => setSelectedJobID(event.target.value)}
          fullWidth
        >
          {bases.map((candidate) => (
            <MenuItem key={candidate.job_id} value={candidate.job_id} sx={{ whiteSpace: "normal", overflowWrap: "anywhere" }}>
              {shortJobName(candidate.job_name, prefix)}
            </MenuItem>
          ))}
        </TextField>
      )}
      {base && found && analysis ? (
        <TestAnalysisBriefing
          key={`${base.job_id}/${base.build_id}`}
          analysis={analysis}
          summary={found.testCase.ai_summary?.summary}
          metadata={`${analysis.severity} severity · Base branch run`}
          context={(
            <Typography color="textSecondary" sx={overviewTypography.description}>
              From {runLink} on {failure.base_ref}. Investigation and fixes here apply to this base-branch run.
            </Typography>
          )}
          fileCtx={testRunFileContext(found.run, found.testCase)}
          buildWebURL={found.run.web_url}
          traceRef={{ job_id: data?.job_id ?? base.job_id, build_id: found.run.build_id, test_name: found.testCase.name }}
          fixPatterns={fixPatternsForRun(data?.pattern_analyses, found.run.build_id)}
          chatTitle="Investigate base-branch failure"
          chatRef={{
            job_id: data?.job_id ?? base.job_id,
            build_id: found.run.build_id,
            test_name: found.testCase.name,
            suite_name: found.testCase.suite_name,
            class_name: found.testCase.class_name,
            junit_file: found.testCase.junit_file,
            analysis_generated_at: analysis.generated_at,
          }}
        />
      ) : base ? (
        <Box component="section" sx={{ bgcolor: "surface.container" }}>
          <DetailSectionBand title="Base branch run" metadata={jobName} />
          <Typography color="textSecondary" sx={{ px: { xs: 1.5, sm: 2 }, py: 1.5, ...overviewTypography.secondaryBody }}>
            The same test also fails in {runLink} on {failure.base_ref}.{" "}
            {loading ? "Loading the published analysis." : error ? "The base-branch run could not be loaded." : "No analysis is published for this run."}
          </Typography>
        </Box>
      ) : null}
      {!base && !escalationEnabled && (
        <Box component="section" sx={{ bgcolor: "surface.container" }}>
          <DetailSectionBand title="Analysis briefing" metadata="Unavailable" />
          <Typography color="textSecondary" sx={{ px: { xs: 1.5, sm: 2 }, py: 1.5, ...overviewTypography.secondaryBody }}>
            No base-branch analysis is published for this group. Review the affected pull requests and their artifacts for failure evidence.
          </Typography>
        </Box>
      )}
      <SharedFailureEscalation failure={failure} enabled={escalationEnabled} hasBaseAnalysis={Boolean(analysis)} />
    </Stack>
  );
}

export function SharedFailurePage() {
  const { id } = useParams<{ id: string }>();
  const manifest = useManifest();
  const enabled = manifest.pull_requests?.enabled ?? false;
  const includePresubmits = manifest.discovery?.include_presubmits ?? false;
  const { data: dashboard } = useDashboard();
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
  const linkToJob = isPublishedPresubmitJob(failure.job_id, includePresubmits, dashboard?.jobs);
  const prefix = manifest.short_name_prefix ?? "";
  const jobName = shortJobName(failure.job_name, prefix);
  const title = failure.build_level ? jobName : parseTestDisplayName(failure.test_name).displayName;
  const bases = failure.base_branch_failures ?? [];

  return (
    <Stack spacing={{ xs: 2.5, sm: 3.5 }} sx={{ minWidth: 0, maxWidth: "100%", overflowX: "clip" }}>
      {breadcrumbs}

      <Box sx={{ minWidth: 0 }}>
        <Typography
          component="h1"
          title={sharedFailureSubject(failure)}
          sx={{
            ...overviewTypography.pageHeadline,
            fontSize: { xs: "26px", sm: "30px" },
            lineHeight: { xs: "33px", sm: "38px" },
            fontWeight: 720,
            color: "text.primary",
            overflowWrap: "anywhere",
          }}
        >
          {title}
        </Typography>
        <Typography component="p" color="textSecondary" sx={{ m: 0, mt: 0.75, ...overviewTypography.secondaryBody }}>
          Failing on {sharedFailureScope(failure)}
          {" · "}
          {linkToJob ? (
            <Link component={RouterLink} to={jobPath(failure.job_id)} underline="none">
              {jobName}
            </Link>
          ) : (
            jobName
          )}
        </Typography>
      </Box>

      <Box component="section" sx={{ minWidth: 0, bgcolor: "surface.container" }}>
        <DetailSectionBand title="Why these failures are grouped" />
        <Box sx={{ px: { xs: 1.5, sm: 2 }, py: 1.5, borderTop: "1px solid", borderColor: "divider" }}>
          <Typography sx={overviewTypography.secondaryBody}>
            These {failure.pull_requests.length} pull requests failed {failure.build_level ? "the same job" : "the same test in this presubmit job"} while targeting {failure.base_ref || "the same base branch"}.
            {bases.length > 0 && ` The same test also fails on ${failure.base_ref}.`}
          </Typography>
          <Typography color="textSecondary" sx={{ mt: 0.75, ...overviewTypography.secondaryBody }}>
            Matching causes have not been verified. These changes are not established to be independent; one may be stacked on another.
          </Typography>
        </Box>
      </Box>

      <Box
        sx={{
          display: "grid",
          minWidth: 0,
          gridTemplateColumns: { xs: "minmax(0, 1fr)", lg: "minmax(0, 1.5fr) minmax(360px, 0.85fr)" },
          gap: 2,
          alignItems: "start",
        }}
      >
        <SharedFailureAnalysis
          key={failure.id}
          failure={failure}
          escalationEnabled={escalationEnabled}
        />
        <Stack spacing={2} sx={{ minWidth: 0 }}>
          <Box component="section" sx={{ minWidth: 0, bgcolor: "surface.container" }}>
            <DetailSectionBand title="Affected pull requests" metadata={`${failure.pull_requests.length} observed`} />
            {failure.pull_requests.map((member) => (
              <MemberRow
                key={member.number}
                member={member}
                wouldSupplyEvidence={escalationEnabled && failure.escalatable && evidence?.number === member.number}
              />
            ))}
          </Box>
          <Box>
            <RunMetadata
              title="Observed builds"
              status="Current pass"
              stacked
              items={[
                { label: "Oldest build seen", value: formatTimestamp(failure.oldest_build_started) },
                { label: "Newest build seen", value: formatTimestamp(failure.newest_build_started) },
              ]}
              links={[]}
            />
            <Typography color="textSecondary" sx={{ px: 1.5, py: 1, ...overviewTypography.description }}>
              This window covers the builds observed in the current pass, not when the failure started.
            </Typography>
          </Box>
        </Stack>
      </Box>
      <TechnicalIdentity items={[
        { label: "Job", value: failure.job_name, copyLabel: "Copy job name" },
        ...(!failure.build_level ? [{ label: "Test", value: failure.test_name, copyLabel: "Copy test name" }] : []),
      ]} />
    </Stack>
  );
}
