import { useCallback, useEffect, useRef, useState } from "react";
import ChevronRight from "@mui/icons-material/ChevronRight";
import Box from "@mui/material/Box";
import ButtonBase from "@mui/material/ButtonBase";
import CircularProgress from "@mui/material/CircularProgress";
import Collapse from "@mui/material/Collapse";
import Typography from "@mui/material/Typography";
import { useAuth } from "../hooks/useAuth";
import { useCapabilities } from "../hooks/useCapabilities";
import { analysisHealthVerdict } from "../lib/analysisHealth";
import { analysisTraceResponseIDs, formatTraceDuration } from "../lib/analysisTraces";
import { overviewTypography } from "../theme/overview";
import type { AnalysisTrace, AnalysisTraceFile } from "../types/traces";
import { TraceDetailBody, TraceHealthSignal, TraceReasons } from "./AnalysisTraceLedger";

const API_BASE = import.meta.env.BASE_URL;

export interface AnalysisTraceReference {
  job_id: string;
  build_id: string;
  test_name: string;
}

type LoadState =
  | { status: "idle" }
  | { status: "loading" }
  | { status: "loaded"; traces: AnalysisTrace[] }
  | { status: "error"; message: string };

/** Orders repeated analyses of the same failure most recent first. */
function newestFirst(traces: AnalysisTrace[]): AnalysisTrace[] {
  const at = (trace: AnalysisTrace) => {
    const parsed = Date.parse(trace.recorded_at ?? trace.started_at);
    return Number.isNaN(parsed) ? Number.NEGATIVE_INFINITY : parsed;
  };
  return [...traces].sort((a, b) => at(b) - at(a));
}

/**
 * Inline runtime inspector for one analysis. Loads the retained traces for a
 * single job, build, and test on first expand so an operator can see why an
 * analysis came out the way it did without leaving the test page.
 */
export function AnalysisTraceInspector({ reference }: { reference: AnalysisTraceReference }) {
  const { features } = useCapabilities();
  const auth = useAuth();
  const [open, setOpen] = useState(false);
  const [state, setState] = useState<LoadState>({ status: "idle" });
  const controllerRef = useRef<AbortController | null>(null);
  const enabled = Boolean(features.analysis_health) && auth.status === "authenticated";

  useEffect(() => () => controllerRef.current?.abort(), []);

  const load = useCallback(() => {
    const controller = new AbortController();
    controllerRef.current?.abort();
    controllerRef.current = controller;
    const query = new URLSearchParams({
      job_id: reference.job_id,
      build_id: reference.build_id,
      test_name: reference.test_name,
    }).toString();
    setState({ status: "loading" });
    fetch(`${API_BASE}api/analysis-health?${query}`, {
      credentials: "same-origin",
      signal: controller.signal,
    })
      .then(async (response) => {
        if (response.status === 404) return { traces: [] } as unknown as AnalysisTraceFile;
        if (!response.ok) throw new Error(`HTTP ${response.status}`);
        return response.json() as Promise<AnalysisTraceFile>;
      })
      .then((data) => setState({ status: "loaded", traces: data.traces ?? [] }))
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        setState({
          status: "error",
          message: err instanceof Error ? err.message : "Unable to load runtime detail",
        });
      });
  }, [reference.job_id, reference.build_id, reference.test_name]);

  function toggle() {
    setOpen((wasOpen) => {
      if (!wasOpen && state.status !== "loaded") load();
      return !wasOpen;
    });
  }

  if (!enabled) return null;

  return (
    <Box component="section" sx={{ mt: 1.5, border: "1px solid", borderColor: "divider", borderRadius: "4px", overflow: "hidden" }}>
      <ButtonBase
        type="button"
        onClick={toggle}
        aria-expanded={open}
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
          "&.Mui-focusVisible": { outline: "2px solid", outlineColor: "primary.main", outlineOffset: -2 },
        }}
      >
        <Typography component="span" sx={{ ...overviewTypography.secondaryBody, fontWeight: 700 }}>
          Analysis runtime detail
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

      <Collapse in={open} timeout="auto" unmountOnExit>
        <Box sx={{ borderTop: "1px solid", borderColor: "divider" }}>
          {state.status !== "loaded" && state.status !== "error" && (
            <Box aria-busy="true" sx={{ minHeight: 120, display: "grid", placeItems: "center" }}>
              <CircularProgress size={24} aria-label="Loading runtime detail" />
            </Box>
          )}
          {state.status === "error" && (
            <Typography color="textSecondary" role="alert" sx={{ px: 1.5, py: 2, ...overviewTypography.secondaryBody }}>
              Runtime detail could not be loaded: {state.message}
            </Typography>
          )}
          {state.status === "loaded" && state.traces.length === 0 && (
            <Typography color="textSecondary" sx={{ px: 1.5, py: 2, ...overviewTypography.secondaryBody }}>
              No retained runtime detail. This analysis was served from cache or has rolled out of the retained window.
            </Typography>
          )}
          {state.status === "loaded" &&
            newestFirst(state.traces).map((trace) => {
              const verdict = analysisHealthVerdict(trace);
              const recorded = trace.recorded_at ?? trace.started_at;
              const recordedLabel = Number.isNaN(Date.parse(recorded))
                ? recorded
                : new Date(recorded).toLocaleString();
              return (
                <Box key={`${trace.started_at}-${trace.build_id}`} sx={{ borderBottom: "1px solid", borderColor: "divider", "&:last-of-type": { borderBottom: 0 } }}>
                  <Box sx={{ display: "flex", alignItems: "center", gap: 1.5, flexWrap: "wrap", px: 1.5, py: 1.25 }}>
                    <TraceHealthSignal severity={verdict.severity} />
                    <Typography color="textSecondary" sx={overviewTypography.data}>
                      {recordedLabel} · {formatTraceDuration(trace.elapsed_ms)} · {verdict.modelRequests} model{" "}
                      {verdict.modelRequests === 1 ? "request" : "requests"} · {verdict.toolCalls} tool{" "}
                      {verdict.toolCalls === 1 ? "call" : "calls"}
                      {verdict.toolErrors > 0 ? ` (${verdict.toolErrors} failed)` : ""} ·{" "}
                      {verdict.evidenceBytes.toLocaleString()} bytes read
                    </Typography>
                  </Box>
                  <Box sx={{ px: 1.5, pb: 1.25 }}>
                    <TraceReasons reasons={verdict.reasons} />
                  </Box>
                  <TraceDetailBody trace={trace} responseIDs={analysisTraceResponseIDs(trace)} />
                </Box>
              );
            })}
        </Box>
      </Collapse>
    </Box>
  );
}
