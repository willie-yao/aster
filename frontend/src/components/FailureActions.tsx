import {
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
  type ReactNode,
} from "react";
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
import {
  BugReport,
  Build,
  GitHub,
  CheckCircleOutlined,
  Replay,
} from "@mui/icons-material";
import { useCapabilities } from "../hooks/useCapabilities";
import { useAuth } from "../hooks/useAuth";
import { useResolved } from "../hooks/useData";
import { soft } from "../theme";
import { useSearchParams } from "react-router-dom";
import { ActionDraftPreview } from "./ActionDraftPreview";
import type {
  Action,
  ActionRequest,
  ActionPreview,
} from "../types/actions";
import {
  actionErrorMessage,
  actionRequestCanConfirm,
  actionRequestIsActive,
  actionRequestIsPollable,
  actionRequestIsRecoverable,
  actionRequestStorageOwner,
  cancelActionRequest,
  loadLatestActionRequest,
  readStoredActionRequestID,
  syncStoredActionRequest,
} from "../lib/actionRequests";

function requestedAction(value: string | null): Action | null {
  return value === "create-issue" || value === "propose-fix" ? value : null;
}

function requestStateError(request: ActionRequest): string | null {
  if (request.status === "failed" && !request.warning) {
    return request.error || "Draft generation failed.";
  }
  if (request.status === "expired") return "This draft expired.";

  return null;
}

const API_BASE = import.meta.env.BASE_URL;

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
    <DialogTitle
      sx={{ display: "flex", alignItems: "center", gap: 1.5, px: 3, py: 2.25 }}
    >
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
        <Typography
          variant="headline"
          component="span"
          sx={{ display: "block", fontSize: "1.125rem", lineHeight: 1.2 }}
        >
          {title}
        </Typography>
        {subtitle && (
          <Typography
            variant="caption"
            color="text.secondary"
            sx={{ display: "block", mt: 0.25 }}
          >
            {subtitle}
          </Typography>
        )}
      </Box>
    </DialogTitle>
  );
}

export function FailureActions({ failureID, resolvable = true }: { failureID: string; resolvable?: boolean }) {
  const { features } = useCapabilities();
  const { status, signIn, login, mode } = useAuth();
  const [searchParams, setSearchParams] = useSearchParams();
  const linkedFailure = searchParams.get("failure");
  const linkedAction = requestedAction(searchParams.get("action"));
  const [reviewIntent, setReviewIntent] = useState<Action | null>(null);
  const [action, setAction] = useState<Action | null>(null);
  const [busy, setBusy] = useState<
    "preview" | "refine" | "confirm" | "cancel" | null
  >(null);
  const [request, setRequest] = useState<ActionRequest | null>(null);
  const [preview, setPreview] = useState<ActionPreview | null>(null);
  const [instruction, setInstruction] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [url, setUrl] = useState<string | null>(null);
  const requestID = request?.id;
  const requestStatus = request?.status;
  const activeFailureID = useRef(failureID);
  const activeRequestID = useRef<string | undefined>(requestID);
  const activeAction = useRef<Action | null>(null);
  const storageOwner = actionRequestStorageOwner(login, mode);

  const { data: resolved, refetch: refetchResolved } = useResolved();
  const [resolveOpen, setResolveOpen] = useState(false);
  const [note, setNote] = useState("");
  const [resolveBusy, setResolveBusy] = useState(false);
  const [resolveError, setResolveError] = useState<string | null>(null);

  useLayoutEffect(() => {
    const failureChanged = activeFailureID.current !== failureID;
    activeFailureID.current = failureID;
    activeRequestID.current = requestID;
    activeAction.current = failureChanged ? null : action;
  }, [action, failureID, requestID]);

  // Reset action state if this component is reused for a different failure.
  useEffect(() => {
    setReviewIntent(null);
    setAction(null);
    setBusy(null);
    setRequest(null);
    setPreview(null);
    setInstruction("");
    setError(null);
    setUrl(null);
  }, [failureID]);

  // Email action links are inert GETs. After authentication, they open a local
  // intent dialog that requires an explicit click before a request is created.
  useEffect(() => {
    if (
      !features.action_requests ||
      status !== "authenticated" ||
      linkedFailure !== failureID ||
      !linkedAction
    ) {
      return;
    }
    setReviewIntent(linkedAction);
    const next = new URLSearchParams(searchParams);
    next.delete("failure");
    next.delete("action");
    setSearchParams(next, { replace: true });
  }, [
    failureID,
    features.action_requests,
    linkedAction,
    linkedFailure,
    searchParams,
    setSearchParams,
    status,
  ]);

  useEffect(() => {
    if (
      !features.action_requests ||
      status !== "authenticated" ||
      !storageOwner
    ) {
      return;
    }
    const owner = storageOwner;
    const expectedOwner = login?.trim().toLowerCase();
    const stored = (["create-issue", "propose-fix"] as const)
      .map((kind) => ({
        kind,
        id: readStoredActionRequestID(
          window.sessionStorage,
          owner,
          failureID,
          kind,
        ),
      }))
      .filter((candidate): candidate is { kind: Action; id: string } =>
        Boolean(candidate.id),
      );
    if (stored.length === 0) return;

    let cancelled = false;
    async function recover() {
      const recovered = await Promise.all(
        stored.map(async ({ kind, id }) => {
          try {
            const value = await loadLatestActionRequest(API_BASE, id);
            if (
              value.failure_id !== failureID ||
              value.kind !== kind ||
              (expectedOwner &&
                value.owner.trim().toLowerCase() !== expectedOwner)
            ) {
              return null;
            }
            syncStoredActionRequest(
              window.sessionStorage,
              owner,
              value,
            );
            return actionRequestIsRecoverable(value.status) ? value : null;
          } catch {
            return null;
          }
        }),
      );
      if (cancelled || activeAction.current !== null) return;
      const latest = recovered
        .filter((value): value is ActionRequest => value !== null)
        .sort((left, right) => right.updated_at.localeCompare(left.updated_at))[0];
      if (!latest) return;
      activeAction.current = latest.kind;
      setAction(latest.kind);
      setRequest(latest);
      setPreview(latest.preview ?? null);
      setError(requestStateError(latest));
    }
    void recover();
    return () => {
      cancelled = true;
    };
  }, [failureID, features.action_requests, login, status, storageOwner]);

  useEffect(() => {
    if (
      !features.action_requests ||
      status !== "authenticated" ||
      !storageOwner ||
      !request ||
      request.failure_id !== failureID
    ) {
      return;
    }
    syncStoredActionRequest(window.sessionStorage, storageOwner, request);
  }, [failureID, features.action_requests, request, status, storageOwner]);

  useEffect(() => {
    if (
      !features.action_requests ||
      status !== "authenticated" ||
      !requestID ||
      !requestStatus ||
      !actionRequestIsPollable(requestStatus)
    ) {
      return;
    }
    const activeRequestID = requestID;
    let cancelled = false;
    let timer: number | undefined;
    let retryCount = 0;
    async function load() {
      try {
        const res = await fetch(
          `${API_BASE}api/action-requests/${encodeURIComponent(activeRequestID)}`,
          { credentials: "same-origin", cache: "no-store" },
        );
        if (!res.ok) {
          const message = await actionErrorMessage(res);
          if (cancelled) return;
          setError(message);
          if (res.status >= 500) {
            const delay = Math.min(10_000, 1000 * 2 ** retryCount++);
            timer = window.setTimeout(load, delay);
          }
          return;
        }
        const value = (await res.json()) as ActionRequest;
        const latest = value.superseded_by
          ? await loadLatestActionRequest(API_BASE, value.id)
          : value;
        if (cancelled) return;
        retryCount = 0;
        setAction((current) => (current === null ? null : latest.kind));
        setRequest(latest);
        setPreview(latest.preview ?? null);
        setError(requestStateError(latest));
        if (actionRequestIsPollable(latest.status)) {
          timer = window.setTimeout(load, 2000);
        }
      } catch (e) {
        if (cancelled) return;
        setError(e instanceof Error ? e.message : String(e));
        const delay = Math.min(10_000, 1000 * 2 ** retryCount++);
        timer = window.setTimeout(load, delay);
      }
    }
    timer = window.setTimeout(load, 1200);
    return () => {
      cancelled = true;
      if (timer !== undefined) window.clearTimeout(timer);
    };
  }, [features.action_requests, requestID, requestStatus, status]);

  if (
    !features.actions ||
    !failureID ||
    status === "loading" ||
    status === "unavailable"
  ) {
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
        {features.action_requests && linkedFailure === failureID && linkedAction
          ? `Sign in to review ${linkedAction === "propose-fix" ? "a fix proposal" : "an issue draft"}`
          : features.action_requests
            ? "Sign in to file issues or fixes"
            : "Sign in to manage this failure"}
      </Button>
    );
  }

  function dismissReviewIntent() {
    setReviewIntent(null);
  }

  async function refreshRequestState(
    id: string,
    startedFailureID: string,
  ): Promise<ActionRequest | null> {
    try {
      const value = await loadLatestActionRequest(API_BASE, id);
      if (
        activeFailureID.current !== startedFailureID ||
        (activeRequestID.current !== undefined && activeRequestID.current !== id)
      ) return null;
      activeAction.current = value.kind;
      setAction(value.kind);
      setRequest(value);
      setPreview(value.preview ?? null);
      return value;
    } catch {
      return null;
    }
  }

  function handleRecoveredReplacement(
    recovered: ActionRequest | null,
    previousID: string,
  ): boolean {
    if (!recovered || recovered.id === previousID) return false;
    if (recovered.status === "confirmed" && recovered.result_url) {
      setUrl(recovered.result_url);
      close();
      return true;
    }
    setError(requestStateError(recovered));
    return true;
  }

  async function startRequest(
    requested: Action,
    prompt = "",
    previousRequestID?: string,
  ) {
    if (!features.action_requests) {
      activeAction.current = requested;
      setAction(requested);
      setError("Background draft generation is unavailable on this deployment.");
      return;
    }
    const startedFailureID = failureID;
    activeAction.current = requested;
    setAction(requested);
    setBusy(prompt ? "refine" : "preview");
    setError(null);
    setUrl(null);
    try {
      const res = await fetch(
        `${API_BASE}api/failures/${encodeURIComponent(failureID)}/${requested}/requests`,
        {
          method: "POST",
          credentials: "same-origin",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            ...(prompt.trim() ? { instruction: prompt.trim() } : {}),
            ...(previousRequestID
              ? { supersedes_request_id: previousRequestID }
              : {}),
          }),
        },
      );
      if (!res.ok) throw new Error(await actionErrorMessage(res));
      const value = (await res.json()) as ActionRequest;
      if (activeFailureID.current !== startedFailureID) return;
      setRequest(value);
      setPreview(value.preview ?? null);
      setInstruction("");
    } catch (e) {
      const message = e instanceof Error ? e.message : String(e);
      const refreshed = previousRequestID
        ? await refreshRequestState(previousRequestID, startedFailureID)
        : null;
      if (activeFailureID.current !== startedFailureID) return;
      if (
        previousRequestID &&
        handleRecoveredReplacement(refreshed, previousRequestID)
      ) {
        return;
      }
      setError(message);
    } finally {
      if (activeFailureID.current === startedFailureID) setBusy(null);
    }
  }

  async function generateRequestedDraft() {
    if (!reviewIntent) return;
    const requested = reviewIntent;
    setReviewIntent(null);
    await startRequest(requested);
  }

  function open(requested: Action) {
    setInstruction("");
    setError(null);
    const activeRequest = request && actionRequestIsActive(request) ? request : null;
    if (activeRequest?.status === "unknown") {
      activeAction.current = activeRequest.kind;
      setAction(activeRequest.kind);
      setPreview(activeRequest.preview ?? null);
      return;
    }
    if (activeRequest?.kind === requested) {
      activeAction.current = requested;
      setAction(requested);
      setPreview(activeRequest.preview ?? null);
      return;
    }
    setRequest(null);
    setPreview(null);
    void startRequest(requested, "", activeRequest?.id);
  }

  async function confirm() {
    if (
      !action ||
      !request ||
      !actionRequestCanConfirm(request.status, Boolean(preview))
    ) {
      return;
    }
    const startedFailureID = failureID;
    setBusy("confirm");
    setError(null);
    try {
      const res = await fetch(
        `${API_BASE}api/action-requests/${encodeURIComponent(request.id)}/confirm`,
        { method: "POST", credentials: "same-origin" },
      );
      if (!res.ok) throw new Error(await actionErrorMessage(res));
      const body = (await res.json()) as { url: string };
      if (activeFailureID.current !== startedFailureID) return;
      setUrl(body.url);
      setRequest({ ...request, status: "confirmed", result_url: body.url });
      close();
    } catch (e) {
      const message = e instanceof Error ? e.message : String(e);
      const refreshed = await refreshRequestState(request.id, startedFailureID);
      if (activeFailureID.current !== startedFailureID) return;
      if (handleRecoveredReplacement(refreshed, request.id)) return;
      if (refreshed?.status === "confirmed" && refreshed.result_url) {
        setUrl(refreshed.result_url);
        close();
        return;
      }
      setError(message);
    } finally {
      if (activeFailureID.current === startedFailureID) setBusy(null);
    }
  }

  async function cancelRequest() {
    if (!request || request.status === "cancelling") return;
    const startedFailureID = failureID;
    const startedRequestID = request.id;
    setBusy("cancel");
    setError(null);
    try {
      const value = await cancelActionRequest(API_BASE, request.id);
      const latest = value.superseded_by
        ? await loadLatestActionRequest(API_BASE, value.id)
        : value;
      if (activeFailureID.current !== startedFailureID || activeRequestID.current !== startedRequestID) return;
      activeAction.current = latest.kind;
      setAction(latest.kind);
      setRequest(latest);
      setPreview(latest.preview ?? null);
      setError(requestStateError(latest));
    } catch (e) {
      if (activeRequestID.current !== startedRequestID) return;
      const message = e instanceof Error ? e.message : String(e);
      const refreshed = await refreshRequestState(request.id, startedFailureID);
      if (activeFailureID.current !== startedFailureID) return;
      if (handleRecoveredReplacement(refreshed, request.id)) return;
      setError(
        refreshed?.status === "cancelled" || refreshed?.status === "cancelling"
          ? null
          : message,
      );
    } finally {
      if (activeFailureID.current === startedFailureID) setBusy(null);
    }
  }

  function close() {
    activeAction.current = null;
    setAction(null);
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
        throw new Error(await actionErrorMessage(res));
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
        throw new Error(await actionErrorMessage(res));
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
      <Stack
        direction="row"
        spacing={1}
        sx={{ alignItems: "center", flexWrap: "wrap", rowGap: 1 }}
      >
        {features.action_requests && (
          <>
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
          </>
        )}
        {resolvable && (isResolved ? (
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
        ))}
      </Stack>

      {resolvable && resolveError && (
        <Alert severity="error" sx={{ mt: 1 }}>
          <Typography variant="body2">{resolveError}</Typography>
        </Alert>
      )}

      <Dialog
        open={reviewIntent !== null && action === null}
        onClose={dismissReviewIntent}
        maxWidth="sm"
        fullWidth
        slotProps={{ paper: { sx: dialogPaperSx } }}
      >
        <DialogHeader
          icon={
            reviewIntent === "propose-fix" ? (
              <Build sx={{ fontSize: 20 }} />
            ) : (
              <BugReport sx={{ fontSize: 20 }} />
            )
          }
          accent="warning"
          title={
            reviewIntent === "propose-fix"
              ? "Generate a fix proposal?"
              : "Generate an issue draft?"
          }
        />
        <DialogContent dividers>
          <Typography variant="body2" color="text.secondary">
            Opening the email link did not create anything. Generate a draft
            now, then review the exact content before confirming any GitHub
            write.
          </Typography>
          {features.action_requests && (
            <Typography variant="body2" color="text.secondary" sx={{ mt: 1.5 }}>
              Generation continues in the background. If draft-ready email is
              configured, the dashboard emails you when the draft is ready.
            </Typography>
          )}
        </DialogContent>
        <DialogActions sx={{ px: 3, py: 2 }}>
          <Button
            onClick={dismissReviewIntent}
            color="inherit"
          >
            Cancel
          </Button>
          <Button
            variant="contained"
            color="warning"
            disableElevation
            onClick={generateRequestedDraft}
          >
            Generate draft
          </Button>
        </DialogActions>
      </Dialog>

      <Dialog
        open={resolvable && resolveOpen}
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
          <Button
            onClick={() => setResolveOpen(false)}
            disabled={resolveBusy}
            color="inherit"
          >
            Cancel
          </Button>
          <Button
            variant="contained"
            color="success"
            disableElevation
            disabled={resolveBusy}
            startIcon={
              resolveBusy ? (
                <CircularProgress size={16} color="inherit" />
              ) : undefined
            }
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
        onClose={busy !== null ? undefined : close}
        maxWidth="md"
        fullWidth
        slotProps={{ paper: { sx: dialogPaperSx } }}
      >
        <DialogHeader
          icon={
            isFix ? (
              <Build sx={{ fontSize: 20 }} />
            ) : (
              <BugReport sx={{ fontSize: 20 }} />
            )
          }
          accent="warning"
          title={isFix ? "Review draft fix PR" : "Review issue"}
          subtitle={`Review the exact ${isFix ? "pull request" : "issue"} before it is opened on GitHub`}
        />
        <DialogContent dividers sx={{ px: 3, py: 2.5 }}>
          {(busy === "preview" || request?.status === "pending") && (
            <Box>
              <Stack
                direction="row"
                spacing={1.5}
                sx={{ alignItems: "center", py: 2 }}
              >
                <CircularProgress size={20} />
                <Box>
                  <Typography variant="body2" sx={{ fontWeight: 600 }}>
                    {isFix ? "Generating the fix proposal" : "Preparing the issue draft"}
                  </Typography>
                  <Typography variant="body2" color="text.secondary">
                    Generation continues in the background. You can close this
                    dialog. If draft-ready email is configured, you can also
                    return from the email.
                  </Typography>
                </Box>
              </Stack>
              {request && (
                <Button
                  size="small"
                  variant="outlined"
                  color="inherit"
                  disabled={busy === "cancel"}
                  onClick={cancelRequest}
                >
                  {busy === "cancel" ? "Cancelling…" : "Cancel request"}
                </Button>
              )}
            </Box>
          )}

          {request?.status === "cancelling" && (
            <Box
              role="status"
              sx={{
                borderRadius: "12px",
                bgcolor: (theme) =>
                  (theme.vars ?? theme).palette.surface.containerLow,
                border: "1px solid",
                borderColor: "divider",
                p: 2,
              }}
            >
              <Stack direction="row" spacing={1.5} sx={{ alignItems: "center" }}>
                <CircularProgress size={20} />
                <Box>
                  <Typography variant="body2" sx={{ fontWeight: 600 }}>
                    Stopping runtime work
                  </Typography>
                  <Typography variant="body2" color="text.secondary">
                    This request remains active until the server confirms that
                    its runtime work has stopped.
                  </Typography>
                </Box>
              </Stack>
            </Box>
          )}

          {error && (
            <Alert
              severity="error"
              variant="outlined"
              sx={{ mb: 2, borderRadius: "10px" }}
            >
              <Typography variant="body2">{error}</Typography>
            </Alert>
          )}
          {request?.warning && (
            <Alert
              severity="warning"
              variant="outlined"
              sx={{ mb: 2, borderRadius: "10px" }}
            >
              <Typography variant="body2">{request.warning}</Typography>
            </Alert>
          )}

          {request?.status === "cancelled" && (
            <Alert severity="info">This request was cancelled.</Alert>
          )}
          {request?.status === "unknown" && (
            <Alert severity="warning">GitHub may have accepted this action. Use Check GitHub result; do not regenerate or cancel it.</Alert>
          )}

          {preview &&
            (request?.status === "ready" ||
              request?.status === "unknown" ||
              (request?.status === "failed" && Boolean(request.warning))) && (
            <Stack spacing={2.5}>
              <ActionDraftPreview preview={preview} />
              {request.status === "ready" && <Box>
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
                  onChange={(event) => setInstruction(event.target.value)}
                />
                <Button
                  size="small"
                  variant="outlined"
                  color="primary"
                  sx={{ mt: 1.25 }}
                  startIcon={
                    busy === "refine" ? (
                      <CircularProgress size={14} color="inherit" />
                    ) : undefined
                  }
                  disabled={
                    busy !== null ||
                    instruction.trim() === "" ||
                    action === null
                  }
                  onClick={() =>
                    action &&
                    void startRequest(action, instruction, request.id)
                  }
                >
                  {busy === "refine"
                    ? "Regenerating…"
                    : "Regenerate with prompt"}
                </Button>
              </Box>}
            </Stack>
          )}
        </DialogContent>
        <DialogActions sx={{ px: 3, py: 2 }}>
          <Button
            onClick={close}
            disabled={busy !== null}
            color="inherit"
          >
            Close
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
            disabled={
              busy !== null ||
              !request ||
              !actionRequestCanConfirm(request.status, Boolean(preview))
            }
            onClick={confirm}
          >
            {request?.status === "unknown" ? "Check GitHub result" : isFix ? "Open draft PR" : "File issue"}
          </Button>
        </DialogActions>
      </Dialog>
    </Box>
  );
}
