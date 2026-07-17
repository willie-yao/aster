import { useCallback, useEffect, useState, type ReactNode } from "react";
import { AuthContext, type AuthStatus } from "../hooks/useAuth";
import { useCapabilities } from "../hooks/useCapabilities";

const API_BASE = import.meta.env.BASE_URL;

// AuthProvider tracks the admin sign-in state for write actions so the navbar
// and the action buttons share one source of truth. In oauth mode it probes
// /api/auth/user; in proxy mode the upstream SSO already authenticated the
// request, so it reports authenticated without an in-app login.
export function AuthProvider({ children }: { children: ReactNode }) {
  const { features, auth } = useCapabilities();
  const actionsAvailable = features.actions;
  const mode = auth?.mode ?? null;
  const loginUrl = auth?.login_url;

  const [oauth, setOAuth] = useState<{ status: "loading" | "anonymous" | "authenticated"; login: string | null }>({
    status: "loading",
    login: null,
  });

  useEffect(() => {
    if (!actionsAvailable || mode !== "oauth") return;
    let cancelled = false;
    fetch(`${API_BASE}api/auth/user`, { credentials: "same-origin" })
      .then((r) => (r.ok ? (r.json() as Promise<{ login: string }>) : null))
      .then((u) => {
        if (!cancelled) setOAuth({ status: u ? "authenticated" : "anonymous", login: u?.login ?? null });
      })
      .catch(() => {
        if (!cancelled) setOAuth({ status: "anonymous", login: null });
      });
    return () => {
      cancelled = true;
    };
  }, [actionsAvailable, mode]);

  const signIn = useCallback(() => {
    const base = loginUrl ?? `${API_BASE}api/auth/login`;
    // Return to the current page after signing in.
    const here = window.location.pathname + window.location.search + window.location.hash;
    window.location.href = `${base}?redirect=${encodeURIComponent(here)}`;
  }, [loginUrl]);

  const signOut = useCallback(async () => {
    await fetch(`${API_BASE}api/auth/logout`, { method: "POST", credentials: "same-origin" });
    setOAuth({ status: "anonymous", login: null });
  }, []);

  let status: AuthStatus;
  let login: string | null = null;
  if (!actionsAvailable) {
    status = "unavailable";
  } else if (mode !== "oauth") {
    status = "authenticated";
  } else {
    status = oauth.status;
    login = oauth.login;
  }

  return (
    <AuthContext.Provider value={{ status, login, mode, signIn, signOut }}>
      {children}
    </AuthContext.Provider>
  );
}
