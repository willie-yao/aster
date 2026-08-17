import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import CircularProgress from "@mui/material/CircularProgress";
import Typography from "@mui/material/Typography";
import { useCallback, useEffect, useRef, useState } from "react";
import {
  escalationActive,
  getEscalation,
  startEscalation,
  type EscalationRef,
  type EscalationView,
} from "../lib/pullRequestEscalation";
import { soft } from "../theme";
import { overviewTypography } from "../theme/overview";

const pollIntervalMs = 2000;

function newIdempotencyKey(): string {
  if (typeof crypto !== "undefined" && "randomUUID" in crypto) {
    return crypto.randomUUID();
  }
  return `${Date.now()}-${Math.random().toString(16).slice(2)}`;
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

interface EscalationPanelProps {
  refValue: EscalationRef;
  /** Rendered only when the deploy advertises the capability. */
  enabled: boolean;
}

// EscalationPanel offers on-demand analysis for a failure the deterministic
// pass could not explain, then renders the result.
export function EscalationPanel({ refValue, enabled }: EscalationPanelProps) {
  const [view, setView] = useState<EscalationView | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [starting, setStarting] = useState(false);
  // The key identifies this panel's own request. A response carrying a
  // superseded key is ignored so a late reply cannot overwrite newer state.
  const requestKey = useRef<string | null>(null);
  const cancelled = useRef(false);

  useEffect(() => {
    cancelled.current = false;
    return () => {
      cancelled.current = true;
    };
  }, []);

  const load = useCallback(async () => {
    try {
      const next = await getEscalation(refValue);
      if (!cancelled.current) setView(next);
    } catch (loadError) {
      if (!cancelled.current) setError(errorMessage(loadError));
    }
  }, [refValue]);

  useEffect(() => {
    if (!enabled) return;
    void load();
  }, [enabled, load]);

  useEffect(() => {
    if (!enabled || !escalationActive(view?.state)) return;
    const timer = setInterval(() => void load(), pollIntervalMs);
    return () => clearInterval(timer);
  }, [enabled, load, view?.state]);

  async function onStart() {
    setStarting(true);
    setError(null);
    const key = newIdempotencyKey();
    requestKey.current = key;
    try {
      const next = await startEscalation(refValue, key);
      if (!cancelled.current && requestKey.current === key) setView(next);
    } catch (startError) {
      if (!cancelled.current && requestKey.current === key) {
        setError(errorMessage(startError));
      }
    } finally {
      if (!cancelled.current) setStarting(false);
    }
  }

  if (!enabled) return null;

  const state = view?.state ?? "not_started";
  const active = escalationActive(state);

  return (
    <Box
      sx={{
        mt: 1,
        p: 1.25,
        borderRadius: "4px",
        border: "1px solid",
        borderColor: "divider",
        bgcolor: "surface.containerHigh",
      }}
    >
      <Box sx={{ display: "flex", alignItems: "center", gap: 1, flexWrap: "wrap" }}>
        <Typography component="span" color="text.secondary" sx={overviewTypography.tableHeading}>
          Deeper analysis
        </Typography>
        {state === "not_started" && (
          <Button size="small" variant="outlined" onClick={() => void onStart()} disabled={starting}>
            {starting ? "Starting..." : "Investigate"}
          </Button>
        )}
        {active && (
          <Box sx={{ display: "inline-flex", alignItems: "center", gap: 0.75 }}>
            <CircularProgress size={14} />
            <Typography component="span" color="text.secondary" sx={overviewTypography.description}>
              {state === "queued" ? "Queued behind another analysis" : "Investigating"}
            </Typography>
          </Box>
        )}
      </Box>

      {error && (
        <Typography color="error.main" sx={{ mt: 0.75, ...overviewTypography.description }}>
          {error}
        </Typography>
      )}

      {state === "failed" && (
        <Typography color="error.main" sx={{ mt: 0.75, ...overviewTypography.description }}>
          {view?.error || "The analysis could not complete."}
        </Typography>
      )}

      {state === "complete" && view && (
        <Box sx={{ mt: 1 }}>
          {view.root_cause && (
            <Typography sx={{ whiteSpace: "pre-wrap", ...overviewTypography.secondaryBody }}>
              {view.root_cause}
            </Typography>
          )}
          {view.suggested_fix && (
            <Typography color="text.secondary" sx={{ mt: 0.75, whiteSpace: "pre-wrap", ...overviewTypography.description }}>
              {view.suggested_fix}
            </Typography>
          )}
          {(view.citations?.length ?? 0) > 0 && (
            <Box sx={{ mt: 1 }}>
              <Typography color="text.secondary" sx={overviewTypography.tableHeading}>
                Evidence
              </Typography>
              {view.citations?.map((citation, index) => (
                <Box
                  key={`${citation.path}-${index}`}
                  sx={{
                    mt: 0.5,
                    p: 1,
                    borderRadius: "4px",
                    bgcolor: (theme) => soft(theme, "primary", 0.06),
                  }}
                >
                  <Typography sx={overviewTypography.data}>
                    {citation.path}
                    {citation.line_start ? `:${citation.line_start}` : ""}
                  </Typography>
                  {citation.quote && (
                    <Typography
                      component="pre"
                      sx={{
                        m: 0,
                        mt: 0.25,
                        fontFamily: "monospace",
                        fontSize: "0.75rem",
                        whiteSpace: "pre-wrap",
                        color: "text.secondary",
                      }}
                    >
                      {citation.quote}
                    </Typography>
                  )}
                </Box>
              ))}
            </Box>
          )}
          <Typography color="text.secondary" sx={{ mt: 1, ...overviewTypography.description }}>
            This analysis explains the failure from build artifacts. It does not
            establish that the pull request caused it.
          </Typography>
        </Box>
      )}
    </Box>
  );
}
