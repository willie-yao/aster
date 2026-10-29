import { createContext, useContext } from "react";
import type { CapabilityProbeState } from "../lib/capabilities";
import { STATIC_CAPABILITIES } from "../types/capabilities";

export type CapabilityState = CapabilityProbeState & { retry?: () => void };

export const CapabilitiesContext = createContext<CapabilityState>(STATIC_CAPABILITIES);

// useCapabilities returns the active deploy capabilities. It is always defined:
// outside a CapabilitiesProvider it resolves to the read-only static default.
export function useCapabilities(): CapabilityState {
  return useContext(CapabilitiesContext);
}
