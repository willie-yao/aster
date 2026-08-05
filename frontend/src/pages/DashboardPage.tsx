import Box from "@mui/material/Box";
import Typography from "@mui/material/Typography";
import { useMemo, useState } from "react";
import { useDashboard, useFlakinessReport } from "../hooks/useData";
import { useManifest } from "../hooks/useManifest";
import {
  timeAgo,
  groupByCategory,
  categoryLabelsFromRules,
  categoryDisplayOrder,
} from "../lib/utils";
import type { JobSummary } from "../types/dashboard";
import { HealthPanel } from "../components/HealthPanel";
import { NeedsAttention } from "../components/NeedsAttention";
import { JobHealthTable, type JobHealthSection } from "../components/JobHealthTable";
import { OverviewFilters } from "../components/OverviewFilters";
import {
  MAX_OVERVIEW_PATTERNS,
  orderedDashboardBranches,
  overviewHeadlineForReport,
  type OverviewStatusFilter,
} from "../lib/dashboardOverview";
import { LoadingState } from "../components/LoadingState";
import { ErrorState } from "../components/ErrorState";
import { overviewLayout, overviewTypography } from "../theme/overview";

function EmptyDashboardState({ generatedAt }: { generatedAt: string }) {
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
        No jobs available yet
      </Typography>
      <Typography color="text.secondary" sx={{ mt: 1.5 }}>
        The dashboard loaded, but the latest fetch did not publish any Prow jobs.
        Discovery may have found no matches, or every job may have failed while
        loading build data.
      </Typography>
      <Typography color="text.secondary" sx={{ mt: 1 }}>
        Check the fetcher logs first. Fix storage or artifact errors if jobs failed
        to load. Otherwise verify <Box component="code">testgrid.dashboard</Box> or
        the bucket discovery settings in <Box component="code">project.yaml</Box>.
      </Typography>
      <Box
        component="code"
        sx={{
          display: "block",
          mt: 3,
          p: 1.5,
          borderRadius: "4px",
          bgcolor: "surface.containerHigh",
          color: "text.primary",
          fontFamily: "monospace",
          fontSize: "0.8rem",
          overflowWrap: "anywhere",
        }}
      >
        fetcher -project-dir=&lt;consumer&gt; -ai=false -builds=1
      </Box>
      <Typography variant="data" color="text.secondary" sx={{ display: "block", mt: 2 }}>
        Last generated {timeAgo(generatedAt)}
      </Typography>
    </Box>
  );
}

export function DashboardPage() {
  const { data, loading, error } = useDashboard();
  const attention = useFlakinessReport();
  const manifest = useManifest();
  const categoryLabels = useMemo(
    () => categoryLabelsFromRules(manifest.categories),
    [manifest.categories],
  );
  const categoryOrder = useMemo(
    () => categoryDisplayOrder(manifest.categories, manifest.category_display_order),
    [manifest.categories, manifest.category_display_order],
  );
  const [statusFilter, setStatusFilter] = useState<OverviewStatusFilter>("ALL");
  const [branchFilter, setBranchFilter] = useState("ALL");

  const branches = useMemo(
    () => (data ? orderedDashboardBranches(data.jobs) : []),
    [data],
  );

  const filtered = useMemo(() => {
    if (!data) return [];
    return data.jobs.filter((job: JobSummary) => {
      if (statusFilter !== "ALL" && job.overall_status !== statusFilter) return false;
      if (branchFilter !== "ALL" && job.branch !== branchFilter) return false;
      return true;
    });
  }, [data, statusFilter, branchFilter]);

  const grouped = useMemo(() => groupByCategory(filtered), [filtered]);
  const hasCategories = (manifest.categories?.length ?? 0) > 0;

  if (loading) return <LoadingState />;
  if (error) return <ErrorState message={error} onRetry={() => window.location.reload()} />;
  if (!data) return null;
  if (data.jobs.length === 0) return <EmptyDashboardState generatedAt={data.generated_at} />;

  const sortedCategories = Object.keys(grouped).sort((a, b) => {
    const ai = categoryOrder.indexOf(a);
    const bi = categoryOrder.indexOf(b);
    return (ai === -1 ? 999 : ai) - (bi === -1 ? 999 : bi);
  });
  const sections: JobHealthSection[] = hasCategories
    ? sortedCategories.map((category) => ({
        id: category,
        label: categoryLabels[category] ?? category.charAt(0).toUpperCase() + category.slice(1),
        jobs: grouped[category],
      }))
    : [{ id: "all-jobs", jobs: filtered }];
  const failingJobs = data.jobs.filter((job) => job.overall_status === "FAILING").length;
  const recurringPatterns = attention.data
    ? Math.min(
        (attention.data.recurring_patterns ?? []).filter((pattern) => pattern.job_id).length,
        MAX_OVERVIEW_PATTERNS,
      )
    : null;
  const headline = overviewHeadlineForReport(failingJobs, recurringPatterns, attention.loading, Boolean(attention.error));
  const jobsByID = Object.fromEntries(data.jobs.map((job) => [job.job_id, job]));

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
        <Box>
          <Typography variant="label" component="p" color="text.secondary" sx={overviewTypography.eyebrow}>
            Incident briefing
          </Typography>
          <Typography variant="h4" component="h1" sx={{ mt: 0.5, ...overviewTypography.pageHeadline }}>
            {headline}
          </Typography>
        </Box>
        <Typography variant="data" color="text.secondary" sx={{ justifySelf: { sm: "end" }, ...overviewTypography.data }}>
          Updated {timeAgo(data.generated_at)}
        </Typography>
      </Box>

      <HealthPanel
        jobs={data.jobs}
        onFilterClick={(status) => setStatusFilter(status as OverviewStatusFilter)}
        activeFilter={statusFilter}
      />

      <NeedsAttention
        report={attention.data}
        loading={attention.loading}
        error={attention.error}
        jobsByID={jobsByID}
      />

      <Box component="section" aria-labelledby="job-ledger-heading">
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
            id="job-ledger-heading"
            variant="headline"
            component="h2"
            sx={overviewTypography.majorHeading}
          >
            Job ledger
          </Typography>
        </Box>

        <OverviewFilters
          statusFilter={statusFilter}
          branchFilter={branchFilter}
          branches={branches}
          matchingJobs={filtered.length}
          onStatusChange={setStatusFilter}
          onBranchChange={setBranchFilter}
        />

        {filtered.length === 0 ? (
          <Box sx={{ py: 8, textAlign: "center" }}>
            <Typography color="text.secondary">No jobs match filters</Typography>
          </Box>
        ) : (
          <JobHealthTable sections={sections} />
        )}
      </Box>
    </Box>
  );
}
