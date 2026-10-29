import { STATIC_CAPABILITIES, type Capabilities } from "../types/capabilities.js";

export interface CapabilityProbeState extends Omit<Capabilities, "mode"> {
  mode: Capabilities["mode"] | "pending" | "unavailable";
  error?: string;
}

function waitForRetry(delay: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    const finish = () => {
      clearTimeout(timer);
      signal.removeEventListener("abort", finish);
      resolve();
    };
    const timer = setTimeout(finish, delay);
    signal.addEventListener("abort", finish, { once: true });
    if (signal.aborted) finish();
  });
}

// Probe once with bounded retries; cancellation suppresses every late result.
export async function probeCapabilities({
  url,
  signal,
  onResult,
  fetcher = fetch,
  wait = waitForRetry,
}: {
  url: string;
  signal: AbortSignal;
  onResult: (state: CapabilityProbeState) => void;
  fetcher?: typeof fetch;
  wait?: (delay: number, signal: AbortSignal) => Promise<void>;
}): Promise<void> {
  for (let attempt = 0; attempt < 3 && !signal.aborted; attempt += 1) {
    let retryable = true;
    try {
      const response = await fetcher(url, {
        signal: AbortSignal.any([signal, AbortSignal.timeout(5000)]),
        credentials: "same-origin",
        cache: "no-store",
      });
      if (signal.aborted) return;
      if (response.status === 404) {
        onResult(STATIC_CAPABILITIES);
        return;
      }
      if (!response.ok) {
        retryable = response.status >= 500;
        throw new Error(`HTTP ${response.status}`);
      }
      // Vite and static SPA hosts can serve index.html for an unknown path.
      if (response.headers.get("Content-Type")?.split(";")[0].trim().toLowerCase() === "text/html") {
        onResult(STATIC_CAPABILITIES);
        return;
      }
      const capabilities = await response.json() as Capabilities;
      if (signal.aborted) return;
      retryable = false;
      if (
        (capabilities?.mode !== "server" && capabilities?.mode !== "static") ||
        typeof capabilities.features?.actions !== "boolean"
      ) {
        throw new Error("Invalid capability response");
      }
      onResult(capabilities.mode === "static" ? STATIC_CAPABILITIES : capabilities);
      return;
    } catch (error) {
      if (signal.aborted) return;
      if (!retryable || error instanceof SyntaxError || attempt === 2) {
        onResult({
          mode: "unavailable",
          features: { actions: false },
          error: error instanceof Error ? error.message : "Capability request failed",
        });
        return;
      }
    }
    await wait(500 * (attempt + 1), signal);
  }
}
