import { Fragment, useLayoutEffect, useRef, useState, type ReactNode } from "react";
import Alert from "@mui/material/Alert";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import CircularProgress from "@mui/material/CircularProgress";
import Dialog from "@mui/material/Dialog";
import DialogActions from "@mui/material/DialogActions";
import DialogContent from "@mui/material/DialogContent";
import TextField from "@mui/material/TextField";
import Typography from "@mui/material/Typography";
import { Replay, TaskAltOutlined } from "@mui/icons-material";
import { DialogHeader } from "./ActionDialog";
import { dialogGutter, dialogPaperSx, overviewTypography } from "../theme/overview";
import { useAuth } from "../hooks/useAuth";
import { useCapabilities } from "../hooks/useCapabilities";
import { causeResolutionAvailable, reopenFailure, resolveFailure } from "../lib/resolution";
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
  appearance = "bar",
  onResolvedChange,
}: {
  signature?: string;
  resolvedEntry?: ResolvedEntry;
  resolvable: boolean;
  // "bar" is an outlined control sized to sit beside the cause's other action;
  // "inline" is a bare text control for callers that already own their row.
  appearance?: "bar" | "inline";
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
  if (!causeResolutionAvailable({
    actionsEnabled: Boolean(features.actions),
    authenticated: status === "authenticated",
    signature,
    resolvable,
    resolved,
  })) {
    return null;
  }
  const cause = signature as string;

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

  const bar = appearance === "bar";
  // In the bar the control sits beside the cause's other action, so it matches
  // that button's weight and never shrinks: the representative-failure label is
  // the one that gives up width when the row is tight.
  const buttonSx = bar
    ? {
        textTransform: "none" as const,
        flexShrink: 0,
        minHeight: { xs: 44, sm: 32 },
        ...overviewTypography.secondaryBody,
        fontWeight: 650,
      }
    : { textTransform: "none" as const, px: 0, minHeight: { xs: 44, sm: 36 } };

  // The bar is a flex row that the control's parts join directly, so it stays a
  // fragment. Inline callers place it in a row of their own that neither wraps
  // nor stacks, so there it owns a column wrapper and the alert lands beneath
  // the button rather than beside it.
  const Wrapper = bar ? Fragment : InlineWrapper;

  return (
    <Wrapper>
      {resolved ? (
        <Button
          size="small"
          variant={bar ? "outlined" : "text"}
          color="primary"
          startIcon={<Replay sx={{ fontSize: 18 }} />}
          disabled={busy}
          onClick={() => submit(() => reopenFailure("cause", cause))}
          sx={buttonSx}
        >
          Reopen failure
        </Button>
      ) : (
        <Button
          size="small"
          variant={bar ? "outlined" : "text"}
          color="primary"
          startIcon={<TaskAltOutlined sx={{ fontSize: 18 }} />}
          disabled={busy}
          onClick={() => {
            setError(null);
            setOpen(true);
          }}
          sx={buttonSx}
        >
          Resolve failure
        </Button>
      )}
      {resolvedEntry && bar && (
        <Typography variant="caption" color="textSecondary" sx={{ minWidth: 0 }}>
          Resolved by {resolvedEntry.resolved_by}
          {resolvedEntry.note ? `. ${resolvedEntry.note}` : ""}
        </Typography>
      )}

      {error && (
        // In the bar the alert is a flex item beside the controls, so a full
        // width forces it onto its own wrapped line. Inline it already has one.
        <Alert severity="error" sx={{ mt: 1, ...(bar && { width: "100%" }) }}>
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
    </Wrapper>
  );
}

// InlineWrapper stacks the control's parts for callers that place it in a row
// which neither wraps nor stacks. The surrounding row still decides where the
// column sits; this only keeps the alert off the button's line.
function InlineWrapper({ children }: { children: ReactNode }) {
  return (
    <Box sx={{ display: "flex", flexDirection: "column", minWidth: 0 }}>{children}</Box>
  );
}
