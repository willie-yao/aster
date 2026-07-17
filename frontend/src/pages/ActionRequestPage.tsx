import { useEffect, useState } from "react";
import { useParams } from "react-router-dom";
import Alert from "@mui/material/Alert";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import CircularProgress from "@mui/material/CircularProgress";
import Link from "@mui/material/Link";
import Stack from "@mui/material/Stack";
import Typography from "@mui/material/Typography";
import { CheckCircle, GitHub } from "@mui/icons-material";
import { useAuth } from "../hooks/useAuth";
import { useCapabilities } from "../hooks/useCapabilities";
import { Panel } from "../components/Panel";

const API_BASE = import.meta.env.BASE_URL;

type RequestStatus =
  "pending" | "ready" | "failed" | "confirmed" | "cancelled" | "expired";

interface Preview {
  kind: "issue" | "fix";
  title: string;
  body: string;
  diff?: string;
  verify_status?: string;
  verify_summary?: string;
  verify_output?: string;
}

interface ActionRequest {
  id: string;
  failure_id: string;
  kind: "create-issue" | "propose-fix";
  owner: string;
  status: RequestStatus;
  created_at: string;
  updated_at: string;
  expires_at: string;
  error?: string;
  result_url?: string;
  preview?: Preview;
  email_sent?: boolean;
  email_error?: string;
}

function stripComments(value: string): string {
  return value.replace(/<!--[\s\S]*?-->/g, "").trim();
}

export function ActionRequestPage() {
  const { requestID = "" } = useParams();
  const { features } = useCapabilities();
  const { status, signIn } = useAuth();
  const [request, setRequest] = useState<ActionRequest | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [confirming, setConfirming] = useState(false);
  const [cancelling, setCancelling] = useState(false);

  useEffect(() => {
    if (!features.action_requests || status !== "authenticated" || !requestID)
      return;
    let cancelled = false;
    let timer: number | undefined;
    let retryCount = 0;
    function scheduleRetry() {
      const delay = Math.min(10_000, 1000 * 2 ** retryCount);
      retryCount += 1;
      timer = window.setTimeout(load, delay);
    }
    async function load() {
      try {
        const res = await fetch(
          `${API_BASE}api/action-requests/${encodeURIComponent(requestID)}`,
          {
            credentials: "same-origin",
            cache: "no-store",
          },
        );
        if (!res.ok) {
          const text = await res.text();
          const message = text.trim() || `HTTP ${res.status}`;
          if (cancelled) return;
          setError(message);
          if (res.status >= 500) scheduleRetry();
          return;
        }
        const value = (await res.json()) as ActionRequest;
        if (cancelled) return;
        retryCount = 0;
        setRequest(value);
        setError(null);
        if (value.status === "pending") timer = window.setTimeout(load, 2000);
      } catch (e) {
        if (cancelled) return;
        setError(e instanceof Error ? e.message : String(e));
        scheduleRetry();
      }
    }
    void load();
    return () => {
      cancelled = true;
      if (timer !== undefined) window.clearTimeout(timer);
    };
  }, [features.action_requests, requestID, status]);

  async function confirm() {
    if (!request) return;
    setConfirming(true);
    setError(null);
    try {
      const res = await fetch(
        `${API_BASE}api/action-requests/${encodeURIComponent(request.id)}/confirm`,
        {
          method: "POST",
          credentials: "same-origin",
        },
      );
      if (!res.ok)
        throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
      const body = (await res.json()) as { url: string };
      setRequest({ ...request, status: "confirmed", result_url: body.url });
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setConfirming(false);
    }
  }

  async function cancel() {
    if (!request) return;
    setCancelling(true);
    setError(null);
    try {
      const res = await fetch(
        `${API_BASE}api/action-requests/${encodeURIComponent(request.id)}/cancel`,
        {
          method: "POST",
          credentials: "same-origin",
        },
      );
      if (!res.ok)
        throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
      setRequest({ ...request, status: "cancelled" });
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setCancelling(false);
    }
  }

  if (!features.action_requests) {
    return (
      <Alert severity="warning">
        Asynchronous action requests are unavailable on this deployment.
      </Alert>
    );
  }
  if (status === "loading") return <CircularProgress size={24} />;
  if (status === "anonymous") {
    return (
      <Button variant="contained" startIcon={<GitHub />} onClick={signIn}>
        Sign in to review this draft
      </Button>
    );
  }
  if (error && !request) return <Alert severity="error">{error}</Alert>;
  if (!request) return <CircularProgress size={24} />;

  const preview = request.preview;
  const isFix = request.kind === "propose-fix";
  return (
    <Stack spacing={2.5}>
      <Box>
        <Typography variant="h4">
          {isFix ? "Fix proposal" : "Issue draft"}
        </Typography>
        <Typography variant="body2" color="text.secondary">
          Requested by {request.owner} · Status: {request.status}
        </Typography>
      </Box>
      {error && <Alert severity="error">{error}</Alert>}
      {request.status === "pending" && (
        <Panel>
          <Stack direction="row" spacing={1.5} sx={{ alignItems: "center" }}>
            <CircularProgress size={20} />
            <Typography>
              Generating the draft. You may leave this page; when draft-ready
              email is configured, a review link is sent when it is ready.
            </Typography>
          </Stack>
          <Button
            sx={{ mt: 2 }}
            color="inherit"
            variant="outlined"
            disabled={cancelling}
            onClick={cancel}
          >
            {cancelling ? "Cancelling…" : "Cancel request"}
          </Button>
        </Panel>
      )}
      {request.status === "failed" && (
        <Alert severity="error">
          {request.error || "Draft generation failed."}
        </Alert>
      )}
      {request.status === "cancelled" && (
        <Alert severity="info">This request was cancelled.</Alert>
      )}
      {request.status === "expired" && (
        <Alert severity={request.result_url ? "info" : "warning"}>
          {request.result_url ? (
            <>
              This request expired after creating:{" "}
              <Link href={request.result_url} target="_blank" rel="noopener">
                {request.result_url}
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
      {preview && request.status === "ready" && (
        <Stack spacing={2}>
          <Panel>
            <Typography variant="label" color="text.secondary">
              Title
            </Typography>
            <Typography variant="h6">{preview.title}</Typography>
          </Panel>
          <Panel>
            <Typography variant="label" color="text.secondary">
              {isFix ? "Description" : "Body"}
            </Typography>
            <Box
              component="pre"
              sx={{
                whiteSpace: "pre-wrap",
                overflowWrap: "anywhere",
                fontFamily: "inherit",
              }}
            >
              {stripComments(preview.body)}
            </Box>
          </Panel>
          {preview.diff && (
            <Panel>
              <Typography variant="label" color="text.secondary">
                Proposed diff
              </Typography>
              <Box
                component="pre"
                sx={{ whiteSpace: "pre", overflow: "auto", fontSize: 13 }}
              >
                {preview.diff}
              </Box>
            </Panel>
          )}
          {preview.verify_status && (
            <Alert
              severity={preview.verify_status === "failed" ? "warning" : "info"}
            >
              Verification: {preview.verify_status}
              {preview.verify_summary ? ` · ${preview.verify_summary}` : ""}
            </Alert>
          )}
          {preview.verify_status === "failed" && preview.verify_output && (
            <Panel>
              <Typography variant="label" color="text.secondary">
                Verification output
              </Typography>
              <Box
                component="pre"
                sx={{
                  whiteSpace: "pre-wrap",
                  overflow: "auto",
                  overflowWrap: "anywhere",
                  maxHeight: 200,
                  fontSize: 13,
                }}
              >
                {preview.verify_output}
              </Box>
            </Panel>
          )}
          <Stack direction="row" spacing={1}>
            <Button
              color="inherit"
              variant="outlined"
              disabled={cancelling || confirming}
              onClick={cancel}
            >
              {cancelling ? "Cancelling…" : "Cancel request"}
            </Button>
            <Button
              color="warning"
              variant="contained"
              disabled={confirming || cancelling}
              startIcon={
                confirming ? (
                  <CircularProgress size={16} color="inherit" />
                ) : undefined
              }
              onClick={confirm}
            >
              {confirming
                ? "Confirming…"
                : isFix
                  ? "Open draft PR"
                  : "File issue"}
            </Button>
          </Stack>
        </Stack>
      )}
    </Stack>
  );
}
