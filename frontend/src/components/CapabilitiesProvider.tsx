import { useEffect, useState, type ReactNode } from "react";
import { CapabilitiesContext } from "../hooks/useCapabilities";
import type { Capabilities } from "../types/capabilities";
import { STATIC_CAPABILITIES } from "../types/capabilities";

// CapabilitiesProvider discovers the deploy mode at boot. It probes
// /api/capabilities; a static Pages deploy has no such endpoint, so any failure
// leaves the app in read-only static mode. Server-only features light up only
// when a server advertises them.
export function CapabilitiesProvider({ children }: { children: ReactNode }) {
  const [capabilities, setCapabilities] = useState<Capabilities>(STATIC_CAPABILITIES);

  useEffect(() => {
    const url = `${import.meta.env.BASE_URL}api/capabilities`;
    fetch(url)
      .then((r) => {
        if (!r.ok) throw new Error(`HTTP ${r.status}`);
        return r.json() as Promise<Capabilities>;
      })
      .then((c) => {
        if (c?.mode === "server") {
          setCapabilities(c);
        }
      })
      .catch(() => {
        // No server endpoint: static Pages mode. Keep the read-only default.
      });
  }, []);

  return (
    <CapabilitiesContext.Provider value={capabilities}>{children}</CapabilitiesContext.Provider>
  );
}
