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
import { analysisChatHistoryQuery, isAnalysisChatOAuthExpired, listAnalysisChatSessions } from "../lib/analysisChat";
import { useAuth } from "../hooks/useAuth";
import type { AnalysisChatHistoryPage } from "../types/analysisChat";
import { overviewTypography, touchTargetSx } from "../theme/overview";

export function AnalysisChatHistoryList({ jobID = "", scope = "", currentSessionID, refreshKey = "", pageSize = 20 }: {
  jobID?: string; scope?: string; currentSessionID?: string; refreshKey?: string; pageSize?: number;
}) {
  const { mode, signIn } = useAuth();
  const [cursors, setCursors] = useState<string[]>([""]);
  const filters = analysisChatHistoryQuery(jobID, scope, cursors.at(-1));
  const query = `${filters}${filters ? "&" : ""}limit=${pageSize}`;
  const key = `${query}:${currentSessionID ?? ""}:${refreshKey}`;
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
      {error && <Alert severity="error">{error}</Alert>}
      {page && sessions.length === 0 && <Typography variant="body2" color="textSecondary">No earlier saved conversations on this page.</Typography>}
      {sessions.map((session) => (
        <Box key={session.id} sx={{ borderBottom: "1px solid", borderColor: "divider", pb: 1.25, minWidth: 0 }}>
          <Stack direction="row" spacing={1} useFlexGap sx={{ flexWrap: "wrap", alignItems: "center", mb: 0.5 }}>
            <Chip size="small" label={session.analysis.scope ?? "test"} variant="outlined" />
            <Typography variant="caption" color="textSecondary">{new Date(session.updated_at).toLocaleString()} · {session.created_by}</Typography>
            {session.archived && <Typography variant="caption" color="textSecondary">Archived</Typography>}
          </Stack>
          <Link component={RouterLink} to={`/investigations/${encodeURIComponent(session.id)}`}
            sx={{ display: "inline-block", py: 0.5, overflowWrap: "anywhere", ...overviewTypography.primaryBody }}>
            {session.title || session.analysis.test_name || "Saved conversation"}
          </Link>
          <Typography variant="caption" color="textSecondary" sx={{ display: "block", overflowWrap: "anywhere" }}>
            {session.analysis.job_id}{session.build_ids?.length ? ` · Builds ${session.build_ids.join(", ")}` : ""}
            {session.comparison_build_id ? ` · Compared with ${session.comparison_build_id}` : ""}
          </Typography>
        </Box>
      ))}
      <Stack direction="row" spacing={1}>
        {cursors.length > 1 && <Button size="small" onClick={() => setCursors((items) => items.slice(0, -1))}>Newer conversations</Button>}
        {page?.next_cursor && <Button size="small" onClick={() => setCursors((items) => [...items, page.next_cursor!])}>Older conversations</Button>}
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
        <Link component={RouterLink} to={`/investigations?${analysisChatHistoryQuery(jobID, scope)}`} variant="body2">View history</Link>
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
          <AnalysisChatHistoryList key={`${jobID}:${scope}`} jobID={jobID} scope={scope} currentSessionID={currentSessionID} refreshKey={refreshKey} pageSize={5} />
        </Box>
      </Collapse>
    </Box>
  );
}
