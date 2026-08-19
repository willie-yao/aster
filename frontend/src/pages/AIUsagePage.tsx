import { useEffect, useMemo, useState, type ReactNode } from "react";
import Download from "@mui/icons-material/Download";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import CircularProgress from "@mui/material/CircularProgress";
import Typography from "@mui/material/Typography";
import { useSearchParams } from "react-router-dom";
import { AIUsageCoverage } from "../components/AIUsageCoverage";
import { AIUsageDailySections } from "../components/AIUsageDaily";
import { AIUsageFilters } from "../components/AIUsageFilters";
import { DetailSectionBand } from "../components/DetailSectionBand";
import { MetricStrip, type MetricStripItem } from "../components/MetricStrip";
import { TraceNotice } from "../components/AnalysisTraceLedger";
import { useAuth } from "../hooks/useAuth";
import { useCapabilities } from "../hooks/useCapabilities";
import {
  aiUsageFilterParams,
  aiUsageFiltersAreCustom,
  aiUsageFiltersFromParams,
  defaultAIUsageFilters,
  formatCoverage,
  formatCurrentRateReprice,
  formatRecordedUsageEstimate,
  formatTokens,
  normalizeAIUsageDay,
  pricedRequestCoverageNote,
  totalTokens,
  usageQuery,
} from "../lib/aiUsage";
import { overviewLayout, overviewTypography } from "../theme/overview";
import type { AIUsageReport, AIUsageTotals } from "../types/usage";

const API_BASE = import.meta.env.BASE_URL;
const zeroTotals: AIUsageTotals = {
  operations: 0,
  cache_hits: 0,
  failures: 0,
  external_unmetered_operations: 0,
  model_gateway_excluded_operations: 0,
  model_requests: 0,
  reported_requests: 0,
  priced_reported_requests: 0,
  cache_write_reported_requests: 0,
  cache_write_priced_requests: 0,
  cache_write_unreported_requests: 0,
  invalid_usage_requests: 0,
  unreported_requests: 0,
  input_tokens: 0,
  cached_input_tokens: 0,
  cache_write_input_tokens: 0,
  output_tokens: 0,
  reasoning_tokens: 0,
  estimated_cost_nanos: "0",
};

function AIUsagePageFrame({ children, action }: { children: ReactNode; action?: ReactNode }) {
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
        <Typography component="h1" sx={{ gridArea: "title", ...overviewTypography.pageHeadline }}>AI Usage</Typography>
        {action && <Box sx={{ gridArea: "action", justifySelf: { xs: "start", sm: "end" }, alignSelf: "end" }}>{action}</Box>}
      </Box>
      {children}
    </Box>
  );
}

function LoadingSection() {
  return (
    <Box component="section" aria-busy="true" aria-label="Loading AI usage" sx={{ bgcolor: "surface.container", borderBottom: "1px solid", borderColor: "divider" }}>
      <DetailSectionBand title="Usage ledger" metadata="Loading" />
      <Box sx={{ minHeight: 180, display: "grid", placeItems: "center" }}>
        <CircularProgress size={28} aria-label="Loading AI usage" />
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
          <Typography component="h2" sx={{ ...overviewTypography.majorHeading, fontSize: "20px" }}>Operator sign-in required</Typography>
          <Typography color="textSecondary" sx={{ mt: 0.75, maxWidth: 620, ...overviewTypography.primaryBody }}>
            Provider usage and estimate data is private operational accounting information available only to authenticated dashboard administrators.
          </Typography>
          <Button type="button" variant="contained" onClick={onSignIn} sx={{ mt: 2.5, minHeight: 44, borderRadius: "4px" }}>
            Sign in to view usage
          </Button>
        </Box>
      </Box>
    </Box>
  );
}

function UnavailableSection({ title, message }: { title: string; message: string }) {
  return (
    <Box component="section" sx={{ bgcolor: "surface.container", borderBottom: "1px solid", borderColor: "divider" }}>
      <DetailSectionBand title="Usage ledger" metadata="Unavailable" />
      <Box sx={{ minHeight: 180, display: "grid", placeItems: "center", px: 2, py: 4, textAlign: "center" }}>
        <Box>
          <Typography component="h2" sx={{ ...overviewTypography.majorHeading, fontSize: "20px" }}>{title}</Typography>
          <Typography color="textSecondary" sx={{ mt: 0.75, maxWidth: 620, ...overviewTypography.primaryBody }}>{message}</Typography>
        </Box>
      </Box>
    </Box>
  );
}

export function AIUsagePage() {
  const { features } = useCapabilities();
  const auth = useAuth();
  const [searchParams, setSearchParams] = useSearchParams();
  const defaults = useMemo(() => defaultAIUsageFilters(), []);
  const filterValues = aiUsageFiltersFromParams(searchParams, defaults);
  const customFilters = aiUsageFiltersAreCustom(searchParams);
  const query = usageQuery(filterValues.start, filterValues.end, filterValues.feature || undefined);
  const [loaded, setLoaded] = useState<{ key: string | null; data: AIUsageReport | null; error: string }>({
    key: null,
    data: null,
    error: "",
  });

  useEffect(() => {
    if (!features.ai_usage || auth.status !== "authenticated") return;
    const controller = new AbortController();
    fetch(`${API_BASE}api/ai-usage?${query}`, { credentials: "same-origin", signal: controller.signal })
      .then(async (response) => {
        if (response.status === 404) return null;
        if (!response.ok) throw new Error(`HTTP ${response.status}`);
        return response.json() as Promise<AIUsageReport>;
      })
      .then((data) => setLoaded({ key: query, data, error: "" }))
      .catch((error: unknown) => {
        if (!controller.signal.aborted) {
          setLoaded({ key: query, data: null, error: error instanceof Error ? error.message : "Unable to load usage" });
        }
      });
    return () => controller.abort();
  }, [auth.status, features.ai_usage, query]);

  const data = loaded.key === query ? loaded.data : null;
  const error = loaded.key === query ? loaded.error : "";
  const loading = auth.status === "authenticated" && loaded.key !== query;
  const totals = data?.totals ?? zeroTotals;
  const days = useMemo(() => (data?.daily ?? []).map(normalizeAIUsageDay), [data]);
  const partialDay = days.find((day) => day.current_partial_utc)?.date;

  if (!features.ai_usage) {
    return (
      <AIUsagePageFrame>
        <TraceNotice severity="info" title="Private AI usage reporting is not available in this deployment." />
      </AIUsagePageFrame>
    );
  }

  if (auth.status === "loading") {
    return (
      <AIUsagePageFrame>
        <Box component="section" aria-busy="true" sx={{ bgcolor: "surface.container", borderBottom: "1px solid", borderColor: "divider" }}>
          <DetailSectionBand title="Private operator access" metadata="Checking session" />
          <Box sx={{ minHeight: 180, display: "grid", placeItems: "center" }}>
            <CircularProgress size={28} aria-label="Checking authentication" />
          </Box>
        </Box>
      </AIUsagePageFrame>
    );
  }

  if (auth.status === "anonymous") {
    return <AIUsagePageFrame><PrivateAccessSection onSignIn={auth.signIn} /></AIUsagePageFrame>;
  }

  const downloadHref = `${API_BASE}api/ai-usage/download?${query}`;
  const pricedReportedRequests = totals.priced_reported_requests ?? data?.coverage.priced_reported_requests;
  const metricItems: MetricStripItem[] = data ? [
    {
      label: "Recorded estimate",
      value: formatRecordedUsageEstimate({
        status: data.recorded_cost_status,
        mixedCurrency: data.mixed_currency,
        nanos: totals.estimated_cost_nanos,
        currency: data.currency,
      }),
      note: data.mixed_currency
        ? "Recorded totals cannot be combined across currencies"
        : data.recorded_cost_status === "unavailable"
          ? "No recorded priced estimate is available"
          : data.recorded_cost_status === "unknown"
            ? "Recorded estimate status is unknown"
            : `Stored per-operation prices · ${pricedRequestCoverageNote(pricedReportedRequests, totals.reported_requests, data.pricing_coverage)}`,
    },
    {
      label: "Current-rate reprice",
      value: formatCurrentRateReprice({
        status: data.current_rate_status,
        nanos: data.current_rate_estimated_cost_nanos,
        currency: data.current_rate_currency,
      }),
      note: data.current_rate_status === "unavailable" || data.current_rate_estimated_cost_nanos === undefined || !data.current_rate_currency
        ? "Current configured-rate repricing is unavailable"
        : data.current_rate_status === "partial"
          ? "Known historical reported tokens at current rates · partial coverage · not actual spend"
          : "Historical reported tokens at rates configured now · not actual spend",
    },
    {
      label: "Provider-reported tokens",
      value: formatTokens(totalTokens(totals)),
      note: `${formatCoverage(totals.reported_requests, totals.model_requests)} model requests reported usage`,
    },
    {
      label: "Model requests",
      value: formatTokens(totals.model_requests),
      note: `${totals.cache_hits.toLocaleString()} cache hits · ${totals.unreported_requests.toLocaleString()} without usage`,
    },
  ] : [];

  return (
    <AIUsagePageFrame
      action={
        <Button component="a" href={downloadHref} startIcon={<Download />} variant="outlined" sx={{ minHeight: { xs: 44, sm: 40 }, borderRadius: "4px" }}>
          Download JSON
        </Button>
      }
    >
      <AIUsageFilters
        key={query}
        values={filterValues}
        custom={customFilters}
        onApply={(values) => setSearchParams(aiUsageFilterParams(values))}
        onReset={() => setSearchParams(new URLSearchParams())}
      />

      {error && (
        <TraceNotice severity="error" title="Unable to load AI usage">{error}</TraceNotice>
      )}

      {loading ? (
        <LoadingSection />
      ) : error ? (
        <UnavailableSection title="Usage data unavailable" message="No accounting conclusions are shown while the private usage API request is in error." />
      ) : !data ? (
        <UnavailableSection title="No AI usage recorded" message="No private provider usage records are available for the selected range." />
      ) : (
        <>
          <MetricStrip items={metricItems} label="AI usage metrics" />
          <AIUsageDailySections
            data={data}
            days={days}
            coverageSection={<AIUsageCoverage data={data} partialDay={partialDay} />}
          />
        </>
      )}
    </AIUsagePageFrame>
  );
}
