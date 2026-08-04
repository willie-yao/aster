// Capabilities describes the deploy mode the frontend is talking to. The
// Kubernetes-native server publishes it at /api/capabilities; static Pages
// deploys have no such endpoint, so the frontend defaults to read-only static
// mode and interactive features stay off.

export interface CapabilityFeatures {
  actions: boolean;
  analysis_critique_version?: number;
  action_requests?: boolean;
  action_eligibility?: boolean;
  analysis_traces?: boolean;
  ai_usage?: boolean;
  fetch_status?: boolean;
  analysis_chat?: boolean;
  analysis_corrections?: boolean;
  source_investigation?: boolean;
  chat_fix?: boolean;
  chat_fix_min_confidence?: string;
}

// AuthInfo tells the frontend how admins sign in for operator features.
export interface AuthInfo {
  mode: "oauth" | "proxy" | "dev";
  login_url?: string;
}

export interface EngineInfo { version: string; commit: string; image_tag: string }

export interface Capabilities {
  mode: "static" | "server";
  features: CapabilityFeatures;
  auth?: AuthInfo;
  engine?: EngineInfo;
}

// STATIC_CAPABILITIES is the read-only default used whenever no server
// advertises capabilities (the static Pages path).
export const STATIC_CAPABILITIES: Capabilities = {
  mode: "static",
  features: { actions: false },
};
