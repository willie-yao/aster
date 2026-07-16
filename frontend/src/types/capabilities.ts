// Capabilities describes the deploy mode the frontend is talking to. The
// Kubernetes-native server publishes it at /api/capabilities; static Pages
// deploys have no such endpoint, so the frontend defaults to read-only static
// mode and interactive features stay off.

export interface CapabilityFeatures {
  actions: boolean;
}

// AuthInfo tells the frontend how admins sign in for write actions.
export interface AuthInfo {
  mode: "oauth" | "proxy" | "dev";
  login_url?: string;
}

export interface Capabilities {
  mode: "static" | "server";
  features: CapabilityFeatures;
  auth?: AuthInfo;
}

// STATIC_CAPABILITIES is the read-only default used whenever no server
// advertises capabilities (the static Pages path).
export const STATIC_CAPABILITIES: Capabilities = {
  mode: "static",
  features: { actions: false },
};
