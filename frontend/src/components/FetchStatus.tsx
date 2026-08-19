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
  fetchStatusPublishedThisPass,
  fetchStatusStripKey,
  fetchStatusWarningGroups,
  formatFetchRelativeTime,
  formatFetchTimestamp,
  shouldShowFetchStatusStrip,
  type FetchMacroStagePresentation,
  type FetchMacroStageState,
} from "../lib/fetchStatus";
import type { FetchFollowUpComponent, FetchPassSummary, FetchProgressStatus, FetchStatusResponse } from "../types/fetchStatus";
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

function stateIcon(response: FetchStatusResponse, size = 18, severity?: "info" | "warning" | "error" | "success"): ReactNode {
  switch (response.state) {
    case "active":
      return <FetchActivityIcon size={size} />;
    case "failed":
      return severity === "warning" ? <WarningAmber sx={{ fontSize: size }} /> : <ErrorOutlineOutlined sx={{ fontSize: size }} />;
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
      <Typography variant="caption" color="textSecondary">
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
  const outcome = pass.outcome === "succeeded" ? "Succeeded" : pass.outcome.charAt(0).toUpperCase() + pass.outcome.slice(1);
  return `${outcome} · ${formatPassDuration(pass.duration_ms)} · ${pass.published ? "published" : "not published"}`;
}

function cacheRejectionCount(status: FetchProgressStatus): number {
  const rejections = status.analyses.cache_rejections;
  if (!rejections) return 0;
  return rejections.missing + rejections.expired + rejections.tool_floor
    + rejections.evidence_floor + rejections.critique + rejections.malformed;
}

function timingDetail(status: FetchProgressStatus): string | null {
  const durations = status.phase_durations_ms ?? {};
  const groups: Array<[string, string[]]> = [
    ["Fetch", ["setup", "discovery", "artifacts", "aggregation"]],
    ["Analyze", ["analysis-planning", "analysis"]],
    ["Patterns", ["patterns"]],
    ["Publish", ["publication"]],
    ["Follow-up", ["side-effects"]],
  ];
  const details = groups.flatMap(([label, phases]) => {
    const present = phases.some((phase) => durations[phase] !== undefined);
    if (!present) return [];
    const duration = phases.reduce((total, phase) => total + Math.max(0, durations[phase] ?? 0), 0);
    return [`${label} ${formatPassDuration(duration)}`];
  });
  return details.length > 0 ? details.join(" · ") : null;
}

function analysisDetail(status: FetchProgressStatus): string | null {
  const analysis = analysisProgressBreakdown(status);
  if (analysis.total <= 0) return null;
  const unavailable = analysis.failed + analysis.cancelled;
  return `${analysis.ready} ready · ${unavailable} unavailable · ${analysis.reused} reused · ${analysis.analyzing} running`;
}

function cacheDetail(status: FetchProgressStatus): string | null {
  const accepted = Math.max(0, status.analyses.accepted_cache_hits);
  const rejected = cacheRejectionCount(status);
  if (accepted === 0 && rejected === 0) return null;
  return `${accepted} accepted · ${rejected} rejected`;
}

function failureDetail(status: FetchProgressStatus): string | null {
  const parts: string[] = [];
  if (status.analyses.failed > 0) parts.push(`${status.analyses.failed} failed`);
  if (status.analyses.cancelled > 0) parts.push(`${status.analyses.cancelled} cancelled`);
  return parts.length > 0 ? parts.join(" · ") : null;
}

function patternDetail(status: FetchProgressStatus): string | null {
  const patterns = status.patterns;
  if (!patterns) return null;
  const parts = [`${patterns.current ?? patterns.completed} current`];
  if ((patterns.retained ?? 0) > 0) parts.push(`${patterns.retained} retained`);
  if (patterns.failed > 0) parts.push(`${patterns.failed} ${patterns.failed === 1 ? "attempt" : "attempts"} failed`);
  if ((patterns.repairs ?? 0) > 0) parts.push(`${patterns.repairs} ${patterns.repairs === 1 ? "repair" : "repairs"} attempted`);
  if ((patterns.repair_failed ?? 0) > 0) parts.push(`${patterns.repair_failed} ${patterns.repair_failed === 1 ? "repair" : "repairs"} failed`);
  return parts.join(" · ");
}

function followUpComponentDetail(label: string, component?: FetchFollowUpComponent): string | null {
  if (!component) return null;
  switch (component.state) {
    case "completed":
      return `${label} complete`;
    case "running":
      return `${label} running`;
    case "skipped":
      return `${label} skipped`;
    case "disabled":
      return `${label} disabled`;
    case "cancelled":
      return `${label} cancelled`;
    case "failed":
      return component.summary ?? `${label} failed`;
  }
}

function followUpDetail(status: FetchProgressStatus): string | null {
  const followUp = status.follow_up;
  if (!followUp) return null;
  const details = [
    followUpComponentDetail("Notifications", followUp.notifications),
    followUpComponentDetail("Remediation", followUp.remediation),
    followUpComponentDetail("Issue recovery", followUp.automatic_issues),
  ].filter((value): value is string => Boolean(value));
  return details.length > 0 ? details.join(" · ") : null;
}

function followUpFailureCodes(status: FetchProgressStatus): string[] {
  const followUp = status.follow_up;
  if (!followUp) return [];
  return [followUp.notifications, followUp.remediation, followUp.automatic_issues]
    .flatMap((component) => component?.code ? [component.code] : []);
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
        gridTemplateColumns: `repeat(${stages.length}, minmax(0, 1fr))`,
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
          <Typography variant="caption" color="textSecondary" sx={{ display: "block", fontSize: "0.6875rem", lineHeight: 1.2, mt: 0.25 }}>
            {stage.stateLabel}
          </Typography>
        </Box>
      ))}
    </Box>
  );
}


function CopyableDebugRow({ label, value }: { label: string; value: string }) {
  const [copied, setCopied] = useState(false);

  async function copy() {
    if (!navigator.clipboard) return;
    try {
      await navigator.clipboard.writeText(value);
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1600);
    } catch {
      setCopied(false);
    }
  }

  return (
    <Box sx={{ display: "grid", gridTemplateColumns: "minmax(96px, auto) minmax(0, 1fr) auto", gap: 1, alignItems: "center" }}>
      <Typography variant="caption" color="textSecondary">{label}</Typography>
      <Typography component="code" variant="caption" sx={{ textAlign: "right", overflowWrap: "anywhere" }}>{value}</Typography>
      <Button
        size="small"
        onClick={() => void copy()}
        aria-label={`Copy ${label.toLowerCase()}`}
        sx={{ minWidth: 44, minHeight: 32, px: 0.75, textTransform: "none" }}
      >
        {copied ? "Copied" : "Copy"}
      </Button>
    </Box>
  );
}

export function FetchStatusControl({ response }: FetchStatusControlProps) {
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  const [technicalOpen, setTechnicalOpen] = useState(false);
  const [debugOpen, setDebugOpen] = useState(false);
  const [historyOpen, setHistoryOpen] = useState(false);
  const technicalID = useId();
  const debugID = useId();
  const historyID = useId();
  const reduceMotion = useMediaQuery("(prefers-reduced-motion: reduce)");
  const compact = response ? fetchStatusCompactPresentation(response) : null;
  const presentation = response ? fetchStatusPresentation(response) : null;
  const status = response?.status;
  if (!response || !compact || !presentation || !status) return null;

  const popoverID = anchor ? "fetch-status-details" : undefined;
  const stages = fetchStatusMacroStages(response);
  const warningGroups = fetchStatusWarningGroups(status);
  const progress = presentation.determinateTotal && presentation.determinateTotal > 0
    ? Math.min(100, (presentation.determinateCompleted / presentation.determinateTotal) * 100)
    : null;
  const completedPipeline = fetchStatusHasCompletedPipeline(response);
  const publishedThisPass = fetchStatusPublishedThisPass(response);
  const timing = timingDetail(status);
  const analysisSummary = analysisDetail(status);
  const cacheSummary = cacheDetail(status);
  const patternsSummary = patternDetail(status);
  const followUpSummary = followUpDetail(status);
  const retryCount = Math.max(0, status.analyses.retries) + Math.max(0, status.analyses.result_retrieval_retries);
  const failures = failureDetail(status);
  const failureCodes = followUpFailureCodes(status);
  const availabilityMessage = response.state === "active"
    ? "The last published dashboard remains available while this refresh finishes."
    : publishedThisPass || completedPipeline
      ? "The latest published dashboard remains available."
      : "The last published dashboard remains available.";

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
          "& .MuiButton-endIcon": { display: { xs: "none", md: "inherit" } },
          "&:hover": {
            borderColor: `${compact.severity}.main`,
            bgcolor: (theme) => soft(theme, compact.severity, 0.08),
          },
        }}
      >
        <Box component="span" sx={{ display: "inline-flex", alignItems: "center", gap: { xs: 0, md: 1 } }}>
          {stateIcon(response, 18, presentation.severity)}
          <Box
            component="span"
            sx={{ display: { xs: "none", md: "inline" }, color: "text.primary", fontSize: "0.75rem", fontWeight: 700 }}
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
              overflowX: "hidden",
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
            <Box sx={{ color: `${compact.severity}.main`, display: "flex", mt: 0.25 }}>
              {stateIcon(response, 20, presentation.severity)}
            </Box>
            <Box sx={{ minWidth: 0, flex: 1 }}>
              <Typography variant="caption" color="textSecondary" sx={{ display: "block", fontWeight: 700 }}>
                Data refresh
              </Typography>
              <Typography variant="subtitle2" sx={{ fontWeight: 700, mt: 0.125 }}>
                {presentation.title}
              </Typography>
              <Typography variant="body2" color="textSecondary" sx={{ mt: 0.25 }}>
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

          {warningGroups.length > 0 && (
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
              <Stack spacing={0.75} sx={{ minWidth: 0 }}>
                {warningGroups.map((group) => (
                  <Box key={group.label}>
                    <Typography variant="caption" sx={{ display: "block", color: "text.primary", fontWeight: 700 }}>
                      {group.label}
                    </Typography>
                    {group.items.map((item) => (
                      <Typography key={item} variant="caption" color="textSecondary" sx={{ display: "block", lineHeight: 1.45 }}>
                        {item}
                      </Typography>
                    ))}
                  </Box>
                ))}
              </Stack>
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

          {status.last_successful_publication_at && (
            <Typography variant="caption" color="textSecondary" sx={{ display: "block", mt: 1.25, lineHeight: 1.5 }}>
              {availabilityMessage}
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
              {timing && <DetailRow label="Timing" value={timing} />}
              {analysisSummary && <DetailRow label="Analysis" value={analysisSummary} />}
              {cacheSummary && <DetailRow label="Cache" value={cacheSummary} />}
              {patternsSummary && <DetailRow label="Patterns" value={patternsSummary} />}
              {followUpSummary && <DetailRow label="Follow-up" value={followUpSummary} />}
              {retryCount > 0 && <DetailRow label="Retries" value={retryCount} />}
              {failures && <DetailRow label="Failures and cancellations" value={failures} />}

              <Button
                size="small"
                aria-expanded={debugOpen}
                aria-controls={debugID}
                onClick={() => setDebugOpen((open) => !open)}
                endIcon={(
                  <ExpandMore
                    sx={{
                      fontSize: 16,
                      transform: debugOpen ? "rotate(180deg)" : "rotate(0deg)",
                      transition: reduceMotion ? "none" : "transform 150ms ease",
                    }}
                  />
                )}
                sx={{ alignSelf: "flex-start", px: 0.5, textTransform: "none" }}
              >
                Debug identifiers
              </Button>
              <Collapse in={debugOpen} timeout={reduceMotion ? 0 : "auto"}>
                <Stack id={debugID} spacing={0.75} sx={{ pt: 0.25 }}>
                  <CopyableDebugRow label="Run ID" value={status.run_id} />
                  <CopyableDebugRow label="Pass ID" value={status.pass_id} />
                  {status.engine_version && <CopyableDebugRow label="Engine version" value={status.engine_version} />}
                  {status.failure_category && <CopyableDebugRow label="Failure category" value={status.failure_category} />}
                  {failureCodes.length > 0 && <CopyableDebugRow label="Follow-up code" value={failureCodes.join(", ")} />}
                  <CopyableDebugRow label="Run started" value={formatFetchTimestamp(status.run_started_at)} />
                  <CopyableDebugRow label="Pass started" value={formatFetchTimestamp(status.pass_started_at)} />
                  <CopyableDebugRow label="Phase started" value={formatFetchTimestamp(status.phase_started_at)} />
                  <CopyableDebugRow label="Last activity" value={formatFetchTimestamp(status.last_progress_at)} />
                  {status.last_checked_at && <CopyableDebugRow label="Last checked" value={formatFetchTimestamp(status.last_checked_at)} />}
                  {status.last_successful_publication_at && (
                    <CopyableDebugRow label="Last published" value={formatFetchTimestamp(status.last_successful_publication_at)} />
                  )}
                </Stack>
              </Collapse>

              {response.history && response.history.length > 0 && (
                <>
                  <Button
                    size="small"
                    aria-expanded={historyOpen}
                    aria-controls={historyID}
                    onClick={() => setHistoryOpen((open) => !open)}
                    endIcon={(
                      <ExpandMore
                        sx={{
                          fontSize: 16,
                          transform: historyOpen ? "rotate(180deg)" : "rotate(0deg)",
                          transition: reduceMotion ? "none" : "transform 150ms ease",
                        }}
                      />
                    )}
                    sx={{ alignSelf: "flex-start", px: 0.5, textTransform: "none" }}
                  >
                    Recent refreshes
                  </Button>
                  <Collapse in={historyOpen} timeout={reduceMotion ? 0 : "auto"}>
                    <Stack id={historyID} spacing={0.75} sx={{ pt: 0.25 }}>
                      {response.history.slice(-3).reverse().map((pass) => (
                        <DetailRow
                          key={`${pass.started_at}/${pass.pass_type}`}
                          label={passTypeLabel(pass.pass_type)}
                          value={recentPassDetail(pass)}
                        />
                      ))}
                    </Stack>
                  </Collapse>
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
        <Box sx={{ color: `${presentation.severity}.main`, display: "flex" }}>{stateIcon(response, 19, presentation.severity)}</Box>
        <Box sx={{ minWidth: 0 }}>
          <Typography variant="caption" sx={{ display: "block", color: "text.primary", fontWeight: 700 }}>
            {presentation.title}
          </Typography>
          <Typography
            variant="caption"
            color="textSecondary"
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
