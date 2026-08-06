import CheckCircleOutlined from "@mui/icons-material/CheckCircleOutlined";
import Close from "@mui/icons-material/Close";
import ErrorOutlineOutlined from "@mui/icons-material/ErrorOutlineOutlined";
import ExpandMore from "@mui/icons-material/ExpandMore";
import Schedule from "@mui/icons-material/Schedule";
import WarningAmber from "@mui/icons-material/WarningAmber";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import CircularProgress from "@mui/material/CircularProgress";
import Collapse from "@mui/material/Collapse";
import Container from "@mui/material/Container";
import Divider from "@mui/material/Divider";
import IconButton from "@mui/material/IconButton";
import LinearProgress from "@mui/material/LinearProgress";
import Popover from "@mui/material/Popover";
import Stack from "@mui/material/Stack";
import Tooltip from "@mui/material/Tooltip";
import Typography from "@mui/material/Typography";
import useMediaQuery from "@mui/material/useMediaQuery";
import { useId, useLayoutEffect, useRef, useState, type ReactNode } from "react";
import {
  analysisProgressBreakdown,
  fetchStatusCompactPresentation,
  fetchStatusHasCompletedPipeline,
  fetchStatusMacroStages,
  fetchStatusPresentation,
  fetchStatusStripKey,
  formatFetchRelativeTime,
  formatFetchTimestamp,
  patternFailureLabel,
  shouldShowFetchStatusStrip,
  type FetchMacroStagePresentation,
  type FetchMacroStageState,
} from "../lib/fetchStatus";
import type { FetchPassSummary, FetchProgressStatus, FetchStatusResponse } from "../types/fetchStatus";
import { soft } from "../theme";

interface FetchStatusControlProps {
  response: FetchStatusResponse | null;
}

interface FetchStatusStripProps {
  response: FetchStatusResponse | null;
  dismissedKey: string | null;
  onDismiss: (key: string) => void;
}

const activeSpinnerDuration = 1400;

const visuallyHidden = {
  border: 0,
  clip: "rect(0 0 0 0)",
  height: "1px",
  m: "-1px",
  overflow: "hidden",
  p: 0,
  position: "absolute",
  whiteSpace: "nowrap",
  width: "1px",
} as const;

export function FetchActivityIcon({ size }: { size: number }) {
  const spinnerRef = useRef<HTMLSpanElement>(null);

  useLayoutEffect(() => {
    const spinner = spinnerRef.current;
    if (!spinner) return;

    const reducedMotion = window.matchMedia("(prefers-reduced-motion: reduce)");
    let animation: Animation | null = null;
    const updateAnimation = () => {
      animation?.cancel();
      animation = null;
      spinner.style.transform = "rotate(0deg)";
      if (reducedMotion.matches) return;

      animation = spinner.animate(
        [{ transform: "rotate(0deg)" }, { transform: "rotate(360deg)" }],
        { duration: activeSpinnerDuration, iterations: Infinity, easing: "linear" },
      );
      animation.startTime = 0;
    };

    updateAnimation();
    reducedMotion.addEventListener("change", updateAnimation);
    return () => {
      reducedMotion.removeEventListener("change", updateAnimation);
      animation?.cancel();
    };
  }, []);

  return (
    <Box
      ref={spinnerRef}
      component="span"
      aria-hidden="true"
      sx={{
        display: "inline-flex",
        width: size,
        height: size,
        flex: "0 0 auto",
        transform: "rotate(0deg)",
      }}
    >
      <CircularProgress
        aria-hidden="true"
        role="presentation"
        variant="determinate"
        value={30}
        size={size}
        thickness={5}
        color="inherit"
        sx={{
          display: "block",
          "& .MuiCircularProgress-circle": {
            strokeLinecap: "round",
            transition: "none",
          },
        }}
      />
    </Box>
  );
}

function stateIcon(response: FetchStatusResponse, size = 18): ReactNode {
  switch (response.state) {
    case "active":
      return <FetchActivityIcon size={size} />;
    case "failed":
      return <ErrorOutlineOutlined sx={{ fontSize: size }} />;
    case "stale":
    case "interrupted":
    case "cancelled":
      return <WarningAmber sx={{ fontSize: size }} />;
    case "idle":
    case "completed":
      return <CheckCircleOutlined sx={{ fontSize: size }} />;
    default:
      return <Schedule sx={{ fontSize: size }} />;
  }
}

function DetailRow({ label, value }: { label: string; value: ReactNode }) {
  return (
    <Box sx={{ display: "grid", gridTemplateColumns: "minmax(110px, auto) minmax(0, 1fr)", gap: 2 }}>
      <Typography variant="caption" color="text.secondary">
        {label}
      </Typography>
      <Typography variant="body2" sx={{ textAlign: "right", overflowWrap: "anywhere" }}>
        {value}
      </Typography>
    </Box>
  );
}

function TimestampValue({ value }: { value?: string }) {
  const relative = formatFetchRelativeTime(value);
  const exact = formatFetchTimestamp(value);
  if (!value || relative === "Unknown") return <>{relative}</>;
  return (
    <Tooltip title={exact} arrow>
      <Box
        component="time"
        dateTime={value}
        tabIndex={0}
        aria-label={`${relative}. ${exact}`}
        sx={{
          cursor: "help",
          borderRadius: 0.5,
          outline: "none",
          "&:focus-visible": {
            outline: "2px solid",
            outlineColor: "primary.main",
            outlineOffset: 2,
          },
        }}
      >
        {relative}
      </Box>
    </Tooltip>
  );
}

function formatPassDuration(durationMS: number): string {
  const seconds = Math.max(0, Math.round(durationMS / 1000));
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  const remainder = seconds % 60;
  return remainder > 0 ? `${minutes}m ${remainder}s` : `${minutes}m`;
}

function passTypeLabel(passType: FetchPassSummary["pass_type"]): string {
  switch (passType) {
    case "one-shot":
      return "One-shot";
    case "initial-watch":
      return "Initial watch";
    case "lightweight-watch":
      return "Watch";
    case "reconcile":
      return "Reconcile";
  }
}

function recentPassDetail(pass: FetchPassSummary): string {
  const published = pass.published ? "published" : "not published";
  const cohort = (pass.potential_tasks_saved ?? 0) > 0 ? ` · ${pass.potential_tasks_saved} potential dedupe` : "";
  const shared = (pass.same_failure_results_reused ?? 0) > 0 ? ` · ${pass.same_failure_results_reused} same failure` : "";
  return `${pass.outcome} · ${formatPassDuration(pass.duration_ms)} · ${pass.cache_hits} cache · ${pass.compatible_results_reused} compatible · ${pass.exact_results_reused} exact${shared} · ${pass.new_tasks_created} new Tasks${cohort} · ${published}`;
}

function cacheRejectionDetail(status: FetchProgressStatus): string {
  const rejections = status.analyses.cache_rejections;
  if (!rejections) return "None recorded";
  const entries: Array<[string, number]> = [
    ["missing", rejections.missing],
    ["expired", rejections.expired],
    ["tool floor", rejections.tool_floor],
    ["evidence floor", rejections.evidence_floor],
    ["critique", rejections.critique],
    ["malformed", rejections.malformed],
  ];
  const details = entries
    .filter(([, count]) => count > 0)
    .map(([label, count]) => `${count} ${label}`);
  return details.length > 0 ? details.join(" · ") : "None";
}

function phaseDurationDetail(status: FetchProgressStatus): string {
  const durations = Object.entries(status.phase_durations_ms ?? {});
  if (durations.length === 0) return "Not recorded";
  return durations
    .map(([phase, duration]) => `${phase.replaceAll("-", " ")} ${formatPassDuration(duration)}`)
    .join(" · ");
}

function stageColor(state: FetchMacroStageState): string {
  switch (state) {
    case "complete":
      return "success.main";
    case "active":
      return "info.main";
    case "failed":
      return "error.main";
    case "stale":
    case "interrupted":
    case "cancelled":
      return "warning.main";
    default:
      return "text.disabled";
  }
}

function stageIcon(stage: FetchMacroStagePresentation): ReactNode {
  switch (stage.state) {
    case "complete":
      return <CheckCircleOutlined sx={{ fontSize: 17 }} />;
    case "active":
      return <FetchActivityIcon size={17} />;
    case "failed":
      return <ErrorOutlineOutlined sx={{ fontSize: 17 }} />;
    case "stale":
    case "interrupted":
    case "cancelled":
      return <WarningAmber sx={{ fontSize: 17 }} />;
    default:
      return <Schedule sx={{ fontSize: 17 }} />;
  }
}

function MacroStageProgress({ stages }: { stages: FetchMacroStagePresentation[] }) {
  return (
    <Box
      component="ol"
      aria-label="Refresh stages"
      sx={{
        display: "grid",
        gridTemplateColumns: "repeat(4, minmax(0, 1fr))",
        gap: 0.5,
        m: 0,
        p: 0.75,
        listStyle: "none",
        border: "1px solid",
        borderColor: "divider",
        borderRadius: 1,
        bgcolor: "surface.containerHigh",
      }}
    >
      {stages.map((stage) => (
        <Box
          component="li"
          key={stage.id}
          aria-label={`${stage.label}: ${stage.stateLabel}`}
          sx={{ minWidth: 0, px: 0.25, py: 0.5, textAlign: "center" }}
        >
          <Box aria-hidden="true" sx={{ color: stageColor(stage.state), display: "flex", justifyContent: "center", mb: 0.375 }}>
            {stageIcon(stage)}
          </Box>
          <Typography variant="caption" sx={{ display: "block", color: "text.primary", fontWeight: 700, lineHeight: 1.2 }}>
            {stage.label}
          </Typography>
          <Typography variant="caption" color="text.secondary" sx={{ display: "block", fontSize: "0.6875rem", lineHeight: 1.2, mt: 0.25 }}>
            {stage.stateLabel}
          </Typography>
        </Box>
      ))}
    </Box>
  );
}

function statusWarnings(status: FetchProgressStatus): string[] {
  const warnings: string[] = [];
  if (status.analyses.failed > 0) {
    warnings.push(`${status.analyses.failed} ${status.analyses.failed === 1 ? "analysis" : "analyses"} failed`);
  }
  if (status.analyses.cancelled > 0) {
    warnings.push(`${status.analyses.cancelled} ${status.analyses.cancelled === 1 ? "analysis was" : "analyses were"} cancelled`);
  }
  if ((status.patterns?.failed ?? 0) > 0) {
    warnings.push(`${status.patterns?.failed} pattern ${status.patterns?.failed === 1 ? "attempt" : "attempts"} failed`);
  }
  if ((status.patterns?.repair_failed ?? 0) > 0) {
    warnings.push(`${status.patterns?.repair_failed} pattern ${status.patterns?.repair_failed === 1 ? "repair" : "repairs"} failed`);
  }
  if (status.patterns?.repair_failure_category) {
    warnings.push(`Pattern repair reported ${patternFailureLabel(status.patterns.repair_failure_category)}`);
  }
  if (status.patterns?.failure_category) {
    warnings.push(`Pattern processing reported ${patternFailureLabel(status.patterns.failure_category)}`);
  }
  if (status.pattern_phase === "failed" && !status.patterns?.failure_category) warnings.push("Pattern processing failed");
  if (status.publication_phase === "failed") warnings.push("Dashboard publication failed");
  if (status.side_effect_phase === "failed") warnings.push("Refresh follow-up work failed");
  return [...new Set(warnings)];
}

export function FetchStatusControl({ response }: FetchStatusControlProps) {
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  const [technicalOpen, setTechnicalOpen] = useState(false);
  const technicalID = useId();
  const reduceMotion = useMediaQuery("(prefers-reduced-motion: reduce)");
  const compact = response ? fetchStatusCompactPresentation(response) : null;
  const presentation = response ? fetchStatusPresentation(response) : null;
  const status = response?.status;
  if (!response || !compact || !presentation || !status) return null;

  const popoverID = anchor ? "fetch-status-details" : undefined;
  const analysis = analysisProgressBreakdown(status);
  const patterns = status.patterns;
  const stages = fetchStatusMacroStages(response);
  const warnings = statusWarnings(status);
  const progress = presentation.determinateTotal && presentation.determinateTotal > 0
    ? Math.min(100, (presentation.determinateCompleted / presentation.determinateTotal) * 100)
    : null;
  const completedPipeline = fetchStatusHasCompletedPipeline(response);

  return (
    <>
      <Button
        size="small"
        aria-label={compact.ariaLabel}
        aria-haspopup="dialog"
        aria-controls={popoverID}
        aria-expanded={Boolean(anchor)}
        onClick={(event) => setAnchor(event.currentTarget)}
        endIcon={<ExpandMore sx={{ fontSize: 16 }} />}
        sx={{
          minWidth: { xs: 44, md: "auto" },
          width: { xs: 44, md: "auto" },
          height: { xs: 44, md: 34 },
          px: { xs: 0, md: 1.1 },
          border: "1px solid",
          borderColor: "divider",
          borderRadius: 999,
          color: `${compact.severity}.main`,
          bgcolor: (theme) => (theme.vars ?? theme).palette.surface.container,
          textTransform: "none",
          whiteSpace: "nowrap",
          boxShadow: "none",
          "& .MuiButton-endIcon": {
            display: { xs: "none", md: "inherit" },
          },
          "&:hover": {
            borderColor: `${compact.severity}.main`,
            bgcolor: (theme) => soft(theme, compact.severity, 0.08),
          },
        }}
      >
        <Box
          component="span"
          sx={{
            display: "inline-flex",
            alignItems: "center",
            gap: { xs: 0, md: 1 },
          }}
        >
          {stateIcon(response)}
          <Box
            component="span"
            sx={{
              display: { xs: "none", md: "inline" },
              color: "text.primary",
              fontSize: "0.75rem",
              fontWeight: 700,
            }}
          >
            {compact.label}
          </Box>
        </Box>
      </Button>
      <Box component="span" role="status" aria-live="polite" aria-atomic="true" sx={visuallyHidden}>
        {presentation.announcement}
      </Box>
      <Popover
        id={popoverID}
        open={Boolean(anchor)}
        anchorEl={anchor}
        onClose={() => setAnchor(null)}
        anchorOrigin={{ vertical: "bottom", horizontal: "right" }}
        transformOrigin={{ vertical: "top", horizontal: "right" }}
        slotProps={{
          paper: {
            sx: {
              mt: 1,
              width: { xs: "calc(100vw - 32px)", sm: 390 },
              maxWidth: 390,
              maxHeight: "calc(100vh - 32px)",
              overflowY: "auto",
              border: "1px solid",
              borderColor: "divider",
              borderRadius: "8px",
              bgcolor: (theme) => (theme.vars ?? theme).palette.surface.container,
              backgroundImage: "none",
              boxShadow: "0 18px 50px rgba(0, 0, 0, 0.28)",
            },
          },
        }}
      >
        <Box role="dialog" aria-label="Fetch status details" sx={{ p: 2 }}>
          <Stack direction="row" spacing={1.25} sx={{ alignItems: "flex-start" }}>
            <Box sx={{ color: `${compact.severity}.main`, display: "flex", mt: 0.25 }}>{stateIcon(response, 20)}</Box>
            <Box sx={{ minWidth: 0, flex: 1 }}>
              <Typography variant="caption" color="text.secondary" sx={{ display: "block", fontWeight: 700 }}>
                Data refresh
              </Typography>
              <Typography variant="subtitle2" sx={{ fontWeight: 700, mt: 0.125 }}>
                {presentation.title}
              </Typography>
              <Typography variant="body2" color="text.secondary" sx={{ mt: 0.25 }}>
                {presentation.detail}
              </Typography>
            </Box>
          </Stack>

          {progress !== null && (
            <LinearProgress
              aria-label={`${presentation.title} progress`}
              aria-valuetext={`${presentation.determinateCompleted} of ${presentation.determinateTotal}`}
              variant="determinate"
              value={progress}
              sx={{ height: 4, borderRadius: 999, mt: 1.5 }}
            />
          )}

          <Box sx={{ mt: 1.5 }}>
            <MacroStageProgress stages={stages} />
          </Box>

          {warnings.length > 0 && (
            <Stack
              aria-label="Refresh warnings"
              direction="row"
              spacing={1}
              sx={{
                mt: 1.25,
                p: 1,
                borderRadius: 1,
                color: "warning.main",
                bgcolor: (theme) => soft(theme, "warning", 0.1),
                alignItems: "flex-start",
              }}
            >
              <WarningAmber sx={{ fontSize: 17, mt: 0.125, flex: "0 0 auto" }} />
              <Typography variant="caption" sx={{ color: "text.primary" }}>
                {warnings.join(" · ")}
              </Typography>
            </Stack>
          )}

          <Stack spacing={0.75} sx={{ mt: 1.5 }}>
            <DetailRow label="Last published" value={<TimestampValue value={status.last_successful_publication_at} />} />
            {completedPipeline ? (
              <>
                {status.next_watch_at && <DetailRow label="Next check" value={<TimestampValue value={status.next_watch_at} />} />}
                {status.next_reconcile_at && status.next_reconcile_at !== status.next_watch_at && (
                  <DetailRow label="Next full reconciliation" value={<TimestampValue value={status.next_reconcile_at} />} />
                )}
              </>
            ) : (
              <>
                <DetailRow label="Current pass began" value={<TimestampValue value={status.pass_started_at} />} />
                <DetailRow label="Last activity" value={<TimestampValue value={status.last_progress_at} />} />
              </>
            )}
          </Stack>

          {!completedPipeline && (
            <Typography variant="caption" color="text.secondary" sx={{ display: "block", mt: 1.25, lineHeight: 1.5 }}>
              {response.state === "active"
                ? "The last published dashboard remains available while this refresh finishes."
                : "The last published dashboard remains available."}
            </Typography>
          )}

          <Button
            size="small"
            aria-expanded={technicalOpen}
            aria-controls={technicalID}
            onClick={() => setTechnicalOpen((open) => !open)}
            endIcon={(
              <ExpandMore
                sx={{
                  fontSize: 16,
                  transform: technicalOpen ? "rotate(180deg)" : "rotate(0deg)",
                  transition: reduceMotion ? "none" : "transform 150ms ease",
                }}
              />
            )}
            sx={{ mt: 1.25, px: 0.5, textTransform: "none" }}
          >
            Technical details
          </Button>
          <Collapse in={technicalOpen} timeout={reduceMotion ? 0 : "auto"}>
            <Stack id={technicalID} spacing={0.75} sx={{ mt: 0.75 }}>
              <Divider sx={{ mb: 0.5 }} />
              <Typography variant="caption" color="text.secondary" sx={{ fontWeight: 700 }}>
                Pipeline
              </Typography>
              <DetailRow label="Phase" value={status.phase} />
              <DetailRow label="Pattern stage" value={status.pattern_phase} />
              <DetailRow label="Publication stage" value={status.publication_phase} />
              <DetailRow label="Follow-up stage" value={status.side_effect_phase} />
              <DetailRow label="Phase began" value={formatFetchTimestamp(status.phase_started_at)} />
              <DetailRow label="Last checked" value={formatFetchTimestamp(status.last_checked_at)} />
              <DetailRow label="Phase durations" value={phaseDurationDetail(status)} />
              <DetailRow label="Run ID" value={status.run_id} />
              <DetailRow label="Pass ID" value={status.pass_id} />

              <Divider sx={{ my: 0.5 }} />
              <Typography variant="caption" color="text.secondary" sx={{ fontWeight: 700 }}>
                Analysis accounting
              </Typography>
              <DetailRow label="Results ready" value={analysis.total > 0 ? `${analysis.ready} / ${analysis.total}` : "Not planned"} />
              <DetailRow label="Reused from cache" value={analysis.reusedFromCache} />
              <DetailRow label="Compatible results" value={analysis.compatibleResults} />
              <DetailRow label="Exact results reused" value={analysis.exactResultsReused} />
              <DetailRow label="Same-failure results reused" value={analysis.sameFailureResultsReused} />
              <DetailRow label="Existing Tasks adopted" value={analysis.lateTasksAdopted} />
              <DetailRow label="New analyzer Tasks" value={analysis.newTasksCreated} />
              <DetailRow label="Fresh analyses completed" value={analysis.freshAnalysesCompleted} />
              <DetailRow label="Currently analyzing" value={analysis.analyzing} />
              <DetailRow label="Waiting to check" value={analysis.waiting} />
              <DetailRow label="Analysis failures" value={analysis.failed} />
              <DetailRow label="Analysis cancellations" value={analysis.cancelled} />
              <DetailRow label="Task attempts" value={status.analyses.task_attempts} />
              <DetailRow
                label="Results retrieved"
                value={`${status.analyses.results_retrieved} total, including adopted existing Tasks`}
              />
              <DetailRow
                label="Retries"
                value={`${status.analyses.retries} Task · ${status.analyses.result_retrieval_retries} result retrieval`}
              />
              <DetailRow
                label="Planned Task work"
                value={`${status.analyses.new_work} without cache · ${status.analyses.stale_work} stale`}
              />
              <DetailRow
                label="Same-failure candidates"
                value={`${analysis.sameFailureCandidates} subjects · ${analysis.sameFailureGroups} groups · up to ${analysis.potentialTasksSaved} fewer Tasks · largest ${analysis.largestSameFailureGroup}`}
              />
              <DetailRow label="Cache rejections" value={cacheRejectionDetail(status)} />
              <DetailRow label="Checkpoint" value={status.analyses.checkpoint_committed ? "Saved" : "Not saved"} />

              {patterns && (
                <>
                  <Divider sx={{ my: 0.5 }} />
                  <Typography variant="caption" color="text.secondary" sx={{ fontWeight: 700 }}>
                    Pattern accounting
                  </Typography>
                  <DetailRow
                    label="Patterns"
                    value={`${patterns.current ?? patterns.completed} current · ${patterns.retained ?? 0} retained · ${patterns.cache_hits ?? 0} cached · ${patterns.failed} failed`}
                  />
                  <DetailRow
                    label="Pattern repairs"
                    value={`${patterns.repairs ?? 0} attempted · ${patterns.repair_succeeded ?? 0} succeeded · ${patterns.repair_failed ?? 0} failed`}
                  />
                  {patterns.repair_failure_category && (
                    <DetailRow label="Repair failure" value={patternFailureLabel(patterns.repair_failure_category)} />
                  )}
                  {patterns.failure_category && (
                    <DetailRow label="Pattern failure" value={patternFailureLabel(patterns.failure_category)} />
                  )}
                </>
              )}

              {response.history && response.history.length > 0 && (
                <>
                  <Divider sx={{ my: 0.5 }} />
                  <Typography variant="caption" color="text.secondary" sx={{ fontWeight: 700 }}>
                    Recent passes
                  </Typography>
                  {response.history.slice(-5).reverse().map((pass) => (
                    <DetailRow
                      key={`${pass.started_at}/${pass.pass_type}`}
                      label={passTypeLabel(pass.pass_type)}
                      value={recentPassDetail(pass)}
                    />
                  ))}
                </>
              )}
            </Stack>
          </Collapse>
        </Box>
      </Popover>
    </>
  );
}

export function FetchStatusStrip({ response, dismissedKey, onDismiss }: FetchStatusStripProps) {
  const presentation = response ? fetchStatusPresentation(response) : null;
  const status = response?.status;
  if (!response || !presentation || !status || !shouldShowFetchStatusStrip(response)) return null;

  const stripKey = fetchStatusStripKey(response);
  if (dismissedKey === stripKey) return null;

  return (
    <Box
      role="region"
      aria-label={presentation.ariaLabel}
      sx={{
        borderBottom: "1px solid",
        borderColor: "divider",
        bgcolor: (theme) => soft(theme, presentation.severity, 0.08),
      }}
    >
      <Container
        maxWidth="xl"
        sx={{
          minHeight: 44,
          py: 0.75,
          display: "grid",
          gridTemplateColumns: "auto minmax(0, 1fr) auto",
          columnGap: 1.25,
          alignItems: "center",
        }}
      >
        <Box sx={{ color: `${presentation.severity}.main`, display: "flex" }}>{stateIcon(response, 19)}</Box>
        <Box sx={{ minWidth: 0 }}>
          <Typography variant="caption" sx={{ display: "block", color: "text.primary", fontWeight: 700 }}>
            {presentation.title}
          </Typography>
          <Typography
            variant="caption"
            color="text.secondary"
            sx={{ display: "block", overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}
          >
            {presentation.detail}
          </Typography>
        </Box>
        <Tooltip title="Hide this fetch status update">
          <IconButton
            size="small"
            aria-label="Hide this fetch status update"
            onClick={() => onDismiss(stripKey)}
            sx={{ color: "text.secondary" }}
          >
            <Close sx={{ fontSize: 17 }} />
          </IconButton>
        </Tooltip>
      </Container>
    </Box>
  );
}
