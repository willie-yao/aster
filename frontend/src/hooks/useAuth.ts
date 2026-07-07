import { createContext, useContext } from "react";

// AuthStatus is the admin sign-in state for write actions.
//   unavailable: no actions capability (static Pages or read-only server).
//   loading:     still checking the session.
//   anonymous:   actions available, not signed in (oauth mode).
//   authenticated: signed in (oauth), or authenticated upstream (proxy).
export type AuthStatus = "unavailable" | "loading" | "anonymous" | "authenticated";

export interface AuthState {
  status: AuthStatus;
  // login is the GitHub login when signed in via oauth; null in proxy mode.
  login: string | null;
  // mode mirrors capabilities.auth.mode.
  mode: "oauth" | "proxy" | null;
  signIn: () => void;
  signOut: () => Promise<void>;
}

export const AuthContext = createContext<AuthState>({
  status: "unavailable",
  login: null,
  mode: null,
  signIn: () => {},
  signOut: async () => {},
});

export function useAuth(): AuthState {
  return useContext(AuthContext);
}
