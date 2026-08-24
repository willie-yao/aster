import { useEffect, useId, useLayoutEffect, useMemo, useRef, useState, type ReactNode } from "react";
import ChevronRight from "@mui/icons-material/ChevronRight";
import Download from "@mui/icons-material/Download";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import ButtonBase from "@mui/material/ButtonBase";
import CircularProgress from "@mui/material/CircularProgress";
import Collapse from "@mui/material/Collapse";
import Typography from "@mui/material/Typography";
import { useLocation, useSearchParams } from "react-router-dom";
import { AnalysisTraceFilters } from "../components/AnalysisTraceFilters";
import {
  AnalysisTraceLedger,
  TraceNotice,
  type AnalysisTraceLedgerItem,
} from "../components/AnalysisTraceLedger";
import { DetailSectionBand } from "../components/DetailSectionBand";
import { RefreshPipelineDetails } from "../components/FetchStatus";
import { MetricStrip, type MetricStripItem } from "../components/MetricStrip";
import { useAuth } from "../hooks/useAuth";
import { useCapabilities } from "../hooks/useCapabilities";
import { useSharedFetchStatus } from "../hooks/useSharedFetchStatus";
import { fetchStatusCompactPresentation } from "../lib/fetchStatus";
import type { FetchStatusResponse } from "../types/fetchStatus";
import { useManifest } from "../hooks/useManifest";
import {
  analysisHealthCounts,
  analysisHealthSeverities,
  analysisHealthSeverityDescriptions,
  analysisHealthSeverityLabels,
  rankAnalysisHealth,
  type AnalysisHealthSeverity,
} from "../lib/analysisHealth";
import {
  analysisTraceActiveFilterCount,
  analysisTraceResponseIDs,
} from "../lib/analysisTraces";
import { parseTestDisplayName } from "../lib/detailTitles";
import { testRunPath } from "../lib/routes";
import { shortJobName } from "../lib/utils";
import { overviewLayout, overviewTypography } from "../theme/overview";
import type { AnalysisTraceFile } from "../types/traces";

const API_BASE = import.meta.env.BASE_URL;

function AnalysisHealthPageFrame({
  children,
  action,
}: {
  children: ReactNode;
  action?: ReactNode;
}) {
  return (
    <Box sx={{ display: "flex", flexDirection: "column", gap: { xs: 2.5, sm: overviewLayout.majorSectionGap } }}>
      <Box
        sx={{
          display: "grid",
          gridTemplateColumns: { xs: "minmax(0, 1fr)", sm: "minmax(0, 1fr) auto" },
          gridTemplateAreas: { xs: '"title" "action"', sm: '"title action"' },
          alignItems: "center",
          columnGap: 2,
          rowGap: 1,
        }}
      >
        <Typography component="h1" sx={{ gridArea: "title", ...overviewTypography.pageHeadline }}>
          Analysis Health
        </Typography>
        {action && <Box sx={{ gridArea: "action", justifySelf: { xs: "start", sm: "end" }, alignSelf: "end" }}>{action}</Box>}
      </Box>
      {children}
    </Box>
  );
}

function TracePrivacyDisclosure() {
  const [open, setOpen] = useState(false);
  const generatedID = useId();
  const contentID = `trace-privacy-${generatedID.replaceAll(":", "")}`;

  return (
    <Box component="section" sx={{ bgcolor: "surface.container", borderBottom: "1px solid", borderColor: "divider" }}>
      <ButtonBase
        type="button"
        onClick={() => setOpen((value) => !value)}
        aria-expanded={open}
        aria-controls={contentID}
        sx={{
          width: "100%",
          minHeight: 44,
          px: 1.5,
          py: 0.75,
          justifyContent: "flex-start",
          gap: 1,
          bgcolor: "surface.containerHigh",
          color: "text.primary",
          textAlign: "left",
          "&:hover": { bgcolor: "surface.containerHighest" },
          "&.Mui-focusVisible": {
            outline: "2px solid",
            outlineColor: "primary.main",
            outlineOffset: -2,
          },
        }}
      >
        <Typography component="span" sx={{ ...overviewTypography.secondaryBody, fontWeight: 700 }}>
          About trace privacy
        </Typography>
        <ChevronRight
          aria-hidden="true"
          sx={{
            ml: "auto",
            fontSize: 20,
            color: "text.secondary",
            transform: open ? "rotate(90deg)" : "rotate(0deg)",
            transition: (theme) => theme.transitions.create("transform", { duration: theme.transitions.duration.shortest }),
            "@media (prefers-reduced-motion: reduce)": { transition: "none" },
          }}
        />
      </ButtonBase>
      <Collapse in={open} timeout="auto">
        <Typography
          id={contentID}
          color="textSecondary"
          sx={{ px: 1.5, py: 1.5, borderTop: "1px solid", borderColor: "divider", ...overviewTypography.secondaryBody }}
        >
          Health is derived from private, content-free runtime metadata. Prompts, tool arguments, tool results, credentials, diagnostic content, and billing records are never shown. Cached analyses are omitted because they record no new evidence.
        </Typography>
      </Collapse>
    </Box>
  );
}

function LoadingHealthSection() {
  return (
    <Box component="section" aria-busy="true" aria-label="Loading analysis health" sx={{ bgcolor: "surface.container", borderBottom: "1px solid", borderColor: "divider" }}>
      <DetailSectionBand title="Analysis health" metadata="Loading" />
      <Box sx={{ minHeight: 180, display: "grid", placeItems: "center" }}>
        <CircularProgress size={28} aria-label="Loading analysis health" />
      </Box>
    </Box>
  );
}

function EmptyHealthSection({ activeFilters, onClear }: { activeFilters: number; onClear: () => void }) {
  return (
    <Box component="section" sx={{ bgcolor: "surface.container", borderBottom: "1px solid", borderColor: "divider" }}>
      <DetailSectionBand title="Analysis health" metadata="0 retained analyses" />
      <Box sx={{ minHeight: 220, display: "grid", placeItems: "center", px: 2, py: 4, textAlign: "center" }}>
        <Box>
          <Typography component="h2" sx={{ ...overviewTypography.majorHeading, fontSize: "20px" }}>
            No retained analyses
          </Typography>
          <Typography color="textSecondary" sx={{ mt: 0.75, maxWidth: 620, ...overviewTypography.primaryBody }}>
            {activeFilters > 0
              ? "No retained analysis matches the current URL filters. Clear all filters to return to the retained ledger."
              : "Analyses served entirely from cache are not retained. This fills in once the pipeline runs a fresh analysis."}
          </Typography>
          {activeFilters > 0 && (
            <Button type="button" variant="contained" onClick={onClear} sx={{ mt: 2.5, minHeight: 44, borderRadius: "4px" }}>
              Clear all filters
            </Button>
          )}
        </Box>
      </Box>
    </Box>
  );
}

function ErrorHealthSection() {
  return (
    <Box component="section" sx={{ bgcolor: "surface.container", borderBottom: "1px solid", borderColor: "divider" }}>
      <DetailSectionBand title="Analysis health" metadata="Unavailable" />
      <Box sx={{ minHeight: 150, display: "grid", placeItems: "center", px: 2, py: 3, textAlign: "center" }}>
        <Typography color="textSecondary" sx={overviewTypography.primaryBody}>
          The retained analysis ledger could not be loaded.
        </Typography>
      </Box>
    </Box>
  );
}

function PrivateAccessSection({ onSignIn }: { onSignIn: () => void }) {
  return (
    <Box component="section" sx={{ bgcolor: "surface.container", borderBottom: "1px solid", borderColor: "divider" }}>
      <DetailSectionBand title="Private operator access" metadata="Authentication required" />
      <Box sx={{ minHeight: 236, display: "grid", placeItems: "center", px: 2, py: 4, textAlign: "center" }}>
        <Box>
          <Typography component="h2" sx={{ ...overviewTypography.majorHeading, fontSize: "20px" }}>
            Operator sign-in required
          </Typography>
          <Typography color="textSecondary" sx={{ mt: 0.75, maxWidth: 620, ...overviewTypography.primaryBody }}>
            Analysis health is derived from private execution metadata and is available only to authenticated dashboard administrators.
          </Typography>
          <Button type="button" variant="contained" onClick={onSignIn} sx={{ mt: 2.5, minHeight: 44, borderRadius: "4px" }}>
            Sign in to inspect analysis health
          </Button>
        </Box>
      </Box>
    </Box>
  );
}

function AllHealthySection() {
  return (
    <Box component="section" sx={{ bgcolor: "surface.container", borderBottom: "1px solid", borderColor: "divider" }}>
      <DetailSectionBand title="Nothing needs attention" metadata="All retained analyses completed cleanly" />
      <Typography color="textSecondary" sx={{ px: 1.5, py: 2, ...overviewTypography.primaryBody }}>
        No retained analysis failed, was degraded, or needed a retry. Healthy analyses are listed below.
      </Typography>
    </Box>
  );
}

export function AnalysisHealthPage() {
  const { features } = useCapabilities();
  const auth = useAuth();
  const manifest = useManifest();
  const fetchStatus = useSharedFetchStatus();
  const [searchParams, setSearchParams] = useSearchParams();
  const query = searchParams.toString();
  const [showHealthy, setShowHealthy] = useState(false);
  const [loaded, setLoaded] = useState<{
    key: string | null;
    data: AnalysisTraceFile | null;
    error: string | null;
  }>({
    key: null,
    data: null,
    error: null,
  });

  useEffect(() => {
    if (!features.analysis_health || auth.status !== "authenticated") return;
    const controller = new AbortController();
    fetch(`${API_BASE}api/analysis-health${query ? `?${query}` : ""}`, {
      credentials: "same-origin",
      signal: controller.signal,
    })
      .then(async (response) => {
        if (response.status === 404) {
          return {
            version: 1,
            generated_at: "",
            traces: [],
          } as AnalysisTraceFile;
        }
        if (!response.ok) throw new Error(`HTTP ${response.status}`);
        return response.json() as Promise<AnalysisTraceFile>;
      })
      .then((data) => setLoaded({ key: query, data, error: null }))
      .catch((err: unknown) => {
        if (!controller.signal.aborted) {
          setLoaded({
            key: query,
            data: null,
            error: err instanceof Error ? err.message : "Unable to load analysis health",
          });
        }
      });
    return () => controller.abort();
  }, [auth.status, features.analysis_health, query]);

  const data = loaded.key === query ? loaded.data : null;
  const error = loaded.key === query ? loaded.error : null;
  const loading = auth.status === "authenticated" && loaded.key !== query;
  const activeFilters = analysisTraceActiveFilterCount(searchParams);

  const groups = useMemo(() => {
    const prefix = manifest.short_name_prefix ?? "";
    const ranked = rankAnalysisHealth(data?.traces ?? [], (trace) => trace);
    const counts = analysisHealthCounts(ranked);
    const bySeverity = new Map<AnalysisHealthSeverity, AnalysisTraceLedgerItem[]>(
      analysisHealthSeverities.map((severity) => [severity, []]),
    );
    for (const { item: trace, verdict } of ranked) {
      bySeverity.get(verdict.severity)?.push({
        trace,
        verdict,
        displayTitle: parseTestDisplayName(trace.test_name).displayName,
        displayJob: shortJobName(trace.job_id, prefix),
        testHref: testRunPath(trace.job_id, trace.test_name, trace.build_id),
        responseIDs: analysisTraceResponseIDs(trace),
      });
    }
    return { total: ranked.length, counts, bySeverity };
  }, [data, manifest.short_name_prefix]);

  function clearFilters() {
    setSearchParams(new URLSearchParams());
  }

  if (!features.analysis_health) {
    return (
      <AnalysisHealthPageFrame>
        <TraceNotice severity="info" title="Private analysis health is not available in this deployment." />
      </AnalysisHealthPageFrame>
    );
  }

  if (auth.status === "loading") {
    return (
      <AnalysisHealthPageFrame>
        <Box component="section" aria-busy="true" sx={{ bgcolor: "surface.container", borderBottom: "1px solid", borderColor: "divider" }}>
          <DetailSectionBand title="Private operator access" metadata="Checking session" />
          <Box sx={{ minHeight: 180, display: "grid", placeItems: "center" }}>
            <CircularProgress size={28} aria-label="Checking authentication" />
          </Box>
        </Box>
      </AnalysisHealthPageFrame>
    );
  }

  if (auth.status === "anonymous") {
    return (
      <AnalysisHealthPageFrame>
        <PrivateAccessSection onSignIn={auth.signIn} />
      </AnalysisHealthPageFrame>
    );
  }

  const downloadHref = `${API_BASE}api/analysis-health/download${query ? `?${query}` : ""}`;
  const attention = groups.counts.failed + groups.counts.degraded;
  const metricItems: MetricStripItem[] = [
    {
      label: "Needs attention",
      value: attention,
      color: attention > 0 ? "error.main" : "success.main",
    },
    { label: "Failed", value: groups.counts.failed, color: groups.counts.failed > 0 ? "error.main" : "text.primary" },
    { label: "Degraded", value: groups.counts.degraded, color: groups.counts.degraded > 0 ? "warning.main" : "text.primary" },
    { label: "Recovered", value: groups.counts.retried },
  ];
  const problemSeverities: AnalysisHealthSeverity[] = analysisHealthSeverities.filter(
    (severity) => severity !== "healthy" && (groups.bySeverity.get(severity)?.length ?? 0) > 0,
  );
  const healthyItems = groups.bySeverity.get("healthy") ?? [];

  return (
    <AnalysisHealthPageFrame
      action={
        <Button
          component="a"
          href={downloadHref}
          startIcon={<Download />}
          variant="outlined"
          sx={{ minHeight: { xs: 44, sm: 40 }, borderRadius: "4px" }}
        >
          Download JSON
        </Button>
      }
    >
      <AnalysisTraceFilters
        key={query}
        searchParams={searchParams}
        onApply={setSearchParams}
        onClear={clearFilters}
      />

      <MetricStrip items={metricItems} label="Analysis health metrics" />

      {data?.dropped_traces ? (
        <TraceNotice severity="warning" title={`${data.dropped_traces} analyses were dropped by the bounded recorder.`}>
          {data.retained_since
            ? `Entries recorded before ${new Date(data.retained_since).toLocaleString()} are outside the retained window. Current retained analyses remain available below.`
            : "Current retained analyses remain available below."}
        </TraceNotice>
      ) : null}

      {error && (
        <TraceNotice severity="error" title="Failed to load analysis health">
          {error}
        </TraceNotice>
      )}

      {loading ? (
        <LoadingHealthSection />
      ) : error ? (
        <ErrorHealthSection />
      ) : groups.total === 0 ? (
        <EmptyHealthSection activeFilters={activeFilters} onClear={clearFilters} />
      ) : (
        <>
          {problemSeverities.length === 0 ? (
            <AllHealthySection />
          ) : (
            problemSeverities.map((severity) => (
              <AnalysisTraceLedger
                key={severity}
                title={analysisHealthSeverityLabels[severity]}
                description={analysisHealthSeverityDescriptions[severity]}
                items={groups.bySeverity.get(severity) ?? []}
              />
            ))
          )}

          {healthyItems.length > 0 && (
            <Box>
              <Button
                type="button"
                variant="outlined"
                onClick={() => setShowHealthy((value) => !value)}
                aria-expanded={showHealthy}
                sx={{ minHeight: 44, borderRadius: "4px" }}
              >
                {showHealthy ? "Hide" : "Show"} {healthyItems.length} healthy{" "}
                {healthyItems.length === 1 ? "analysis" : "analyses"}
              </Button>
              <Collapse in={showHealthy} timeout="auto" unmountOnExit>
                <Box sx={{ mt: 1.5 }}>
                  <AnalysisTraceLedger
                    title={analysisHealthSeverityLabels.healthy}
                    description={analysisHealthSeverityDescriptions.healthy}
                    items={healthyItems}
                  />
                </Box>
              </Collapse>
            </Box>
          )}
        </>
      )}

      <RefreshPipelineSection response={fetchStatus} ready={!loading} />

      <TracePrivacyDisclosure />
    </AnalysisHealthPageFrame>
  );
}

// The refresh pipeline is a sibling of analysis health, not part of it: traces
// are keyed per analysis, this is keyed per refresh pass. It lives here because
// both are operator-only surfaces and this one needs more room than the status
// popover can give it.
function RefreshPipelineSection({
  response,
  ready,
}: {
  response: FetchStatusResponse | null;
  ready: boolean;
}) {
  const location = useLocation();
  const handledKey = useRef<string | null>(null);

  // The section mounts after the browser has handled the hash, so the deep link
  // from the status popover scrolls itself once the trace ledger above it has
  // settled and stopped changing this section's position. Keyed on the history
  // entry so following the link again re-scrolls instead of doing nothing.
  useLayoutEffect(() => {
    if (!response || !ready) return;
    if (location.hash !== "#refresh-pipeline") return;
    if (handledKey.current === location.key) return;
    const frame = requestAnimationFrame(() => {
      handledKey.current = location.key;
      const heading = document.getElementById("refresh-pipeline-heading");
      const target = document.getElementById("refresh-pipeline");
      if (!heading || !target) return;
      const reducedMotion = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
      heading.focus({ preventScroll: true });
      target.scrollIntoView({ behavior: reducedMotion ? "auto" : "smooth", block: "start" });
    });
    return () => cancelAnimationFrame(frame);
  }, [location.hash, location.key, ready, response]);

  // A null response means the endpoint is not enabled for this viewer at all.
  if (!response) return null;

  return (
    <Box id="refresh-pipeline" sx={{ scrollMarginTop: 16 }}>
      <DetailSectionBand
        id="refresh-pipeline-heading"
        headingTabIndex={-1}
        title="Refresh pipeline"
        metadata={fetchStatusCompactPresentation(response)?.label ?? "Unavailable"}
      />
      <Box sx={{ mt: 2 }}>
        {response.status ? (
          <RefreshPipelineDetails response={response} />
        ) : (
          // Reported but with no snapshot. This is exactly when an operator
          // comes looking, so say so rather than hiding the section.
          <TraceNotice severity="warning" title={missingSnapshotTitle(response.state)}>
            {missingSnapshotDetail(response.state)}
          </TraceNotice>
        )}
      </Box>
    </Box>
  );
}

// A status-less response means different things per state, and an operator
// reading this is trying to work out whether the pipeline is broken or simply
// has not run.
function missingSnapshotTitle(state: FetchStatusResponse["state"]): string {
  return state === "unavailable"
    ? "Refresh progress could not be read."
    : "No refresh snapshot is available.";
}

function missingSnapshotDetail(state: FetchStatusResponse["state"]): string {
  switch (state) {
    case "unavailable":
      return "The server could not read or validate the progress file. Pipeline diagnostics return once a pass writes a readable snapshot.";
    case "missing":
      return "No progress file has been written yet. Pipeline diagnostics appear here once a refresh pass reports.";
    default:
      return "The server has not reported a snapshot for this state. Pipeline diagnostics appear here once a refresh pass reports.";
  }
}
