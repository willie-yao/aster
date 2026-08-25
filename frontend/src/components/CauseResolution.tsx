import { useLayoutEffect, useRef, useState } from "react";
import Alert from "@mui/material/Alert";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import CircularProgress from "@mui/material/CircularProgress";
import Dialog from "@mui/material/Dialog";
import DialogActions from "@mui/material/DialogActions";
import DialogContent from "@mui/material/DialogContent";
import Stack from "@mui/material/Stack";
import TextField from "@mui/material/TextField";
import Typography from "@mui/material/Typography";
import { Replay, TaskAltOutlined } from "@mui/icons-material";
import { DialogHeader } from "./ActionDialog";
import { dialogGutter, dialogPaperSx } from "../theme/overview";
import { useAuth } from "../hooks/useAuth";
import { useCapabilities } from "../hooks/useCapabilities";
import { reopenFailure, resolveFailure } from "../lib/resolution";
import type { ResolvedEntry } from "../types/dashboard";

// CauseResolution acknowledges one cause of a recurring pattern without touching
// its siblings. resolvable gates starting a NEW resolution; reopening stays
// available whenever the cause reads as resolved, so a cause cannot be stranded
// in the resolved state once it no longer qualifies for a fresh resolution.
//
// resolvedEntry and onResolvedChange come from the owner, which holds the single
// copy of resolved state. Reading it here too would let the two disagree when
// their independent fetches diverge.
export function CauseResolution({
  signature,
  resolvedEntry,
  resolvable,
  appearance = "card",
  onResolvedChange,
}: {
  signature?: string;
  resolvedEntry?: ResolvedEntry;
  resolvable: boolean;
  // "card" separates the control from the cause body above it; "inline" drops
  // that separator for callers that already sit in their own row.
  appearance?: "card" | "inline";
  onResolvedChange: () => void;
}) {
  const { features } = useCapabilities();
  const { status } = useAuth();
  const [open, setOpen] = useState(false);
  const [note, setNote] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // A resolution write outlives the cause it started on, so its result is
  // applied only while this component still shows that cause. The refresh itself
  // always runs: the write landed regardless of which cause is shown now.
  const active = useRef(signature);

  useLayoutEffect(() => {
    if (active.current === signature) return;
    active.current = signature;
    // busy is reset here too: the in-flight write's finally clause is keyed on
    // the signature it started under, so it will not clear it once the
    // component has moved to a different cause.
    setBusy(false);
    setOpen(false);
    setNote("");
    setError(null);
  }, [signature]);

  const resolved = Boolean(resolvedEntry);
  if (!features.actions || status !== "authenticated" || !signature || (!resolvable && !resolved)) {
    return null;
  }
  const cause = signature;

  async function submit(run: () => Promise<void>, onDone?: () => void) {
    setBusy(true);
    setError(null);
    try {
      await run();
      onResolvedChange();
      if (active.current !== cause) return;
      onDone?.();
    } catch (e) {
      if (active.current !== cause) return;
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      if (active.current === cause) setBusy(false);
    }
  }

  return (
    <Box sx={appearance === "card" ? { mt: 1.5, pt: 1.5, borderTop: "1px solid", borderColor: "divider" } : undefined}>
      <Stack direction="row" spacing={1.5} sx={{ alignItems: "center", flexWrap: "wrap", rowGap: 0.5 }}>
        {resolved ? (
          <Button
            size="small"
            variant="text"
            color="primary"
            startIcon={<Replay sx={{ fontSize: 18 }} />}
            disabled={busy}
            onClick={() => submit(() => reopenFailure("cause", cause))}
            sx={{ textTransform: "none", px: 0, minHeight: { xs: 44, sm: 36 } }}
          >
            Reopen failure
          </Button>
        ) : (
          <Button
            size="small"
            variant="text"
            color="primary"
            startIcon={<TaskAltOutlined sx={{ fontSize: 18 }} />}
            disabled={busy}
            onClick={() => {
              setError(null);
              setOpen(true);
            }}
            sx={{ textTransform: "none", px: 0, minHeight: { xs: 44, sm: 36 } }}
          >
            Resolve failure
          </Button>
        )}
        {resolvedEntry && appearance === "card" && (
          <Typography variant="caption" color="textSecondary" sx={{ minWidth: 0 }}>
            Resolved by {resolvedEntry.resolved_by}
            {resolvedEntry.note ? `. ${resolvedEntry.note}` : ""}
          </Typography>
        )}
      </Stack>

      {error && (
        <Alert severity="error" sx={{ mt: 1 }}>
          <Typography variant="body2">{error}</Typography>
        </Alert>
      )}

      <Dialog
        open={open}
        onClose={busy ? undefined : () => setOpen(false)}
        maxWidth="sm"
        fullWidth
        slotProps={{ paper: { sx: dialogPaperSx } }}
      >
        <DialogHeader
          icon={<TaskAltOutlined sx={{ fontSize: 18 }} />}
          accent="primary"
          title="Resolve failure"
        />
        <DialogContent dividers sx={{ px: dialogGutter, py: 2 }}>
          <Typography variant="body2" color="textSecondary" sx={{ mb: 2 }}>
            Records that a maintainer has handled this cause and hides it from the
            active view. The other causes of this pattern are unaffected, and this
            one reappears automatically if a newer build fails the same way.
          </Typography>
          <TextField
            label="Note (optional)"
            placeholder="For example, fixed by kubernetes/test-infra #12345"
            fullWidth
            multiline
            minRows={2}
            size="small"
            value={note}
            disabled={busy}
            onChange={(e) => setNote(e.target.value)}
          />
        </DialogContent>
        <DialogActions sx={{ px: dialogGutter, py: 2 }}>
          <Button onClick={() => setOpen(false)} disabled={busy} color="inherit">
            Cancel
          </Button>
          <Button
            variant="contained"
            color="primary"
            disableElevation
            disabled={busy}
            startIcon={busy ? <CircularProgress size={16} color="inherit" /> : undefined}
            onClick={() =>
              submit(
                () => resolveFailure("cause", cause, note),
                () => {
                  setOpen(false);
                  setNote("");
                },
              )
            }
          >
            Resolve failure
          </Button>
        </DialogActions>
      </Dialog>
    </Box>
  );
}
