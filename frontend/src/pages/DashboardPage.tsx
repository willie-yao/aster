import Box from "@mui/material/Box";
import Typography from "@mui/material/Typography";
import { useMemo, useState } from "react";
import { useDashboard } from "../hooks/useData";
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
import { JobHealthTable } from "../components/JobHealthTable";
import { OverviewFilters } from "../components/OverviewFilters";
import { orderedDashboardBranches, type OverviewStatusFilter } from "../lib/dashboardOverview";
import { LoadingState } from "../components/LoadingState";
import { ErrorState } from "../components/ErrorState";

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

  return (
    <Box sx={{ display: "flex", flexDirection: "column", gap: { xs: 3, sm: 3.5 } }}>
      <Box>
        <Typography variant="h4" component="h1">
          Test Health Overview
        </Typography>
        <Typography variant="data" color="text.secondary" sx={{ display: "block", mt: 0.5 }}>
          Updated {timeAgo(data.generated_at)}
        </Typography>
      </Box>

      <Box
        sx={{
          display: "grid",
          gridTemplateColumns: { xs: "1fr", lg: "300px minmax(0, 1fr)" },
          gap: 2,
          alignItems: "start",
        }}
      >
        <HealthPanel
          jobs={data.jobs}
          onFilterClick={(status) => setStatusFilter(status as OverviewStatusFilter)}
          activeFilter={statusFilter}
        />
        <NeedsAttention />
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
      ) : !hasCategories ? (
        <JobHealthTable jobs={filtered} />
      ) : (
        sortedCategories.map((category) => (
          <Box key={category} component="section">
            <Box sx={{ display: "flex", alignItems: "center", gap: 1, mb: 1.5 }}>
              <Box sx={{ width: 2, height: 18, bgcolor: "primary.main", flexShrink: 0 }} />
              <Typography variant="headline" component="h2">
                {categoryLabels[category] ??
                  category.charAt(0).toUpperCase() + category.slice(1)}
              </Typography>
              <Typography variant="data" color="text.secondary">
                {grouped[category].length}
              </Typography>
            </Box>
            <JobHealthTable jobs={grouped[category]} />
          </Box>
        ))
      )}
    </Box>
  );
}
