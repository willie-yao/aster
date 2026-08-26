import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import CircularProgress from "@mui/material/CircularProgress";
import Typography from "@mui/material/Typography";
import { useCallback, useEffect, useRef, useState } from "react";
import { escalationActive, type EscalationView } from "../lib/escalation";
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
  /** Stable identity of the subject. Changing it reloads the panel. */
  subjectKey: string;
  /** Reads the subject's current escalation state. */
  load: () => Promise<EscalationView>;
  /** Starts one analysis for the subject. */
  start: (idempotencyKey: string) => Promise<EscalationView>;
  /** Trailing note stating what the analysis does and does not establish. */
  disclaimer: string;
  /** Rendered only when the deploy advertises the capability. */
  enabled: boolean;
}

// EscalationPanel offers on-demand analysis for a failure the deterministic
// pass could not explain, then renders the result. It is subject-agnostic: the
// caller supplies the identity and the two calls, so a single pull request
// failure and a failure shared across several share one implementation.
//
// Remounting on subjectKey is what isolates one subject from the next. A panel
// that stays mounted across a subject change would otherwise carry the previous
// subject's result, its in-flight request key, and its disabled button into the
// new one, and an in-flight start would land on the wrong subject.
export function EscalationPanel(props: EscalationPanelProps) {
  return <EscalationPanelForSubject key={props.subjectKey} {...props} />;
}

function EscalationPanelForSubject({
  load,
  start,
  disclaimer,
  enabled,
}: EscalationPanelProps) {
  const [view, setView] = useState<EscalationView | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [starting, setStarting] = useState(false);
  // The key identifies this panel's own request. A response carrying a
  // superseded key is ignored so a late reply cannot overwrite newer state.
  const requestKey = useRef<string | null>(null);
  const cancelled = useRef(false);
  // Starting unmounts the button the operator just activated, so focus moves to
  // the progress region that replaces it. Only a start from this panel claims
  // focus; a page that loads with an analysis already running leaves it alone.
  const claimFocus = useRef(false);
  const startingRef = useRef(false);
  const statusRef = useRef<HTMLDivElement | null>(null);
  const retryRef = useRef<HTMLButtonElement | null>(null);
  // Every read and write takes a generation. A response from an older
  // generation is discarded, so a slow poll cannot overwrite a newer start and
  // regress the panel to not_started.
  const generation = useRef(0);
  // The callers build these inline per render, so holding the latest in a ref
  // keeps the effects keyed on the subject instead of on callback identity. A
  // dependency on the callbacks would refetch on every render.
  const calls = useRef({ load, start });
  useEffect(() => {
    calls.current = { load, start };
  });

  useEffect(() => {
    cancelled.current = false;
    return () => {
      cancelled.current = true;
    };
  }, []);

  const reload = useCallback(async () => {
    const issued = ++generation.current;
    try {
      const next = await calls.current.load();
      if (!cancelled.current && issued === generation.current) setView(next);
    } catch (loadError) {
      if (!cancelled.current && issued === generation.current) {
        setError(errorMessage(loadError));
      }
    }
  }, []);

  useEffect(() => {
    if (!enabled) return;
    void reload();
  }, [enabled, reload]);

  useEffect(() => {
    if (!enabled || !escalationActive(view?.state)) return;
    const timer = setInterval(() => void reload(), pollIntervalMs);
    return () => clearInterval(timer);
  }, [enabled, reload, view?.state]);

  const active = escalationActive(view?.state ?? "not_started");
  // A start that reaches either of these has replaced the button, so the focus
  // it held moves to the status region. Failure leaves the button mounted.
  const engaged = active || view?.state === "complete";

  useEffect(() => {
    if (!engaged || !claimFocus.current) return;
    claimFocus.current = false;
    statusRef.current?.focus();
  }, [engaged]);

  // A run that fails after it started empties the status region the operator is
  // focused on, so focus hands off to the control that retries it.
  useEffect(() => {
    if (view?.state !== "failed") return;
    if (document.activeElement !== statusRef.current) return;
    retryRef.current?.focus();
  }, [view?.state]);

  async function onStart() {
    if (startingRef.current) return;
    startingRef.current = true;
    claimFocus.current = true;
    setStarting(true);
    setError(null);
    const key = newIdempotencyKey();
    requestKey.current = key;
    const issued = ++generation.current;
    try {
      const next = await calls.current.start(key);
      if (!cancelled.current && requestKey.current === key && issued === generation.current) {
        if (next.state === "failed") claimFocus.current = false;
        setView(next);
      }
    } catch (startError) {
      if (!cancelled.current && requestKey.current === key && issued === generation.current) {
        claimFocus.current = false;
        setError(errorMessage(startError));
      }
    } finally {
      startingRef.current = false;
      if (!cancelled.current) setStarting(false);
    }
  }

  if (!enabled) return null;

  const state = view?.state ?? "not_started";
  // Failure states speak through the alert below, so they leave this empty.
  const progressLabel =
    state === "queued"
      ? "Queued behind another analysis"
      : state === "running"
        ? "Investigating"
        : state === "complete"
          ? "Analysis complete"
          : "";

  return (
    <Box
      // Only the pre-state window is busy. Marking the panel busy while running
      // would sit above the status region and defer the very update it carries.
      aria-busy={starting}
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
        <Typography component="span" color="textSecondary" sx={overviewTypography.tableHeading}>
          Deeper analysis
        </Typography>
        {(state === "not_started" || state === "failed") && (
          // aria-disabled rather than disabled: a disabled button drops the
          // operator's focus to the document body mid-action.
          <Button
            ref={retryRef}
            size="small"
            variant="outlined"
            onClick={() => void onStart()}
            aria-disabled={starting || undefined}
            sx={starting ? { opacity: 0.6 } : undefined}
          >
            {starting ? "Starting..." : state === "failed" ? "Retry" : "Investigate"}
          </Button>
        )}
        {/* One region for the whole lifecycle. It never unmounts, so the focus
            it takes when the button disappears survives the analysis finishing,
            and every state change announces from the same place. */}
        <Box
          ref={statusRef}
          role="status"
          tabIndex={-1}
          sx={{
            display: "inline-flex",
            alignItems: "center",
            gap: 0.75,
            "&:focus-visible": { outline: "2px solid", outlineColor: "primary.main", outlineOffset: 2 },
          }}
        >
          {/* The adjacent text names the state, so the spinner is decoration. */}
          {active && <CircularProgress size={14} aria-hidden="true" />}
          {progressLabel && (
            <Typography component="span" color="textSecondary" sx={overviewTypography.description}>
              {progressLabel}
            </Typography>
          )}
        </Box>
      </Box>

      {error && (
        <Typography role="alert" color="error" sx={{ mt: 0.75, ...overviewTypography.description }}>
          {error}
        </Typography>
      )}

      {state === "failed" && (
        <Typography role="alert" color="error" sx={{ mt: 0.75, ...overviewTypography.description }}>
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
            <Typography color="textSecondary" sx={{ mt: 0.75, whiteSpace: "pre-wrap", ...overviewTypography.description }}>
              {view.suggested_fix}
            </Typography>
          )}
          {(view.citations?.length ?? 0) > 0 && (
            <Box sx={{ mt: 1 }}>
              <Typography color="textSecondary" sx={overviewTypography.tableHeading}>
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
          {view.evidence?.build_id && (
            <Typography color="textSecondary" sx={{ mt: 1, ...overviewTypography.description }}>
              Read from build {view.evidence.build_id}
              {view.evidence.pull_number ? ` on pull request #${view.evidence.pull_number}` : ""}.
            </Typography>
          )}
          <Typography color="textSecondary" sx={{ mt: 1, ...overviewTypography.description }}>
            {disclaimer}
          </Typography>
        </Box>
      )}
    </Box>
  );
}
