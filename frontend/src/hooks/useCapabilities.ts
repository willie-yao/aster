import { createContext, useContext } from "react";
import type { Capabilities } from "../types/capabilities";
import { STATIC_CAPABILITIES } from "../types/capabilities";

export const CapabilitiesContext = createContext<Capabilities>(STATIC_CAPABILITIES);

// useCapabilities returns the active deploy capabilities. It is always defined:
// outside a CapabilitiesProvider it resolves to the read-only static default.
export function useCapabilities(): Capabilities {
  return useContext(CapabilitiesContext);
}
