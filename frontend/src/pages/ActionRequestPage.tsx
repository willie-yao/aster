import { useEffect, useRef, useState, type ReactNode } from "react";
import { Link as RouterLink, useNavigate, useParams } from "react-router-dom";
import Alert from "@mui/material/Alert";
import Box from "@mui/material/Box";
import Breadcrumbs from "@mui/material/Breadcrumbs";
import Button from "@mui/material/Button";
import Chip from "@mui/material/Chip";
import CircularProgress from "@mui/material/CircularProgress";
import Link from "@mui/material/Link";
import Stack from "@mui/material/Stack";
import TextField from "@mui/material/TextField";
import Typography from "@mui/material/Typography";
import {
  BugReport,
  Build,
  CheckCircle,
  GitHub,
} from "@mui/icons-material";
import { useAuth } from "../hooks/useAuth";
import { useCapabilities } from "../hooks/useCapabilities";
import { Panel } from "../components/Panel";
import { ActionDraftPreview } from "../components/ActionDraftPreview";
import type { ActionRequest } from "../types/actions";
import {
  actionErrorMessage,
  actionRequestCanConfirm,
  actionRequestIsPollable,
  actionRequestStorageOwner,
  cancelActionRequest,
  loadLatestActionRequest,
  syncStoredActionRequest,
} from "../lib/actionRequests";
import { actionRequestPath } from "../lib/routes";
import { soft } from "../theme";

const API_BASE = import.meta.env.BASE_URL;

function ActionRequestPageFrame({
  children,
  breadcrumbs = false,
}: {
  children: ReactNode;
  breadcrumbs?: boolean;
}) {
  return (
    <Stack spacing={2.5} sx={{ maxWidth: 1040, mx: "auto" }}>
      {breadcrumbs && (
        <Breadcrumbs
          separator="›"
          sx={{ color: "text.secondary", fontSize: "0.875rem" }}
        >
          <Link component={RouterLink} to="/" color="inherit" underline="hover">
            Dashboard
          </Link>
          <Typography variant="inherit" color="text.primary">
            Draft review
          </Typography>
        </Breadcrumbs>
      )}
      <Typography variant="h4" component="h1">
        Action Request
      </Typography>
      {children}
    </Stack>
  );
}

export function ActionRequestPage() {
  const { requestID = "" } = useParams();
  const navigate = useNavigate();
  const { features } = useCapabilities();
  const { status, signIn, login, mode } = useAuth();
  const storageOwner = actionRequestStorageOwner(login, mode);
  const [request, setRequest] = useState<ActionRequest | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [confirming, setConfirming] = useState(false);
  const [cancelling, setCancelling] = useState(false);
  const [refining, setRefining] = useState(false);
  const [instruction, setInstruction] = useState("");
  const requestStatus = request?.status;
  const activeRequestID = useRef(requestID);

  useEffect(() => {
    activeRequestID.current = requestID;
  }, [requestID]);

  useEffect(() => {
    if (!features.action_requests || status !== "authenticated" || !requestID)
      return;
    let cancelled = false;
    let timer: number | undefined;
    let retryCount = 0;
    async function load() {
      try {
        const res = await fetch(
          `${API_BASE}api/action-requests/${encodeURIComponent(requestID)}`,
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
        if (latest.id !== requestID) {
          navigate(actionRequestPath(latest.id), {
            replace: true,
          });
        }
        retryCount = 0;
        setRequest(latest);
        setError(
          latest.status === "failed" && !latest.warning
            ? latest.error || "Draft generation failed."
            : null,
        );
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
    void load();
    return () => {
      cancelled = true;
      if (timer !== undefined) window.clearTimeout(timer);
    };
  }, [features.action_requests, navigate, requestID, requestStatus, status]);

  useEffect(() => {
    if (
      status !== "authenticated" ||
      !storageOwner ||
      !request
    ) {
      return;
    }
    syncStoredActionRequest(window.sessionStorage, storageOwner, request);
  }, [request, status, storageOwner]);

  async function refreshRequestState(id: string): Promise<ActionRequest | null> {
    try {
      const value = await loadLatestActionRequest(API_BASE, id);
      if (activeRequestID.current !== id) return null;
      setRequest(value);
      if (value.id !== requestID) {
        navigate(actionRequestPath(value.id), {
          replace: true,
        });
      }
      return value;
    } catch {
      return null;
    }
  }

  async function confirm() {
    if (!request || !actionRequestCanConfirm(request.status, Boolean(request.preview))) {
      return;
    }
    const startedRequestID = request.id;
    setConfirming(true);
    setError(null);
    try {
      const res = await fetch(
        `${API_BASE}api/action-requests/${encodeURIComponent(request.id)}/confirm`,
        { method: "POST", credentials: "same-origin" },
      );
      if (!res.ok) throw new Error(await actionErrorMessage(res));
      const body = (await res.json()) as { url: string };
      if (activeRequestID.current !== startedRequestID) return;
      setRequest({ ...request, status: "confirmed", result_url: body.url });
    } catch (e) {
      if (activeRequestID.current !== startedRequestID) return;
      const message = e instanceof Error ? e.message : String(e);
      const refreshed = await refreshRequestState(request.id);
      setError(
        refreshed?.status === "confirmed" ||
          (refreshed !== null && refreshed.id !== request.id)
          ? null
          : message,
      );
    } finally {
      setConfirming(false);
    }
  }

  async function cancel() {
    if (!request || request.status === "cancelling") return;
    const startedRequestID = request.id;
    setCancelling(true);
    setError(null);
    try {
      const value = await cancelActionRequest(API_BASE, request.id);
      const latest = value.superseded_by
        ? await loadLatestActionRequest(API_BASE, value.id)
        : value;
      if (activeRequestID.current !== startedRequestID) return;
      setRequest(latest);
      if (latest.id !== requestID) {
        navigate(actionRequestPath(latest.id), {
          replace: true,
        });
      }
    } catch (e) {
      if (activeRequestID.current !== startedRequestID) return;
      const message = e instanceof Error ? e.message : String(e);
      const refreshed = await refreshRequestState(request.id);
      setError(
        refreshed?.status === "cancelled" ||
          refreshed?.status === "cancelling" ||
          (refreshed !== null && refreshed.id !== request.id)
          ? null
          : message,
      );
    } finally {
      setCancelling(false);
    }
  }

  async function refine() {
    if (!request || request.status !== "ready" || instruction.trim() === "") {
      return;
    }
    const startedRequestID = request.id;
    setRefining(true);
    setError(null);
    try {
      const res = await fetch(
        `${API_BASE}api/failures/${encodeURIComponent(request.failure_id)}/${request.kind}/requests`,
        {
          method: "POST",
          credentials: "same-origin",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            instruction: instruction.trim(),
            supersedes_request_id: request.id,
          }),
        },
      );
      if (!res.ok) throw new Error(await actionErrorMessage(res));
      const next = (await res.json()) as ActionRequest;
      if (activeRequestID.current !== startedRequestID) return;
      setInstruction("");
      setRequest(next);
      navigate(actionRequestPath(next.id), { replace: true });
    } catch (e) {
      if (activeRequestID.current !== startedRequestID) return;
      const message = e instanceof Error ? e.message : String(e);
      const refreshed = await refreshRequestState(request.id);
      setError(
        refreshed !== null && refreshed.id !== request.id ? null : message,
      );
    } finally {
      setRefining(false);
    }
  }

  if (!features.action_requests) {
    return (
      <ActionRequestPageFrame>
        <Alert severity="warning">
          Asynchronous action requests are unavailable on this deployment.
        </Alert>
      </ActionRequestPageFrame>
    );
  }
  if (status === "loading") {
    return (
      <ActionRequestPageFrame>
        <CircularProgress size={24} />
      </ActionRequestPageFrame>
    );
  }
  if (status === "anonymous") {
    return (
      <ActionRequestPageFrame>
        <Panel sx={{ maxWidth: 560, mx: "auto", p: 3, textAlign: "center" }}>
          <Typography variant="h5" component="h2" sx={{ mb: 1 }}>
            Sign in to review this draft
          </Typography>
          <Typography variant="body2" color="text.secondary" sx={{ mb: 2.5 }}>
            Drafts are bound to the maintainer who requested them.
          </Typography>
          <Button variant="contained" startIcon={<GitHub />} onClick={signIn}>
            Sign in with GitHub
          </Button>
        </Panel>
      </ActionRequestPageFrame>
    );
  }
  if (error && !request) {
    return (
      <ActionRequestPageFrame>
        <Alert severity="error">{error}</Alert>
      </ActionRequestPageFrame>
    );
  }
  if (!request || request.id !== requestID) {
    return (
      <ActionRequestPageFrame>
        <CircularProgress size={24} />
      </ActionRequestPageFrame>
    );
  }

  const preview = request.preview;
  const isFix = request.kind === "propose-fix";
  const canCancel = request.status === "pending" || request.status === "ready";
  const canConfirm = actionRequestCanConfirm(request.status, Boolean(preview));

  return (
    <ActionRequestPageFrame breadcrumbs>
      <Panel sx={{ borderRadius: "16px", overflow: "hidden" }}>
        <Box
          sx={{
            display: "flex",
            alignItems: "center",
            gap: 1.5,
            px: { xs: 2, sm: 3 },
            py: 2.25,
            borderBottom: "1px solid",
            borderColor: "divider",
          }}
        >
          <Box
            sx={{
              display: "grid",
              placeItems: "center",
              width: 40,
              height: 40,
              borderRadius: "11px",
              color: "warning.main",
              bgcolor: (theme) => soft(theme, "warning", 0.15),
              border: "1px solid",
              borderColor: (theme) => soft(theme, "warning", 0.3),
              flexShrink: 0,
            }}
          >
            {isFix ? <Build /> : <BugReport />}
          </Box>
          <Box sx={{ minWidth: 0, flex: 1 }}>
            <Typography variant="h5" component="h2">
              {isFix ? "Review draft fix PR" : "Review issue"}
            </Typography>
            <Typography variant="body2" color="text.secondary">
              Requested by {request.owner}
            </Typography>
          </Box>
          <Chip
            size="small"
            label={request.status}
            color={
              request.status === "ready" || request.status === "unknown"
                ? "warning"
                : request.status === "confirmed"
                  ? "success"
                  : request.status === "failed"
                    ? "error"
                    : "default"
            }
            sx={{ textTransform: "capitalize" }}
          />
        </Box>

        <Box sx={{ px: { xs: 2, sm: 3 }, py: 2.5 }}>
          {error && (
            <Alert severity="error" variant="outlined" sx={{ mb: 2 }}>
              {error}
            </Alert>
          )}
          {request.warning && (
            <Alert severity="warning" variant="outlined" sx={{ mb: 2 }}>
              {request.warning}
            </Alert>
          )}

          {request.status === "pending" && (
            <Box
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
                    Generating the draft
                  </Typography>
                  <Typography variant="body2" color="text.secondary">
                    You can leave this page. If draft-ready email is configured,
                    the dashboard emails you when generation completes.
                  </Typography>
                </Box>
              </Stack>
            </Box>
          )}

          {request.status === "cancelling" && (
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
                    The request remains active until the server confirms that
                    its runtime work has stopped.
                  </Typography>
                </Box>
              </Stack>
            </Box>
          )}

          {request.status === "cancelled" && (
            <Alert severity="info">This request was cancelled.</Alert>
          )}
          {request.status === "unknown" && (
            <Alert severity="warning">GitHub may have accepted this action. Check the GitHub result before doing anything else.</Alert>
          )}
          {request.status === "expired" && (
            <Alert severity={request.result_url ? "info" : "warning"}>
              {request.result_url ? (
                <>
                  This request expired after the GitHub item was created.{" "}
                  <Link href={request.result_url} target="_blank" rel="noopener">
                    View the created item
                  </Link>
                </>
              ) : (
                "This draft expired. Start a new request from the recurring pattern."
              )}
            </Alert>
          )}
          {request.status === "confirmed" && request.result_url && (
            <Alert severity="success" icon={<CheckCircle />}>
              Created:{" "}
              <Link href={request.result_url} target="_blank" rel="noopener">
                {request.result_url}
              </Link>
            </Alert>
          )}

          {preview &&
            (request.status === "ready" ||
              request.status === "unknown" ||
              (request.status === "failed" && Boolean(request.warning))) && (
            <Stack spacing={2.5}>
              <ActionDraftPreview preview={preview} />
              {request.status === "ready" && <Box>
                <TextField
                  label="Refine this draft with a prompt (optional)"
                  fullWidth
                  multiline
                  minRows={2}
                  size="small"
                  value={instruction}
                  disabled={refining || confirming || cancelling}
                  onChange={(event) => setInstruction(event.target.value)}
                />
                <Button
                  size="small"
                  variant="outlined"
                  sx={{ mt: 1.25 }}
                  disabled={
                    refining ||
                    confirming ||
                    cancelling ||
                    instruction.trim() === ""
                  }
                  startIcon={
                    refining ? <CircularProgress size={14} color="inherit" /> : undefined
                  }
                  onClick={refine}
                >
                  {refining ? "Regenerating…" : "Regenerate with prompt"}
                </Button>
              </Box>}
            </Stack>
          )}
        </Box>

        <Box
          sx={{
            display: "flex",
            justifyContent: "flex-end",
            gap: 1,
            px: { xs: 2, sm: 3 },
            py: 2,
            borderTop: "1px solid",
            borderColor: "divider",
          }}
        >
          {canCancel && (
            <Button
              color="inherit"
              variant="outlined"
              disabled={cancelling || confirming || refining}
              onClick={cancel}
            >
              {cancelling ? "Cancelling…" : "Cancel request"}
            </Button>
          )}
          {canConfirm && (
            <Button
              color="warning"
              variant="contained"
              disabled={confirming || cancelling || refining}
              startIcon={
                confirming ? <CircularProgress size={16} color="inherit" /> : undefined
              }
              onClick={confirm}
            >
              {confirming
                ? "Confirming…"
                : request.status === "unknown"
                  ? "Check GitHub result"
                  : isFix
                  ? "Open draft PR"
                  : "File issue"}
            </Button>
          )}
        </Box>
      </Panel>
    </ActionRequestPageFrame>
  );
}
