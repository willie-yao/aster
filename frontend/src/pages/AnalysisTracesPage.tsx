import { useEffect, useId, useMemo, useState, type ReactNode } from "react";
import ChevronRight from "@mui/icons-material/ChevronRight";
import Download from "@mui/icons-material/Download";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import ButtonBase from "@mui/material/ButtonBase";
import CircularProgress from "@mui/material/CircularProgress";
import Collapse from "@mui/material/Collapse";
import Typography from "@mui/material/Typography";
import { useSearchParams } from "react-router-dom";
import { AnalysisTraceFilters } from "../components/AnalysisTraceFilters";
import {
  AnalysisTraceLedger,
  TraceNotice,
  type AnalysisTraceLedgerItem,
} from "../components/AnalysisTraceLedger";
import { DetailSectionBand } from "../components/DetailSectionBand";
import { MetricStrip, type MetricStripItem } from "../components/MetricStrip";
import { useAuth } from "../hooks/useAuth";
import { useCapabilities } from "../hooks/useCapabilities";
import { useManifest } from "../hooks/useManifest";
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

function AnalysisTracesPageFrame({
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
          Analysis Traces
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
          Traces contain private, content-free runtime metadata. Prompts, tool arguments, tool results, credentials, diagnostic content, and billing records are never shown.
        </Typography>
      </Collapse>
    </Box>
  );
}

function LoadingTraceSection() {
  return (
    <Box component="section" aria-busy="true" aria-label="Loading analysis traces" sx={{ bgcolor: "surface.container", borderBottom: "1px solid", borderColor: "divider" }}>
      <DetailSectionBand title="Trace ledger" metadata="Loading" />
      <Box sx={{ minHeight: 180, display: "grid", placeItems: "center" }}>
        <CircularProgress size={28} aria-label="Loading traces" />
      </Box>
    </Box>
  );
}

function EmptyTraceSection({ activeFilters, onClear }: { activeFilters: number; onClear: () => void }) {
  return (
    <Box component="section" sx={{ bgcolor: "surface.container", borderBottom: "1px solid", borderColor: "divider" }}>
      <DetailSectionBand title="Trace ledger" metadata="0 matching traces" />
      <Box sx={{ minHeight: 220, display: "grid", placeItems: "center", px: 2, py: 4, textAlign: "center" }}>
        <Box>
          <Typography component="h2" sx={{ ...overviewTypography.majorHeading, fontSize: "20px" }}>
            No matching traces
          </Typography>
          <Typography color="textSecondary" sx={{ mt: 0.75, maxWidth: 620, ...overviewTypography.primaryBody }}>
            {activeFilters > 0
              ? "No retained trace matches the current URL filters. Clear all filters to return to the retained trace ledger."
              : "Run an in-process AI analysis to publish private runtime metadata here."}
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

function ErrorTraceSection() {
  return (
    <Box component="section" sx={{ bgcolor: "surface.container", borderBottom: "1px solid", borderColor: "divider" }}>
      <DetailSectionBand title="Trace ledger" metadata="Unavailable" />
      <Box sx={{ minHeight: 150, display: "grid", placeItems: "center", px: 2, py: 3, textAlign: "center" }}>
        <Typography color="textSecondary" sx={overviewTypography.primaryBody}>
          The retained trace ledger could not be loaded.
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
            Analysis traces contain private execution metadata and are available only to authenticated dashboard administrators.
          </Typography>
          <Button type="button" variant="contained" onClick={onSignIn} sx={{ mt: 2.5, minHeight: 44, borderRadius: "4px" }}>
            Sign in to inspect traces
          </Button>
        </Box>
      </Box>
    </Box>
  );
}

export function AnalysisTracesPage() {
  const { features } = useCapabilities();
  const auth = useAuth();
  const manifest = useManifest();
  const [searchParams, setSearchParams] = useSearchParams();
  const query = searchParams.toString();
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
    if (!features.analysis_traces || auth.status !== "authenticated") return;
    const controller = new AbortController();
    fetch(`${API_BASE}api/analysis-traces${query ? `?${query}` : ""}`, {
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
            error: err instanceof Error ? err.message : "Unable to load traces",
          });
        }
      });
    return () => controller.abort();
  }, [auth.status, features.analysis_traces, query]);

  const data = loaded.key === query ? loaded.data : null;
  const error = loaded.key === query ? loaded.error : null;
  const loading = auth.status === "authenticated" && loaded.key !== query;
  const activeFilters = analysisTraceActiveFilterCount(searchParams);

  const totals = useMemo(() => {
    const traces = data?.traces ?? [];
    let modelRequests = 0;
    let toolCalls = 0;
    const responseIDs = new Set<string>();
    for (const trace of traces) {
      for (const event of trace.events) {
        if (event.kind === "model_request") modelRequests++;
        if (event.kind === "tool_call") toolCalls++;
        if (event.response_id) responseIDs.add(event.response_id);
      }
    }
    return {
      traces: traces.length,
      modelRequests,
      toolCalls,
      responseIDs: responseIDs.size,
    };
  }, [data]);

  const ledgerItems = useMemo<AnalysisTraceLedgerItem[]>(() => {
    const prefix = manifest.short_name_prefix ?? "";
    return (data?.traces ?? []).map((trace) => ({
      trace,
      displayTitle: parseTestDisplayName(trace.test_name).displayName,
      displayJob: shortJobName(trace.job_id, prefix),
      testHref: testRunPath(trace.job_id, trace.test_name, trace.build_id),
      responseIDs: analysisTraceResponseIDs(trace),
    }));
  }, [data, manifest.short_name_prefix]);

  function clearFilters() {
    setSearchParams(new URLSearchParams());
  }

  if (!features.analysis_traces) {
    return (
      <AnalysisTracesPageFrame>
        <TraceNotice severity="info" title="Private analysis traces are not available in this deployment." />
      </AnalysisTracesPageFrame>
    );
  }

  if (auth.status === "loading") {
    return (
      <AnalysisTracesPageFrame>
        <Box component="section" aria-busy="true" sx={{ bgcolor: "surface.container", borderBottom: "1px solid", borderColor: "divider" }}>
          <DetailSectionBand title="Private operator access" metadata="Checking session" />
          <Box sx={{ minHeight: 180, display: "grid", placeItems: "center" }}>
            <CircularProgress size={28} aria-label="Checking authentication" />
          </Box>
        </Box>
      </AnalysisTracesPageFrame>
    );
  }

  if (auth.status === "anonymous") {
    return (
      <AnalysisTracesPageFrame>
        <PrivateAccessSection onSignIn={auth.signIn} />
      </AnalysisTracesPageFrame>
    );
  }

  const downloadHref = `${API_BASE}api/analysis-traces/download${query ? `?${query}` : ""}`;
  const metricItems: MetricStripItem[] = [
    { label: "Traces", value: totals.traces },
    { label: "Model requests", value: totals.modelRequests },
    { label: "Tool calls", value: totals.toolCalls },
    { label: "Response IDs", value: totals.responseIDs },
  ];

  return (
    <AnalysisTracesPageFrame
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

      <MetricStrip items={metricItems} label="Trace metrics" />

      {data?.dropped_traces ? (
        <TraceNotice severity="warning" title={`${data.dropped_traces} traces were dropped by the bounded recorder.`}>
          {data.retained_since
            ? `Entries recorded before ${new Date(data.retained_since).toLocaleString()} are outside the retained window. Current retained traces remain available below.`
            : "Current retained traces remain available below."}
        </TraceNotice>
      ) : null}

      {error && (
        <TraceNotice severity="error" title="Failed to load traces">
          {error}
        </TraceNotice>
      )}

      {loading ? (
        <LoadingTraceSection />
      ) : error ? (
        <ErrorTraceSection />
      ) : ledgerItems.length > 0 ? (
        <AnalysisTraceLedger items={ledgerItems} />
      ) : (
        <EmptyTraceSection activeFilters={activeFilters} onClear={clearFilters} />
      )}

      <TracePrivacyDisclosure />
    </AnalysisTracesPageFrame>
  );
}
