import { useEffect, useState, type ReactNode } from "react";
import { Link as RouterLink, useParams, useSearchParams } from "react-router-dom";
import Alert from "@mui/material/Alert";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import CircularProgress from "@mui/material/CircularProgress";
import Link from "@mui/material/Link";
import MenuItem from "@mui/material/MenuItem";
import Stack from "@mui/material/Stack";
import TextField from "@mui/material/TextField";
import Typography from "@mui/material/Typography";
import { AnalysisChatHistoryList } from "../components/AnalysisChatHistory";
import { AnalysisChatTranscript } from "../components/AnalysisChat";
import { DetailSectionBand } from "../components/DetailSectionBand";
import { useAuth } from "../hooks/useAuth";
import { useCapabilities } from "../hooks/useCapabilities";
import { analysisChatHistoryQuery, getAnalysisChatSession, isAnalysisChatOAuthExpired } from "../lib/analysisChat";
import type { AnalysisChatSession } from "../types/analysisChat";
import { overviewTypography } from "../theme/overview";

function HistoryFrame({ children }: { children: ReactNode }) {
  const { features } = useCapabilities();
  const auth = useAuth();
  return (
    <Stack spacing={2.5} sx={{ maxWidth: 1040, mx: "auto" }}>
      <Box>
        <Typography sx={overviewTypography.eyebrow}>Operator workspace</Typography>
        <Typography component="h1" sx={overviewTypography.pageHeadline}>Investigation history</Typography>
        <Typography color="textSecondary" sx={{ mt: 0.75, ...overviewTypography.primaryBody }}>
          Saved conversations and their original evidence. New analysis does not replace earlier findings.
        </Typography>
      </Box>
      {!features.analysis_chat ? <Alert severity="info">Investigation history is not available in this deployment.</Alert>
        : auth.status === "loading" ? <CircularProgress size={24} aria-label="Checking authentication" />
        : auth.status !== "authenticated" ? <Alert severity="info" action={auth.status === "anonymous" ? <Button color="inherit" onClick={auth.signIn}>Sign in</Button> : undefined}>Sign in as a dashboard operator to view private conversation history.</Alert>
        : children}
    </Stack>
  );
}

function HistoryFilters({ jobID, scope, onApply }: { jobID: string; scope: string; onApply: (job: string, scope: string) => void }) {
  const [job, setJob] = useState(jobID);
  const [kind, setKind] = useState(scope);
  return (
    <Stack component="form" direction={{ xs: "column", sm: "row" }} spacing={1.5}
      onSubmit={(event) => { event.preventDefault(); onApply(job, kind); }}>
      <TextField size="small" label="Job ID" value={job} onChange={(event) => setJob(event.target.value)} sx={{ flex: 1 }} />
      <TextField select size="small" label="Scope" value={kind} onChange={(event) => setKind(event.target.value)} sx={{ minWidth: 170 }}>
        <MenuItem value="">All scopes</MenuItem>
        <MenuItem value="test">Test or build failure</MenuItem>
        <MenuItem value="pattern">Whole pattern</MenuItem>
        <MenuItem value="cause">Cause</MenuItem>
      </TextField>
      <Button type="submit" variant="outlined" sx={{ minHeight: 44 }}>Apply filters</Button>
    </Stack>
  );
}

export function InvestigationHistoryPage() {
  const [params, setParams] = useSearchParams();
  const jobID = params.get("job_id") ?? "";
  const scope = params.get("scope") ?? "";
  const key = analysisChatHistoryQuery(jobID, scope);
  return (
    <HistoryFrame>
      <HistoryFilters key={key} jobID={jobID} scope={scope} onApply={(job, kind) => setParams(analysisChatHistoryQuery(job, kind))} />
      <Box component="section" sx={{ bgcolor: "surface.container", borderBottom: "1px solid", borderColor: "divider" }}>
        <DetailSectionBand title="Saved conversations" metadata="Newest activity first" />
        <Box sx={{ p: 2 }}><AnalysisChatHistoryList key={key} jobID={jobID} scope={scope} /></Box>
      </Box>
    </HistoryFrame>
  );
}

export function InvestigationSessionPage() {
  const { sessionID = "" } = useParams();
  const { status, login, mode, signIn } = useAuth();
  const { features } = useCapabilities();
  const key = `${login ?? ""}:${sessionID}`;
  const [loaded, setLoaded] = useState<{ key: string; session?: AnalysisChatSession; error?: string }>();
  useEffect(() => {
    if (!features.analysis_chat || status !== "authenticated") return;
    const controller = new AbortController();
    void getAnalysisChatSession(sessionID, controller.signal).then(
      (session) => { if (!controller.signal.aborted) setLoaded({ key, session }); },
      (error: unknown) => {
        if (controller.signal.aborted) return;
        if (isAnalysisChatOAuthExpired(error, mode)) { signIn(); return; }
        setLoaded({ key, error: error instanceof Error ? error.message : "Unable to load conversation" });
      },
    );
    return () => controller.abort();
  }, [mode, signIn, status, features.analysis_chat, key, sessionID]);
  const session = loaded?.key === key ? loaded.session : undefined;
  const error = loaded?.key === key ? loaded.error : undefined;
  return (
    <HistoryFrame>
      <Link component={RouterLink} to="/investigations">All saved conversations</Link>
      {!session && !error && <CircularProgress size={24} aria-label="Loading saved conversation" />}
      {error && <Alert severity="error">{error}</Alert>}
      {session && (
        <Box component="section" sx={{ bgcolor: "surface.container", borderBottom: "1px solid", borderColor: "divider" }}>
          <DetailSectionBand title="Saved conversation" metadata={session.archived ? "Archived" : "Original evidence snapshot"} />
          <Stack spacing={2} sx={{ p: { xs: 1.5, sm: 2.5 } }}>
            <Box>
              <Typography component="h2" sx={{ ...overviewTypography.majorHeading, overflowWrap: "anywhere" }}>{session.title || session.analysis.test_name || "Investigation"}</Typography>
              <Typography color="textSecondary" sx={{ mt: 1, overflowWrap: "anywhere", ...overviewTypography.data }}>
                {session.analysis.job_id} · {session.analysis.scope ?? "test"} · {new Date(session.created_at).toLocaleString()} · {session.created_by}
              </Typography>
              {session.build_ids?.length ? <Typography variant="body2" sx={{ mt: 0.5, overflowWrap: "anywhere" }}>Builds: {session.build_ids.join(", ")}</Typography> : null}
              {session.comparison_build_id && <Typography variant="body2">Comparison build: {session.comparison_build_id}</Typography>}
              {session.history_expires_at && <Typography variant="caption" color="textSecondary">Retained until {new Date(session.history_expires_at).toLocaleString()}</Typography>}
            </Box>
            <Alert severity="info">This is the original conversation, not a conclusion about newer runs. Viewing history does not extend its retention.</Alert>
            <AnalysisChatTranscript session={session} />
          </Stack>
        </Box>
      )}
    </HistoryFrame>
  );
}
