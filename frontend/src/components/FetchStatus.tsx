import CheckCircleOutlined from "@mui/icons-material/CheckCircleOutlined";
import Close from "@mui/icons-material/Close";
import ErrorOutlineOutlined from "@mui/icons-material/ErrorOutlineOutlined";
import ExpandMore from "@mui/icons-material/ExpandMore";
import Schedule from "@mui/icons-material/Schedule";
import WarningAmber from "@mui/icons-material/WarningAmber";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import Chip from "@mui/material/Chip";
import CircularProgress from "@mui/material/CircularProgress";
import Collapse from "@mui/material/Collapse";
import Container from "@mui/material/Container";
import Divider from "@mui/material/Divider";
import FormControlLabel from "@mui/material/FormControlLabel";
import IconButton from "@mui/material/IconButton";
import LinearProgress from "@mui/material/LinearProgress";
import Popover from "@mui/material/Popover";
import Stack from "@mui/material/Stack";
import Switch from "@mui/material/Switch";
import Tooltip from "@mui/material/Tooltip";
import Typography from "@mui/material/Typography";
import { useId, useLayoutEffect, useRef, useState, type ReactNode } from "react";
import {
  analysisProgressBreakdown,
  fetchStatusCompactPresentation,
  fetchStatusPresentation,
  fetchStatusStripKey,
  formatFetchTimestamp,
  patternFailureLabel,
} from "../lib/fetchStatus";
import type { FetchPassSummary, FetchProgressStatus, FetchStatusResponse } from "../types/fetchStatus";
import { soft } from "../theme";

interface FetchStatusControlProps {
  response: FetchStatusResponse | null;
  idleCompact: boolean;
  onIdleCompactChange: (value: boolean) => void;
}

interface FetchStatusStripProps {
  response: FetchStatusResponse | null;
  dismissedKey: string | null;
  onDismiss: (key: string) => void;
}

const activeSpinnerDuration = 1400;

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
        { duration: activeSpinnerDuration, iterations: Infinity, easing: "linear" }
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
  return `${pass.outcome} · ${formatPassDuration(pass.duration_ms)} · ${pass.cache_hits} cache · ${pass.compatible_results_reused} compatible · ${pass.exact_results_reused} exact · ${pass.new_tasks_created} new Tasks · ${published}`;
}

function cacheRejectionDetail(status: FetchProgressStatus): string {
  const rejections = status.analyses.cache_rejections;
  if (!rejections) return "0";
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
  return details.length > 0 ? details.join(" · ") : "0";
}

export function FetchStatusControl({ response, idleCompact, onIdleCompactChange }: FetchStatusControlProps) {
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  const [technicalOpen, setTechnicalOpen] = useState(false);
  const technicalID = useId();
  const compact = response ? fetchStatusCompactPresentation(response) : null;
  const presentation = response ? fetchStatusPresentation(response) : null;
  const status = response?.status;
  if (!response || !compact || !presentation || !status) return null;

  const iconOnly = compact.quiet && idleCompact;
  const popoverID = anchor ? "fetch-status-details" : undefined;
  const analysis = analysisProgressBreakdown(status);
  const patterns = status.patterns;
  const progress = presentation.determinateTotal && presentation.determinateTotal > 0
    ? Math.min(100, (presentation.determinateCompleted / presentation.determinateTotal) * 100)
    : null;

  const control = (
    <Button
      size="small"
      aria-label={compact.ariaLabel}
      aria-haspopup="dialog"
      aria-controls={popoverID}
      aria-expanded={Boolean(anchor)}
      onClick={(event) => setAnchor(event.currentTarget)}
      endIcon={iconOnly ? undefined : <ExpandMore sx={{ fontSize: 16 }} />}
      sx={{
        minWidth: { xs: 34, md: iconOnly ? 34 : "auto" },
        width: { xs: 34, md: "auto" },
        height: 34,
        px: { xs: 0, md: iconOnly ? 0 : 1.1 },
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
          gap: { xs: 0, md: iconOnly ? 0 : 1 },
        }}
      >
        {stateIcon(response)}
        <Box
          component="span"
          sx={{
            display: { xs: "none", md: iconOnly ? "none" : "inline" },
            color: "text.primary",
            fontSize: "0.75rem",
            fontWeight: 700,
          }}
        >
          {compact.label}
        </Box>
      </Box>
    </Button>
  );

  return (
    <>
      {iconOnly ? <Tooltip title={compact.label}>{control}</Tooltip> : control}
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
              border: "1px solid",
              borderColor: "divider",
              borderRadius: 2.5,
              bgcolor: (theme) => (theme.vars ?? theme).palette.surface.glass,
              backdropFilter: "blur(16px)",
              backgroundImage: "none",
              boxShadow: "0 18px 50px rgba(0, 0, 0, 0.28)",
            },
          },
        }}
      >
        <Box role="dialog" aria-label="Fetch status details" sx={{ p: 2 }}>
          <Stack direction="row" spacing={1.25} sx={{ alignItems: "center" }}>
            <Box sx={{ color: `${compact.severity}.main`, display: "flex" }}>{stateIcon(response, 20)}</Box>
            <Box sx={{ minWidth: 0, flex: 1 }}>
              <Typography variant="subtitle2" sx={{ fontWeight: 700 }}>
                Data refresh
              </Typography>
              <Typography variant="caption" color="text.secondary">
                {presentation.title}
              </Typography>
            </Box>
            <Chip
              size="small"
              label={compact.label}
              sx={{
                color: `${compact.severity}.main`,
                bgcolor: (theme) => soft(theme, compact.severity, 0.12),
                fontWeight: 700,
              }}
            />
          </Stack>

          {response.state === "active" && (
            <Box sx={{ mt: 1.5 }}>
              <LinearProgress
                aria-label="Fetch progress"
                variant={progress === null ? "indeterminate" : "determinate"}
                value={progress ?? undefined}
                sx={{ height: 4, borderRadius: 999 }}
              />
            </Box>
          )}

          <Stack spacing={0.75} sx={{ mt: 2 }}>
            <DetailRow label="Last checked" value={formatFetchTimestamp(status.last_checked_at)} />
            <DetailRow label="Last published" value={formatFetchTimestamp(status.last_successful_publication_at)} />
            {status.next_watch_at && <DetailRow label="Next watch" value={formatFetchTimestamp(status.next_watch_at)} />}
            {status.next_reconcile_at && <DetailRow label="Next reconcile" value={formatFetchTimestamp(status.next_reconcile_at)} />}
          </Stack>

          <Divider sx={{ my: 1.5 }} />

          <Stack spacing={0.75}>
            <DetailRow
              label="Results ready"
              value={analysis.total > 0 ? `${analysis.ready} / ${analysis.total}` : "Not planned"}
            />
            <DetailRow label="Reused from cache" value={analysis.reusedFromCache} />
            <DetailRow label="Compatible results" value={analysis.compatibleResults} />
            <DetailRow label="Existing results adopted" value={analysis.exactResultsReused} />
            <DetailRow label="New analyzer Tasks" value={analysis.newTasksCreated} />
            <DetailRow label="Fresh analyses completed" value={analysis.freshAnalysesCompleted} />
            <DetailRow label="Currently analyzing" value={analysis.analyzing} />
            <DetailRow label="Waiting to check" value={analysis.waiting} />
            <DetailRow
              label="Failures"
              value={analysis.cancelled > 0 ? `${analysis.failed} · ${analysis.cancelled} cancelled` : analysis.failed}
            />
          </Stack>

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
                  transition: "transform 150ms ease",
                }}
              />
            )}
            sx={{ mt: 1.25, px: 0.5, textTransform: "none" }}
          >
            Technical details
          </Button>
          <Collapse in={technicalOpen} timeout="auto">
            <Stack id={technicalID} spacing={0.75} sx={{ mt: 0.75 }}>
              <DetailRow label="Late Task adoptions" value={analysis.lateTasksAdopted} />
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
              <DetailRow label="Cache rejections" value={cacheRejectionDetail(status)} />
              {status.analyses.checkpoint_committed && <DetailRow label="Checkpoint" value="Saved" />}
              {patterns && (patterns.eligible > 0 || patterns.attempts > 0 || (patterns.cache_hits ?? 0) > 0) && (
                <DetailRow
                  label="Patterns"
                  value={`${patterns.current ?? patterns.completed} current · ${patterns.retained ?? 0} retained · ${patterns.cache_hits ?? 0} cached`}
                />
              )}
              {patterns && (patterns.repairs ?? 0) > 0 && (
                <DetailRow
                  label="Repairs"
                  value={`${patterns.repair_succeeded ?? 0} succeeded · ${patterns.repair_failed ?? 0} failed`}
                />
              )}
              {patterns?.repair_failure_category && (
                <DetailRow label="Repair failure" value={patternFailureLabel(patterns.repair_failure_category)} />
              )}
              {patterns?.failure_category && (
                <DetailRow label="Pattern failure" value={patternFailureLabel(patterns.failure_category)} />
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

          <Divider sx={{ my: 1.5 }} />

          <FormControlLabel
            control={(
              <Switch
                size="small"
                checked={idleCompact}
                onChange={(event) => onIdleCompactChange(event.target.checked)}
              />
            )}
            label="Hide idle status label"
            sx={{ m: 0, "& .MuiFormControlLabel-label": { fontSize: "0.8125rem" } }}
          />
          <Typography variant="caption" color="text.secondary" sx={{ display: "block", mt: 0.25 }}>
            The status icon remains available in the header.
          </Typography>
        </Box>
      </Popover>
    </>
  );
}

export function FetchStatusStrip({ response, dismissedKey, onDismiss }: FetchStatusStripProps) {
  const presentation = response ? fetchStatusPresentation(response) : null;
  const status = response?.status;
  if (!response || !presentation || !status) return null;
  if (!["active", "failed", "stale", "interrupted", "cancelled"].includes(response.state)) return null;

  const stripKey = fetchStatusStripKey(response);
  if (dismissedKey === stripKey) return null;

  const progress = presentation.determinateTotal && presentation.determinateTotal > 0
    ? Math.min(100, (presentation.determinateCompleted / presentation.determinateTotal) * 100)
    : null;

  return (
    <Box
      role="region"
      aria-label={presentation.ariaLabel}
      sx={{
        borderBottom: "1px solid",
        borderColor: "divider",
        bgcolor: (theme) => soft(theme, presentation.severity, response.state === "active" ? 0.04 : 0.08),
      }}
    >
      <Box
        component="span"
        role="status"
        aria-live="polite"
        aria-atomic="true"
        sx={{
          border: 0,
          clip: "rect(0 0 0 0)",
          height: "1px",
          m: "-1px",
          overflow: "hidden",
          p: 0,
          position: "absolute",
          whiteSpace: "nowrap",
          width: "1px",
        }}
      >
        {presentation.announcement}
      </Box>
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
      {response.state === "active" && (
        <LinearProgress
          aria-label="Fetch progress"
          variant={progress === null ? "indeterminate" : "determinate"}
          value={progress ?? undefined}
          sx={{ height: 2 }}
        />
      )}
    </Box>
  );
}
