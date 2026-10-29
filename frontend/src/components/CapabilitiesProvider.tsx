import { useEffect, useState, type ReactNode } from "react";
import Alert from "@mui/material/Alert";
import Button from "@mui/material/Button";
import { CapabilitiesContext, useCapabilities } from "../hooks/useCapabilities";
import { probeCapabilities, type CapabilityProbeState } from "../lib/capabilities";

// Server-only features stay off until the capability probe succeeds.
export function CapabilitiesProvider({ children }: { children: ReactNode }) {
  const [capabilities, setCapabilities] = useState<CapabilityProbeState>({
    mode: "pending",
    features: { actions: false },
  });
  const [attempt, setAttempt] = useState(0);

  useEffect(() => {
    const controller = new AbortController();
    void probeCapabilities({
      url: `${import.meta.env.BASE_URL}api/capabilities`,
      signal: controller.signal,
      onResult: setCapabilities,
    });
    return () => controller.abort();
  }, [attempt]);

  function retry() {
    setCapabilities({ mode: "pending", features: { actions: false } });
    setAttempt((value) => value + 1);
  }

  return (
    <CapabilitiesContext.Provider value={{ ...capabilities, retry }}>{children}</CapabilitiesContext.Provider>
  );
}

export function CapabilityNotice() {
  const capabilities = useCapabilities();
  if (capabilities.mode === "pending") {
    return (
      <Alert severity="info" role="status" sx={{ mb: 2 }}>
        Checking server capabilities. Server-only features are temporarily disabled.
      </Alert>
    );
  }
  if (capabilities.mode !== "unavailable") return null;
  return (
    <Alert
      severity="warning"
      action={<Button color="inherit" onClick={capabilities.retry} sx={{ minHeight: 44 }}>Retry</Button>}
      sx={{ mb: 2 }}
    >
      Unable to load server capabilities: {capabilities.error}. Server-only features remain disabled.
    </Alert>
  );
}
