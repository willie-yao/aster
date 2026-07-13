import { useEffect, useState, type ReactNode } from "react";
import Alert from "@mui/material/Alert";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import CircularProgress from "@mui/material/CircularProgress";
import Dialog from "@mui/material/Dialog";
import DialogActions from "@mui/material/DialogActions";
import DialogContent from "@mui/material/DialogContent";
import DialogTitle from "@mui/material/DialogTitle";
import Link from "@mui/material/Link";
import Stack from "@mui/material/Stack";
import TextField from "@mui/material/TextField";
import Typography from "@mui/material/Typography";
import { BugReport, Build, GitHub, CheckCircleOutlined, Replay } from "@mui/icons-material";
import { useCapabilities } from "../hooks/useCapabilities";
import { useAuth } from "../hooks/useAuth";
import { useResolved } from "../hooks/useData";
import { soft } from "../theme";
import type { Theme } from "@mui/material/styles";

type Action = "create-issue" | "propose-fix";

interface Preview {
  token: string;
  kind: "issue" | "fix";
  title: string;
  body: string;
  diff?: string;
}

const API_BASE = import.meta.env.BASE_URL;

// stripComments removes the hidden dedup HTML comment from a body for display;
// the posted content keeps it.
function stripComments(s: string): string {
  return s.replace(/<!--[\s\S]*?-->/g, "").trim();
}

const sectionLabelSx = {
  display: "block",
  textTransform: "uppercase",
  fontSize: "0.625rem",
  fontWeight: 700,
  letterSpacing: "0.06em",
  color: "text.secondary",
  mb: 0.75,
} as const;

const previewBoxSx = {
  borderRadius: "10px",
  border: "1px solid",
  borderColor: "divider",
  bgcolor: (t: Theme) => (t.vars ?? t).palette.surface.containerLow,
  p: 1.75,
  fontFamily: "monospace",
  fontSize: "0.8125rem",
  lineHeight: 1.65,
  whiteSpace: "pre-wrap",
  wordBreak: "break-word",
} as const;

const dialogPaperSx = {
  borderRadius: "16px",
  border: "1px solid",
  borderColor: "divider",
  backgroundImage: "none",
} as const;

function DialogHeader({
  icon,
  accent,
  title,
  subtitle,
}: {
  icon: ReactNode;
  accent: "warning" | "success";
  title: string;
  subtitle?: string;
}) {
  return (
    <DialogTitle sx={{ display: "flex", alignItems: "center", gap: 1.5, px: 3, py: 2.25 }}>
      <Box
        sx={{
          display: "flex",
          alignItems: "center",
          justifyContent: "center",
          width: 38,
          height: 38,
          borderRadius: "11px",
          flexShrink: 0,
          color: `${accent}.main`,
          bgcolor: (t) => soft(t, accent, 0.15),
          border: "1px solid",
          borderColor: (t) => soft(t, accent, 0.3),
        }}
      >
        {icon}
      </Box>
      <Box sx={{ minWidth: 0 }}>
        <Typography variant="headline" component="span" sx={{ display: "block", fontSize: "1.125rem", lineHeight: 1.2 }}>
          {title}
        </Typography>
        {subtitle && (
          <Typography variant="caption" color="text.secondary" sx={{ display: "block", mt: 0.25 }}>
            {subtitle}
          </Typography>
        )}
      </Box>
    </DialogTitle>
  );
}

export function FailureActions({ failureID }: { failureID: string }) {
  const { features } = useCapabilities();
  const { status, signIn } = useAuth();
  const [action, setAction] = useState<Action | null>(null);
  const [busy, setBusy] = useState<"preview" | "refine" | "confirm" | null>(null);
  const [preview, setPreview] = useState<Preview | null>(null);
  // drafts caches the last generated preview per action so reopening the dialog
  // reuses it instead of regenerating (fix generation is expensive). A draft is
  // invalidated after a successful confirm or when its server-side token has
  // expired.
  const [drafts, setDrafts] = useState<Partial<Record<Action, Preview>>>({});
  const [instruction, setInstruction] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [url, setUrl] = useState<string | null>(null);

  const { data: resolved, refetch: refetchResolved } = useResolved();
  const [resolveOpen, setResolveOpen] = useState(false);
  const [note, setNote] = useState("");
  const [resolveBusy, setResolveBusy] = useState(false);
  const [resolveError, setResolveError] = useState<string | null>(null);

  // Cached drafts hold a server token bound to a specific failure. If this
  // instance is reused for a different failure (e.g. route change without a
  // remount), reset everything so a stale draft can never be confirmed against
  // the wrong failure.
  useEffect(() => {
    setAction(null);
    setBusy(null);
    setPreview(null);
    setDrafts({});
    setInstruction("");
    setError(null);
    setUrl(null);
  }, [failureID]);

  if (!features.actions || !failureID || status === "loading" || status === "unavailable") {
    return null;
  }

  if (status === "anonymous") {
    return (
      <Button
        size="small"
        variant="outlined"
        startIcon={<GitHub sx={{ fontSize: 18 }} />}
        onClick={signIn}
      >
        Sign in to file issues or fixes
      </Button>
    );
  }

  function open(act: Action) {
    setAction(act);
    setInstruction("");
    setError(null);
    setUrl(null);
    const cached = drafts[act];
    if (cached) {
      // Reuse the already-generated draft instead of regenerating.
      setPreview(cached);
    } else {
      setPreview(null);
      void loadPreview(act, false);
    }
  }

  async function loadPreview(act: Action, refine: boolean) {
    setBusy(refine ? "refine" : "preview");
    setError(null);
    try {
      const res = await fetch(
        `${API_BASE}api/failures/${encodeURIComponent(failureID)}/${act}/preview`,
        {
          method: "POST",
          credentials: "same-origin",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(refine ? { instruction } : {}),
        },
      );
      if (!res.ok) {
        const text = await res.text();
        throw new Error(text.trim() || `HTTP ${res.status}`);
      }
      const p = (await res.json()) as Preview;
      setPreview(p);
      setDrafts((d) => ({ ...d, [act]: p }));
      if (refine) setInstruction("");
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(null);
    }
  }

  function invalidate(act: Action) {
    setDrafts((d) => {
      const next = { ...d };
      delete next[act];
      return next;
    });
  }

  async function confirm() {
    if (!preview || !action) return;
    const act = action;
    setBusy("confirm");
    setError(null);
    try {
      const res = await fetch(`${API_BASE}api/actions/confirm`, {
        method: "POST",
        credentials: "same-origin",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ token: preview.token }),
      });
      if (!res.ok) {
        const text = await res.text();
        // A 404 means the cached draft's token expired server-side; drop it so
        // the next open regenerates.
        if (res.status === 404) {
          invalidate(act);
          setPreview(null);
          throw new Error("This draft expired. Close and reopen to regenerate it.");
        }
        throw new Error(text.trim() || `HTTP ${res.status}`);
      }
      const body = (await res.json()) as { url: string };
      setUrl(body.url);
      // The token is single-use; drop the draft so a repeat action regenerates.
      invalidate(act);
      close();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(null);
    }
  }

  function close() {
    setAction(null);
    setPreview(null);
    setInstruction("");
    setError(null);
  }

  async function submitResolve() {
    setResolveBusy(true);
    setResolveError(null);
    try {
      const res = await fetch(
        `${API_BASE}api/failures/${encodeURIComponent(failureID)}/resolve`,
        {
          method: "POST",
          credentials: "same-origin",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ note: note.trim() }),
        },
      );
      if (!res.ok) {
        const text = await res.text();
        throw new Error(text.trim() || `HTTP ${res.status}`);
      }
      setResolveOpen(false);
      setNote("");
      refetchResolved();
    } catch (e) {
      setResolveError(e instanceof Error ? e.message : String(e));
    } finally {
      setResolveBusy(false);
    }
  }

  async function unresolve() {
    setResolveBusy(true);
    setResolveError(null);
    try {
      const res = await fetch(
        `${API_BASE}api/failures/${encodeURIComponent(failureID)}/unresolve`,
        { method: "POST", credentials: "same-origin" },
      );
      if (!res.ok) {
        const text = await res.text();
        throw new Error(text.trim() || `HTTP ${res.status}`);
      }
      refetchResolved();
    } catch (e) {
      setResolveError(e instanceof Error ? e.message : String(e));
    } finally {
      setResolveBusy(false);
    }
  }

  const isFix = action === "propose-fix";
  const isResolved = !!resolved.resolved[failureID];

  return (
    <Box>
      <Stack direction="row" spacing={1} sx={{ alignItems: "center", flexWrap: "wrap", rowGap: 1 }}>
        <Button
          size="small"
          variant="outlined"
          color="warning"
          startIcon={<BugReport sx={{ fontSize: 18 }} />}
          disabled={action !== null}
          onClick={() => open("create-issue")}
        >
          File issue
        </Button>
        <Button
          size="small"
          variant="outlined"
          color="warning"
          startIcon={<Build sx={{ fontSize: 18 }} />}
          disabled={action !== null}
          onClick={() => open("propose-fix")}
        >
          Propose fix
        </Button>
        {isResolved ? (
          <Button
            size="small"
            variant="outlined"
            color="success"
            startIcon={<Replay sx={{ fontSize: 18 }} />}
            disabled={resolveBusy}
            onClick={unresolve}
          >
            Unresolve
          </Button>
        ) : (
          <Button
            size="small"
            variant="outlined"
            color="success"
            startIcon={<CheckCircleOutlined sx={{ fontSize: 18 }} />}
            disabled={resolveBusy}
            onClick={() => {
              setResolveError(null);
              setResolveOpen(true);
            }}
          >
            Mark resolved
          </Button>
        )}
      </Stack>

      {resolveError && (
        <Alert severity="error" sx={{ mt: 1 }}>
          <Typography variant="body2">{resolveError}</Typography>
        </Alert>
      )}

      <Dialog
        open={resolveOpen}
        onClose={resolveBusy ? undefined : () => setResolveOpen(false)}
        maxWidth="sm"
        fullWidth
        slotProps={{ paper: { sx: dialogPaperSx } }}
      >
        <DialogHeader
          icon={<CheckCircleOutlined sx={{ fontSize: 20 }} />}
          accent="success"
          title="Mark pattern resolved"
        />
        <DialogContent dividers>
          <Typography variant="body2" color="text.secondary" sx={{ mb: 2 }}>
            Hides this recurring pattern from the active view. It re-appears
            automatically if a newer failing build recurs.
          </Typography>
          <TextField
            label="Note (optional)"
            placeholder="e.g. fixed by kubernetes/test-infra #12345"
            fullWidth
            multiline
            minRows={2}
            size="small"
            value={note}
            disabled={resolveBusy}
            onChange={(e) => setNote(e.target.value)}
          />
        </DialogContent>
        <DialogActions sx={{ px: 3, py: 2 }}>
          <Button onClick={() => setResolveOpen(false)} disabled={resolveBusy} color="inherit">
            Cancel
          </Button>
          <Button
            variant="contained"
            color="success"
            disableElevation
            disabled={resolveBusy}
            startIcon={resolveBusy ? <CircularProgress size={16} color="inherit" /> : undefined}
            onClick={submitResolve}
          >
            Mark resolved
          </Button>
        </DialogActions>
      </Dialog>

      {url && (
        <Alert severity="success" sx={{ mt: 1 }}>
          Opened:{" "}
          <Link href={url} target="_blank" rel="noopener">
            {url}
          </Link>
        </Alert>
      )}

      <Dialog
        open={action !== null}
        onClose={busy ? undefined : close}
        maxWidth="md"
        fullWidth
        slotProps={{ paper: { sx: dialogPaperSx } }}
      >
        <DialogHeader
          icon={isFix ? <Build sx={{ fontSize: 20 }} /> : <BugReport sx={{ fontSize: 20 }} />}
          accent="warning"
          title={isFix ? "Review draft fix PR" : "Review issue"}
          subtitle={`Review the exact ${isFix ? "pull request" : "issue"} before it is opened on GitHub`}
        />
        <DialogContent dividers sx={{ px: 3, py: 2.5 }}>
          {busy === "preview" && (
            <Stack direction="row" spacing={1.5} sx={{ alignItems: "center", py: 3 }}>
              <CircularProgress size={20} />
              <Typography variant="body2" color="text.secondary">
                {isFix
                  ? "Generating a fix from the failure artifacts. This can take a minute…"
                  : "Preparing the issue draft…"}
              </Typography>
            </Stack>
          )}

          {error && (
            <Alert severity="error" variant="outlined" sx={{ mb: 2, borderRadius: "10px" }}>
              <Typography variant="body2">{error}</Typography>
            </Alert>
          )}

          {preview && busy !== "preview" && (
            <Stack spacing={2.5}>
              <Box>
                <Typography sx={sectionLabelSx}>Title</Typography>
                <Box
                  sx={{
                    borderRadius: "10px",
                    border: "1px solid",
                    borderColor: "divider",
                    bgcolor: (t) => (t.vars ?? t).palette.surface.containerLow,
                    px: 1.75,
                    py: 1.25,
                  }}
                >
                  <Typography variant="body1" sx={{ fontWeight: 600, wordBreak: "break-word" }}>
                    {preview.title}
                  </Typography>
                </Box>
              </Box>

              <Box>
                <Typography sx={sectionLabelSx}>
                  {preview.kind === "fix" ? "Description" : "Body"}
                </Typography>
                <Box sx={{ ...previewBoxSx, maxHeight: 340, overflowY: "auto" }}>
                  {stripComments(preview.body) || "(no description)"}
                </Box>
              </Box>

              {preview.diff && (
                <Box>
                  <Typography sx={sectionLabelSx}>Proposed diff</Typography>
                  <Box component="pre" sx={{ ...previewBoxSx, m: 0, maxHeight: 320, overflow: "auto" }}>
                    {preview.diff}
                  </Box>
                </Box>
              )}

              <Box>
                <TextField
                  label="Refine this draft with a prompt (optional)"
                  placeholder={
                    isFix
                      ? "e.g. patch the kustomize base instead of the rendered template"
                      : "e.g. mention this also affects the IPv6 flavor"
                  }
                  fullWidth
                  multiline
                  minRows={2}
                  size="small"
                  value={instruction}
                  disabled={busy !== null}
                  onChange={(e) => setInstruction(e.target.value)}
                />
                <Button
                  size="small"
                  variant="outlined"
                  color="primary"
                  sx={{ mt: 1.25 }}
                  startIcon={busy === "refine" ? <CircularProgress size={14} color="inherit" /> : undefined}
                  disabled={busy !== null || instruction.trim() === "" || action === null}
                  onClick={() => action && loadPreview(action, true)}
                >
                  {busy === "refine" ? "Regenerating…" : "Regenerate with prompt"}
                </Button>
              </Box>
            </Stack>
          )}
        </DialogContent>
        <DialogActions sx={{ px: 3, py: 2 }}>
          <Button onClick={close} disabled={busy !== null} color="inherit">
            Cancel
          </Button>
          <Button
            variant="contained"
            color="warning"
            disableElevation
            startIcon={
              busy === "confirm" ? (
                <CircularProgress size={16} color="inherit" />
              ) : isFix ? (
                <Build sx={{ fontSize: 18 }} />
              ) : (
                <BugReport sx={{ fontSize: 18 }} />
              )
            }
            disabled={busy !== null || !preview}
            onClick={confirm}
          >
            {isFix ? "Open draft PR" : "File issue"}
          </Button>
        </DialogActions>
      </Dialog>
    </Box>
  );
}
