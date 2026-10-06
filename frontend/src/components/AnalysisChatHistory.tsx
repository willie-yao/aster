import { useEffect, useId, useState } from "react";
import { Link as RouterLink } from "react-router-dom";
import Alert from "@mui/material/Alert";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import ButtonBase from "@mui/material/ButtonBase";
import Chip from "@mui/material/Chip";
import CircularProgress from "@mui/material/CircularProgress";
import Collapse from "@mui/material/Collapse";
import Link from "@mui/material/Link";
import Stack from "@mui/material/Stack";
import Typography from "@mui/material/Typography";
import ChevronRight from "@mui/icons-material/ChevronRight";
import HistoryOutlined from "@mui/icons-material/HistoryOutlined";
import { analysisChatHistoryQuery, analysisChatScopeLabel, isAnalysisChatOAuthExpired, listAnalysisChatSessions } from "../lib/analysisChat";
import { useAuth } from "../hooks/useAuth";
import type { AnalysisChatHistoryPage } from "../types/analysisChat";
import { overviewTypography, touchTargetSx } from "../theme/overview";

export function AnalysisChatHistoryList({ jobID = "", scope = "", currentSessionID, refreshKey = "", pageSize = 20 }: {
  jobID?: string; scope?: string; currentSessionID?: string; refreshKey?: string; pageSize?: number;
}) {
  const { mode, signIn } = useAuth();
  const [cursors, setCursors] = useState<string[]>([""]);
  const [retry, setRetry] = useState(0);
  const filters = analysisChatHistoryQuery(jobID, scope, cursors.at(-1));
  const query = `${filters}${filters ? "&" : ""}limit=${pageSize}`;
  const key = `${query}:${currentSessionID ?? ""}:${refreshKey}:${retry}`;
  const [loaded, setLoaded] = useState<{ key: string; page?: AnalysisChatHistoryPage; error?: string }>();
  useEffect(() => {
    const controller = new AbortController();
    void listAnalysisChatSessions(query, controller.signal).then(
      (page) => { if (!controller.signal.aborted) setLoaded({ key, page }); },
      (error: unknown) => {
        if (controller.signal.aborted) return;
        if (isAnalysisChatOAuthExpired(error, mode)) { signIn(); return; }
        setLoaded({ key, error: error instanceof Error ? error.message : "Unable to load history" });
      },
    );
    return () => controller.abort();
  }, [key, mode, query, signIn]);
  const page = loaded?.key === key ? loaded.page : undefined;
  const error = loaded?.key === key ? loaded.error : undefined;
  const sessions = page?.sessions.filter((session) => session.id !== currentSessionID) ?? [];
  return (
    <Stack spacing={1.25}>
      {!page && !error && <Box sx={{ py: 1 }}><CircularProgress size={20} aria-label="Loading conversation history" /></Box>}
      {error && <Alert severity="error" action={<Button color="inherit" onClick={() => setRetry((value) => value + 1)} sx={touchTargetSx}>Retry</Button>}>{error}</Alert>}
      {page && sessions.length === 0 && <Typography variant="body2" color="textSecondary">
        {jobID || scope ? "No saved conversations match these filters on this page." : "No saved conversations on this page."}
      </Typography>}
      {sessions.map((session) => (
        <Box key={session.id} sx={{ borderBottom: "1px solid", borderColor: "divider", pb: 1.25, minWidth: 0 }}>
          <Stack direction="row" spacing={1} useFlexGap sx={{ flexWrap: "wrap", alignItems: "center", mb: 0.5 }}>
            <Chip size="small" label={analysisChatScopeLabel(session.analysis)} variant="outlined" />
            {(session.archived || session.read_only) && <Typography variant="caption" color="textSecondary">{session.archived ? "Archived · Read-only" : "Read-only"}</Typography>}
          </Stack>
          <Link component={RouterLink} to={`/investigations/${encodeURIComponent(session.id)}`}
            underline="hover"
            sx={{ display: "inline-flex", alignItems: "center", ...touchTargetSx, py: 0.5, overflowWrap: "anywhere", ...overviewTypography.primaryBody, fontWeight: 650 }}>
            {session.title || session.analysis.test_name || "Saved conversation"}
          </Link>
          <Typography variant="caption" color="textSecondary" sx={{ display: "block", overflowWrap: "anywhere" }}>
            Updated {new Date(session.updated_at).toLocaleString()}{session.created_by ? ` · ${session.created_by}` : ""}
          </Typography>
          <Typography variant="caption" color="textSecondary" sx={{ display: "block", overflowWrap: "anywhere" }}>
            {session.analysis.job_id}
          </Typography>
          <Typography color="textSecondary" sx={{ display: "block", overflowWrap: "anywhere", ...overviewTypography.data }}>
            {session.build_ids?.length ? `Builds: ${session.build_ids.join(", ")}` : ""}
            {session.comparison_build_id ? `${session.build_ids?.length ? " · " : ""}Comparison build: ${session.comparison_build_id}` : ""}
          </Typography>
        </Box>
      ))}
      <Stack direction="row" spacing={1} useFlexGap sx={{ flexWrap: "wrap" }}>
        {cursors.length > 1 && <Button size="small" sx={touchTargetSx} onClick={() => setCursors((items) => items.slice(0, -1))}>Newer conversations</Button>}
        {page?.next_cursor && <Button size="small" sx={touchTargetSx} onClick={() => setCursors((items) => [...items, page.next_cursor!])}>Older conversations</Button>}
      </Stack>
    </Stack>
  );
}

// AnalysisChatHistory starts collapsed and loads the list only when opened, so
// earlier conversations never take room from the current one by default.
export function AnalysisChatHistory({ jobID, scope, currentSessionID, refreshKey }: {
  jobID: string; scope: string; currentSessionID?: string; refreshKey: string;
}) {
  const [open, setOpen] = useState(false);
  const listID = useId();
  return (
    <Box component="section" aria-label="Earlier conversations" sx={{ px: 2, py: 0.5, borderTop: "1px solid", borderColor: "divider", bgcolor: "surface.container", flexShrink: 0 }}>
      <Stack direction="row" spacing={1} sx={{ alignItems: "center" }}>
        <ButtonBase
          type="button"
          onClick={() => setOpen((wasOpen) => !wasOpen)}
          aria-expanded={open}
          aria-controls={open ? listID : undefined}
          sx={{
            ...touchTargetSx,
            flex: 1,
            minWidth: 0,
            mx: -0.5,
            px: 0.5,
            justifyContent: "flex-start",
            gap: 1,
            borderRadius: 1,
            color: "text.primary",
            textAlign: "left",
            "&:hover": { bgcolor: "action.hover" },
            "&.Mui-focusVisible": { outline: "2px solid", outlineColor: "primary.main", outlineOffset: -2 },
          }}
        >
          <HistoryOutlined fontSize="small" color="action" sx={{ display: { xs: "none", sm: "inline-block" } }} />
          <Typography component="span" variant="subtitle2" noWrap sx={{ minWidth: 0 }}>Earlier conversations</Typography>
          <ChevronRight
            aria-hidden="true"
            sx={{
              fontSize: 20,
              color: "text.secondary",
              transform: open ? "rotate(90deg)" : "rotate(0deg)",
              transition: (theme) => theme.transitions.create("transform", { duration: theme.transitions.duration.shortest }),
              "@media (prefers-reduced-motion: reduce)": { transition: "none" },
            }}
          />
        </ButtonBase>
        <Link component={RouterLink} to={`/investigations?${analysisChatHistoryQuery(jobID, scope)}`} variant="body2"
          sx={{ ...touchTargetSx, display: "inline-flex", alignItems: "center", flexShrink: 0 }}>View history</Link>
      </Stack>
      <Collapse in={open} timeout="auto" unmountOnExit>
        {/* Capped with its own scroll so an open list still leaves the
            current conversation most of the panel. */}
        <Box
          id={listID}
          sx={{
            maxHeight: { xs: "28vh", sm: "min(32vh, 280px)" },
            overflowY: "auto",
            pt: 0.5,
            pb: 1,
            pr: 0.5,
            scrollbarWidth: "thin",
            scrollbarColor: (theme) => `${theme.palette.divider} transparent`,
          }}
        >
          <Typography variant="caption" color="textSecondary" sx={{ display: "block", mb: 1 }}>
            Saved for this job and scope. These conversations may describe different causes or older evidence.
          </Typography>
          <AnalysisChatHistoryList key={`${jobID}:${scope}`} jobID={jobID} scope={scope} currentSessionID={currentSessionID} refreshKey={refreshKey} pageSize={5} />
        </Box>
      </Collapse>
    </Box>
  );
}
