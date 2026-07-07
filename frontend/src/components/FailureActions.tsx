import { useState } from "react";
import Alert from "@mui/material/Alert";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import Link from "@mui/material/Link";
import Stack from "@mui/material/Stack";
import Typography from "@mui/material/Typography";
import { BugReport, Build, GitHub } from "@mui/icons-material";
import { useCapabilities } from "../hooks/useCapabilities";
import { useAuth } from "../hooks/useAuth";

type Action = "create-issue" | "propose-fix";

interface Result {
  url?: string;
  error?: string;
}

const API_BASE = import.meta.env.BASE_URL;

// FailureActions renders admin write buttons for one systemic pattern. It shows
// only in server mode when the actions capability is present. Auth state is
// shared with the navbar via useAuth: in oauth mode a signed-out admin sees a
// contextual sign-in prompt; once signed in (or in proxy mode) the buttons act.
export function FailureActions({ failureID }: { failureID: string }) {
  const { features } = useCapabilities();
  const { status, signIn } = useAuth();
  const [busy, setBusy] = useState<Action | null>(null);
  const [result, setResult] = useState<Result | null>(null);

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

  async function run(action: Action) {
    setBusy(action);
    setResult(null);
    try {
      const res = await fetch(
        `${API_BASE}api/failures/${encodeURIComponent(failureID)}/${action}`,
        { method: "POST", credentials: "same-origin" },
      );
      if (!res.ok) {
        const text = await res.text();
        throw new Error(text.trim() || `HTTP ${res.status}`);
      }
      const body = (await res.json()) as { url: string };
      setResult({ url: body.url });
    } catch (e) {
      setResult({ error: e instanceof Error ? e.message : String(e) });
    } finally {
      setBusy(null);
    }
  }

  return (
    <Box>
      <Stack direction="row" spacing={1} sx={{ alignItems: "center", flexWrap: "wrap", rowGap: 1 }}>
        <Button
          size="small"
          variant="outlined"
          color="warning"
          startIcon={<BugReport sx={{ fontSize: 18 }} />}
          disabled={busy !== null}
          onClick={() => run("create-issue")}
        >
          {busy === "create-issue" ? "Filing…" : "File issue"}
        </Button>
        <Button
          size="small"
          variant="outlined"
          color="warning"
          startIcon={<Build sx={{ fontSize: 18 }} />}
          disabled={busy !== null}
          onClick={() => run("propose-fix")}
        >
          {busy === "propose-fix" ? "Drafting…" : "Propose fix"}
        </Button>
      </Stack>
      {result?.url && (
        <Alert severity="success" sx={{ mt: 1 }}>
          Opened:{" "}
          <Link href={result.url} target="_blank" rel="noopener">
            {result.url}
          </Link>
        </Alert>
      )}
      {result?.error && (
        <Alert severity="error" sx={{ mt: 1 }}>
          <Typography variant="body2">{result.error}</Typography>
        </Alert>
      )}
    </Box>
  );
}
