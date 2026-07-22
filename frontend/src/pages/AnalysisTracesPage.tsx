import { useEffect, useMemo, useState, type FormEvent } from "react";
import ExpandMore from "@mui/icons-material/ExpandMore";
import Download from "@mui/icons-material/Download";
import FilterAlt from "@mui/icons-material/FilterAlt";
import OpenInNew from "@mui/icons-material/OpenInNew";
import Terminal from "@mui/icons-material/Terminal";
import Accordion from "@mui/material/Accordion";
import AccordionDetails from "@mui/material/AccordionDetails";
import AccordionSummary from "@mui/material/AccordionSummary";
import Alert from "@mui/material/Alert";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import Chip from "@mui/material/Chip";
import CircularProgress from "@mui/material/CircularProgress";
import Link from "@mui/material/Link";
import Stack from "@mui/material/Stack";
import TextField from "@mui/material/TextField";
import Typography from "@mui/material/Typography";
import { Link as RouterLink, useSearchParams } from "react-router-dom";
import { useAuth } from "../hooks/useAuth";
import { useCapabilities } from "../hooks/useCapabilities";
import { Panel } from "../components/Panel";
import type {
  AnalysisTrace,
  AnalysisTraceEvent,
  AnalysisTraceFile,
} from "../types/traces";
import { soft } from "../theme";

const API_BASE = import.meta.env.BASE_URL;

function tone(outcome?: string): "success" | "warning" | "error" | "default" {
  const value = outcome?.toLowerCase() ?? "";
  if (/(success|succeeded|passed|hit|completed|revised)/.test(value))
    return "success";
  if (/(retry|objected|truncated|over_budget|uncached)/.test(value))
    return "warning";
  if (/(error|failed|cancelled|unavailable|rejected|exhausted)/.test(value))
    return "error";
  return "default";
}

function formatDuration(ms?: number): string {
  if (ms === undefined) return "";
  if (ms < 1000) return `${ms} ms`;
  return `${(ms / 1000).toFixed(ms < 10_000 ? 2 : 1)} s`;
}

function eventDetails(event: AnalysisTraceEvent): string[] {
  const details: string[] = [];
  if (event.response_id) details.push(`response ${event.response_id}`);
  if (event.tool) details.push(event.tool);
  if (event.status) details.push(`status ${event.status}`);
  if (event.finish_reason) details.push(`finish ${event.finish_reason}`);
  if (event.duration_ms !== undefined)
    details.push(`request ${formatDuration(event.duration_ms)}`);
  if (event.attempts && event.attempts > 1)
    details.push(`${event.attempts} attempts`);
  if (event.http_status) details.push(`HTTP ${event.http_status}`);
  if (event.input_tokens || event.output_tokens)
    details.push(
      `${event.input_tokens ?? 0} in / ${event.output_tokens ?? 0} out`,
    );
  if (event.message_count) details.push(`${event.message_count} messages`);
  if (event.tool_call_count) details.push(`${event.tool_call_count} tool calls`);
  if (event.bytes) details.push(`${event.bytes.toLocaleString()} bytes`);
  if (event.elided) details.push(`${event.elided} elided`);
  if (event.retry) details.push(`retry ${event.retry}`);
  if (event.issue_count)
    details.push(
      `${event.issue_count} issue${event.issue_count === 1 ? "" : "s"}`,
    );
  if (event.error_code) details.push(event.error_code);
  return details;
}

function TraceEventRow({ event }: { event: AnalysisTraceEvent }) {
  const details = eventDetails(event);
  return (
    <Box
      sx={{
        display: "grid",
        gridTemplateColumns: {
          xs: "42px minmax(0, 1fr)",
          md: "42px 74px 160px 120px minmax(0, 1fr)",
        },
        columnGap: 1.5,
        rowGap: 0.4,
        alignItems: "baseline",
        py: 1.1,
        borderBottom: "1px solid",
        borderColor: "divider",
        "&:last-child": { borderBottom: 0 },
      }}
    >
      <Typography
        variant="caption"
        sx={{ fontFamily: "monospace", color: "text.disabled" }}
      >
        {String(event.sequence).padStart(2, "0")}
      </Typography>
      <Typography
        variant="caption"
        sx={{
          fontFamily: "monospace",
          color: "text.secondary",
          display: { xs: "none", md: "block" },
        }}
      >
        +{formatDuration(event.elapsed_ms)}
      </Typography>
      <Typography
        variant="body2"
        sx={{
          fontFamily: "monospace",
          fontWeight: 700,
          overflowWrap: "anywhere",
        }}
      >
        {event.kind}
      </Typography>
      <Box sx={{ display: { xs: "none", md: "block" } }}>
        {event.outcome && (
          <Chip
            size="small"
            color={tone(event.outcome)}
            label={event.outcome}
            variant="outlined"
          />
        )}
      </Box>
      <Typography
        variant="caption"
        color="text.secondary"
        sx={{ gridColumn: { xs: "2", md: "auto" }, overflowWrap: "anywhere" }}
      >
        {details.join(" · ") || "No additional metadata"}
      </Typography>
    </Box>
  );
}

function TraceCard({ trace }: { trace: AnalysisTrace }) {
  const testHref = `/job/${encodeURIComponent(trace.job_id)}/test/${encodeURIComponent(trace.test_name)}?run=${encodeURIComponent(trace.build_id)}`;
  return (
    <Accordion
      disableGutters
      sx={{
        bgcolor: "transparent",
        backgroundImage: "none",
        border: "1px solid",
        borderColor: "divider",
        borderRadius: "10px !important",
        overflow: "hidden",
        "&:before": { display: "none" },
      }}
    >
      <AccordionSummary expandIcon={<ExpandMore />} sx={{ px: 2, py: 0.5 }}>
        <Stack spacing={0.8} sx={{ minWidth: 0, width: "100%", pr: 1 }}>
          <Stack
            direction="row"
            spacing={1}
            sx={{ alignItems: "center", flexWrap: "wrap" }}
          >
            <Chip size="small" label={trace.backend} variant="outlined" />
            <Chip
              size="small"
              label={trace.outcome}
              color={tone(trace.outcome)}
            />
            <Typography
              variant="caption"
              color="text.secondary"
              sx={{ fontFamily: "monospace" }}
            >
              {trace.api_mode}
            </Typography>
            <Typography variant="caption" color="text.disabled">
              {formatDuration(trace.elapsed_ms)} · {trace.events.length} events
            </Typography>
          </Stack>
          <Typography
            variant="body2"
            sx={{ fontWeight: 700, overflowWrap: "anywhere" }}
          >
            {trace.test_name}
          </Typography>
          <Typography
            variant="caption"
            color="text.secondary"
            sx={{ fontFamily: "monospace", overflowWrap: "anywhere" }}
          >
            {trace.job_id} / {trace.build_id}
          </Typography>
          {trace.task_name && (
            <Typography
              variant="caption"
              color="text.disabled"
              sx={{ fontFamily: "monospace", overflowWrap: "anywhere" }}
            >
              Task {trace.task_namespace ? `${trace.task_namespace}/` : ""}
              {trace.task_name}
            </Typography>
          )}
        </Stack>
      </AccordionSummary>
      <AccordionDetails sx={{ px: 2, pt: 0, pb: 2 }}>
        <Stack
          direction="row"
          spacing={1}
          sx={{ mb: 1.5, alignItems: "center", flexWrap: "wrap" }}
        >
          <Link
            component={RouterLink}
            to={testHref}
            underline="hover"
            sx={{ display: "inline-flex", alignItems: "center", gap: 0.5 }}
          >
            Open test run <OpenInNew sx={{ fontSize: 15 }} />
          </Link>
          {trace.error_code && (
            <Chip
              size="small"
              color="error"
              variant="outlined"
              label={trace.error_code}
            />
          )}
          {trace.truncated && (
            <Chip size="small" color="warning" label="Trace truncated" />
          )}
          {trace.contract_hash && (
            <Typography
              variant="caption"
              color="text.disabled"
              sx={{ fontFamily: "monospace", overflowWrap: "anywhere" }}
            >
              contract {trace.contract_hash}
            </Typography>
          )}
        </Stack>
        <Box
          sx={{
            borderRadius: 1.5,
            bgcolor: (theme) => soft(theme, "primary", 0.035),
            px: 1.5,
          }}
        >
          {trace.events.map((event) => (
            <TraceEventRow
              key={`${event.sequence}-${event.kind}`}
              event={event}
            />
          ))}
        </Box>
      </AccordionDetails>
    </Accordion>
  );
}

export function AnalysisTracesPage() {
  const { features } = useCapabilities();
  const auth = useAuth();
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
        if (response.status === 404)
          return {
            version: 1,
            generated_at: "",
            traces: [],
          } as AnalysisTraceFile;
        if (!response.ok) throw new Error(`HTTP ${response.status}`);
        return response.json() as Promise<AnalysisTraceFile>;
      })
      .then((data) => setLoaded({ key: query, data, error: null }))
      .catch((err: unknown) => {
        if (!controller.signal.aborted)
          setLoaded({
            key: query,
            data: null,
            error: err instanceof Error ? err.message : "Unable to load traces",
          });
      });
    return () => controller.abort();
  }, [auth.status, features.analysis_traces, query]);

  const data = loaded.key === query ? loaded.data : null;
  const error = loaded.key === query ? loaded.error : null;
  const loading = auth.status === "authenticated" && loaded.key !== query;

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

  const applyFilters = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const form = new FormData(event.currentTarget);
    const next = new URLSearchParams();
    for (const key of [
      "backend",
      "task_name",
      "job_id",
      "build_id",
      "test_name",
      "response_id",
    ]) {
      const value = String(form.get(key) ?? "").trim();
      if (value) next.set(key, value);
    }
    setSearchParams(next);
  };

  if (!features.analysis_traces) {
    return (
      <Alert severity="info">
        Private analysis traces are not available in this deployment.
      </Alert>
    );
  }
  if (auth.status === "loading") {
    return (
      <Box sx={{ display: "grid", placeItems: "center", py: 10 }}>
        <CircularProgress size={28} />
      </Box>
    );
  }
  if (auth.status === "anonymous") {
    return (
      <Panel sx={{ maxWidth: 620, mx: "auto", p: 4, textAlign: "center" }}>
        <Terminal sx={{ fontSize: 34, color: "primary.main", mb: 1 }} />
        <Typography variant="h5" sx={{ mb: 1 }}>
          Operator sign-in required
        </Typography>
        <Typography color="text.secondary" sx={{ mb: 2.5 }}>
          Analysis traces contain private execution metadata and are restricted
          to dashboard administrators.
        </Typography>
        <Button variant="contained" onClick={auth.signIn}>
          Sign in to inspect traces
        </Button>
      </Panel>
    );
  }

  const downloadHref = `${API_BASE}api/analysis-traces/download${query ? `?${query}` : ""}`;
  return (
    <Stack spacing={2.5}>
      <Stack
        direction={{ xs: "column", sm: "row" }}
        spacing={2}
        sx={{ alignItems: { sm: "center" }, justifyContent: "space-between" }}
      >
        <Box>
          <Stack direction="row" spacing={1} sx={{ alignItems: "center" }}>
            <Terminal color="primary" />
            <Typography variant="h4" component="h1">
              Analysis Traces
            </Typography>
          </Stack>
          <Typography color="text.secondary" sx={{ mt: 0.5 }}>
            Private, content-free execution metadata from in-process and Orka
            analysis backends.
          </Typography>
        </Box>
        <Button
          component="a"
          href={downloadHref}
          startIcon={<Download />}
          variant="outlined"
        >
          Download JSON
        </Button>
      </Stack>

      <Panel key={query} component="form" onSubmit={applyFilters} sx={{ p: 2 }}>
        <Box
          sx={{
            display: "grid",
            gridTemplateColumns: {
              xs: "1fr",
              md: "repeat(2, minmax(0, 1fr))",
              xl: "repeat(3, minmax(0, 1fr))",
            },
            gap: 1.5,
          }}
        >
          {(
            [
              "backend",
              "task_name",
              "job_id",
              "build_id",
              "test_name",
              "response_id",
            ] as const
          ).map((key) => (
              <TextField
                key={key}
                size="small"
                name={key}
                label={
                  {
                    backend: "Backend",
                    task_name: "Task name",
                    job_id: "Job ID",
                    build_id: "Build ID",
                    test_name: "Test name",
                    response_id: "Response ID",
                  }[key]
                }
                defaultValue={searchParams.get(key) ?? ""}
                sx={{ flex: 1, minWidth: 0 }}
              />
            ))}
          <Stack direction="row" spacing={1} sx={{ gridColumn: "1 / -1" }}>
            <Button type="submit" variant="contained" startIcon={<FilterAlt />}>
              Filter
            </Button>
            <Button onClick={() => setSearchParams(new URLSearchParams())}>
              Clear
            </Button>
          </Stack>
        </Box>
      </Panel>

      <Box
        sx={{
          display: "grid",
          gridTemplateColumns: { xs: "repeat(2, 1fr)", md: "repeat(4, 1fr)" },
          gap: 1.5,
        }}
      >
        {[
          ["Traces", totals.traces],
          ["Model requests", totals.modelRequests],
          ["Tool calls", totals.toolCalls],
          ["Response IDs", totals.responseIDs],
        ].map(([label, value]) => (
          <Panel key={label} sx={{ p: 2 }}>
            <Typography variant="caption" color="text.secondary">
              {label}
            </Typography>
            <Typography variant="h5" sx={{ mt: 0.4, fontFamily: "monospace" }}>
              {value}
            </Typography>
          </Panel>
        ))}
      </Box>

      {data?.dropped_traces ? (
        <Alert severity="warning">
          {data.dropped_traces} traces were dropped by the bounded recorder.
          {data.retained_since
            ? ` Entries recorded before ${new Date(data.retained_since).toLocaleString()} are outside the retained window.`
            : ""}
        </Alert>
      ) : null}
      {error && <Alert severity="error">Failed to load traces: {error}</Alert>}
      {loading ? (
        <Box sx={{ display: "grid", placeItems: "center", py: 8 }}>
          <CircularProgress size={28} />
        </Box>
      ) : data && data.traces.length > 0 ? (
        <Stack spacing={1.25}>
          {data.traces.map((trace, index) => (
            <TraceCard
              key={`${trace.job_id}-${trace.build_id}-${trace.test_name}-${index}`}
              trace={trace}
            />
          ))}
        </Stack>
      ) : (
        <Panel sx={{ p: 5, textAlign: "center" }}>
          <Terminal sx={{ color: "text.disabled", fontSize: 34, mb: 1 }} />
          <Typography variant="h6">No matching traces</Typography>
          <Typography color="text.secondary" sx={{ mt: 0.5 }}>
            Run an in-process AI analysis or clear the current filters.
          </Typography>
        </Panel>
      )}
    </Stack>
  );
}
