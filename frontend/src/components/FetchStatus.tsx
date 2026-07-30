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
import { useLayoutEffect, useRef, useState, type ReactNode } from "react";
import {
  fetchStatusCompactPresentation,
  fetchStatusPresentation,
  fetchStatusStripKey,
  formatFetchTimestamp,
  patternFailureLabel,
} from "../lib/fetchStatus";
import type { FetchProgressStatus, FetchStatusResponse } from "../types/fetchStatus";
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

function ActiveStateIcon({ size }: { size: number }) {
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
      return <ActiveStateIcon size={size} />;
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

function analysisDone(status: FetchProgressStatus): number {
  return status.analyses.completed + status.analyses.failed + status.analyses.cancelled;
}

export function FetchStatusControl({ response, idleCompact, onIdleCompactChange }: FetchStatusControlProps) {
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  const compact = response ? fetchStatusCompactPresentation(response) : null;
  const presentation = response ? fetchStatusPresentation(response) : null;
  const status = response?.status;
  if (!response || !compact || !presentation || !status) return null;

  const iconOnly = compact.quiet && idleCompact;
  const popoverID = anchor ? "fetch-status-details" : undefined;
  const done = analysisDone(status);
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
              label="Analysis"
              value={status.analyses.logical_total > 0 ? `${done} / ${status.analyses.logical_total}` : "Not planned"}
            />
            <DetailRow label="Running / queued" value={`${status.analyses.running} / ${status.analyses.queued}`} />
            <DetailRow
              label="Task attempts"
              value={`${status.analyses.task_attempts}${status.analyses.retries ? ` · ${status.analyses.retries} ${status.analyses.retries === 1 ? "retry" : "retries"}` : ""}`}
            />
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
          </Stack>

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
      role="status"
      aria-live="polite"
      aria-label={presentation.ariaLabel}
      sx={{
        borderBottom: "1px solid",
        borderColor: "divider",
        bgcolor: (theme) => soft(theme, presentation.severity, response.state === "active" ? 0.04 : 0.08),
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
