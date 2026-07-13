// Package server serves the dashboard over HTTP for the Kubernetes-native
// deploy mode. It is a strict superset of the static Pages contract: it serves
// the exact same /data/*.json files the SPA already reads, and adds a
// capability descriptor at /api/capabilities that lets the frontend discover
// server-only features. With no capability descriptor the frontend stays in
// read-only static mode, so one build serves both deploy targets.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/actions"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/auth"
)

// ActionRunner performs on-demand actions for a failure id using the admin's
// token. Actions are two-phase: a preview renders the exact issue or PR without
// posting, then Confirm posts the previewed draft. Resolve/Unresolve hide or
// restore a systemic pattern in the published view. actions.Service satisfies it.
type ActionRunner interface {
	PreviewIssue(ctx context.Context, failureID, userToken, instruction string) (actions.PreviewResult, error)
	PreviewFix(ctx context.Context, failureID, userToken, instruction string) (actions.PreviewResult, error)
	Confirm(ctx context.Context, token, userToken string) (string, error)
	Resolve(failureID, login, note string) error
	Unresolve(failureID string) error
}

// defaultActionTimeout bounds a single on-demand action. Fix-PR drafting calls
// the model and opens a PR, so it can run for a while.
const defaultActionTimeout = 5 * time.Minute

// Options configures a server Handler.
type Options struct {
	// DataDir is the fetcher output directory served at /data. Required.
	DataDir string
	// StaticDir is an optional built frontend (dist) served at / with SPA
	// fallback. Empty serves data and API only, with the SPA hosted elsewhere.
	StaticDir string
	// Capabilities is the descriptor returned at /api/capabilities.
	Capabilities Capabilities
	// Auth and Actions enable the admin-gated write endpoints. Both must be set;
	// when either is nil the server is read-only and advertises no actions.
	Auth    auth.Authenticator
	Actions ActionRunner
	// ActionTimeout bounds a single action. Zero uses defaultActionTimeout.
	ActionTimeout time.Duration
	// AuthMode is advertised to the frontend: "oauth" (show a sign-in button) or
	// "proxy" (auth handled upstream; the UI just calls the actions).
	AuthMode string
	// LoginURL is where the frontend sends admins to sign in (oauth mode).
	LoginURL string
	// TrustedOrigins are extra public origins the CSRF guard accepts in addition
	// to the request host, given as full origins ("https://host") or bare hosts.
	// Needed when a reverse proxy (e.g. Azure Front Door) terminates the public
	// hostname but forwards a different Host to this server, so the browser's
	// Origin never equals r.Host. Empty keeps strict same-origin behavior.
	TrustedOrigins []string
}

// Capabilities tells the frontend which deploy mode it is talking to and which
// server-only features are available. Its absence (static Pages mode) means
// read-only.
type Capabilities struct {
	// Mode is "server" when served by this binary.
	Mode string `json:"mode"`
	// Features gates additive interactive UI. All false at read parity.
	Features Features `json:"features"`
	// Auth describes how the frontend should authenticate for write actions.
	// Nil when actions are unavailable.
	Auth *AuthInfo `json:"auth,omitempty"`
}

// AuthInfo tells the frontend how admins sign in for write actions.
type AuthInfo struct {
	// Mode is "oauth" (redirect to LoginURL) or "proxy" (upstream SSO).
	Mode string `json:"mode"`
	// LoginURL is the sign-in redirect for oauth mode.
	LoginURL string `json:"login_url,omitempty"`
}

// Features enumerates the optional interactive capabilities.
type Features struct {
	// Chat enables conversational triage UI.
	Chat bool `json:"chat"`
	// Actions enables on-page create-issue / propose-fix buttons.
	Actions bool `json:"actions"`
}

// authRegistrar is implemented by authenticators that need their own routes
// (the OAuth login/callback/user/logout endpoints).
type authRegistrar interface {
	Register(mux *http.ServeMux)
}

// DefaultCapabilities is the read-parity descriptor: server mode, no
// interactive features yet.
func DefaultCapabilities() Capabilities {
	return Capabilities{Mode: "server"}
}

// Handler builds the HTTP handler for the dashboard server. DataDir must exist.
func Handler(opts Options) (http.Handler, error) {
	if opts.DataDir == "" {
		return nil, fmt.Errorf("server: DataDir is required")
	}
	if info, err := os.Stat(opts.DataDir); err != nil {
		return nil, fmt.Errorf("server: data dir %q: %w", opts.DataDir, err)
	} else if !info.IsDir() {
		return nil, fmt.Errorf("server: data dir %q is not a directory", opts.DataDir)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})

	// Admin-gated write actions, enabled only when both auth and an action
	// runner are configured. Advertise the capability so the frontend lights up
	// the buttons.
	caps := opts.Capabilities
	if opts.Auth != nil && opts.Actions != nil {
		caps.Features.Actions = true
		caps.Auth = &AuthInfo{Mode: opts.AuthMode, LoginURL: opts.LoginURL}
		timeout := opts.ActionTimeout
		if timeout <= 0 {
			timeout = defaultActionTimeout
		}
		// Register the authenticator's own routes (OAuth login/callback/etc).
		if reg, ok := opts.Auth.(authRegistrar); ok {
			reg.Register(mux)
		}
		trusted := trustedOriginSet(opts.TrustedOrigins)
		guard := func(next http.Handler) http.Handler { return csrfGuard(trusted, next) }
		mux.Handle("POST /api/failures/{id}/create-issue/preview",
			auth.Middleware(opts.Auth, guard(previewHandler(timeout, opts.Actions.PreviewIssue))))
		mux.Handle("POST /api/failures/{id}/propose-fix/preview",
			auth.Middleware(opts.Auth, guard(previewHandler(timeout, opts.Actions.PreviewFix))))
		mux.Handle("POST /api/actions/confirm",
			auth.Middleware(opts.Auth, guard(confirmHandler(timeout, opts.Actions.Confirm))))
		mux.Handle("POST /api/failures/{id}/resolve",
			auth.Middleware(opts.Auth, guard(resolveHandler(opts.Actions.Resolve))))
		mux.Handle("POST /api/failures/{id}/unresolve",
			auth.Middleware(opts.Auth, guard(unresolveHandler(opts.Actions.Unresolve))))
	}
	mux.HandleFunc("/api/capabilities", capabilitiesHandler(caps))

	// /data/* serves the fetcher output tree (manifest.json, dashboard.json,
	// jobs/*.json, flakiness.json, search-index.json) at read parity. Directory
	// listings are disabled so it serves files, not a browsable tree. The data
	// is rewritten every fetch, so mark it no-cache: the browser revalidates via
	// If-Modified-Since (cheap 304 when unchanged) instead of serving a stale
	// copy from its heuristic cache.
	dataFS := http.FileServer(noListFS{http.Dir(opts.DataDir)})
	mux.Handle("/data/", http.StripPrefix("/data/", noCache(dataFS)))

	if opts.StaticDir != "" {
		if info, err := os.Stat(opts.StaticDir); err != nil {
			return nil, fmt.Errorf("server: static dir %q: %w", opts.StaticDir, err)
		} else if !info.IsDir() {
			return nil, fmt.Errorf("server: static dir %q is not a directory", opts.StaticDir)
		}
		mux.Handle("/", spaHandler(opts.StaticDir))
	}

	return securityHeaders(mux), nil
}

// securityHeaders wraps the handler with conservative response headers. It denies
// framing (clickjacking of the admin action flow), disables MIME sniffing, and
// limits referrer leakage. No Content-Security-Policy is set here because the SPA
// relies on inline styles; a nonce-based CSP is a separate frontend change.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		next.ServeHTTP(w, r)
	})
}

// capabilitiesHandler returns the capability descriptor as JSON.
func capabilitiesHandler(c Capabilities) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(c)
	}
}

// csrfGuard rejects a state-changing request whose Origin header is present and
// matches neither the request host nor a configured trusted origin. Combined
// with the SameSite=Lax session cookie, this blocks cross-site POSTs while
// leaving same-origin and non-browser (no Origin header) clients unaffected.
// trusted carries public hostnames served by a reverse proxy that rewrites the
// forwarded Host, so the browser's Origin never equals r.Host.
func csrfGuard(trusted map[string]struct{}, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || (u.Host != r.Host && !originTrusted(trusted, u.Host)) {
				http.Error(w, "cross-origin request rejected", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// originTrusted reports whether host is in the trusted set. Empty set never
// matches, preserving strict same-origin behavior.
func originTrusted(trusted map[string]struct{}, host string) bool {
	if host == "" || len(trusted) == 0 {
		return false
	}
	_, ok := trusted[host]
	return ok
}

// trustedOriginSet normalizes configured origins to a host set. Each entry may
// be a full origin ("https://host[:port]") or a bare host; the scheme is
// dropped so it matches an Origin header's host component.
func trustedOriginSet(origins []string) map[string]struct{} {
	set := make(map[string]struct{}, len(origins))
	for _, o := range origins {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		host := o
		if u, err := url.Parse(o); err == nil && u.Host != "" {
			host = u.Host
		}
		set[host] = struct{}{}
	}
	return set
}

// previewFunc renders a draft (issue or fix) for a failure id without posting.
type previewFunc func(ctx context.Context, failureID, userToken, instruction string) (actions.PreviewResult, error)

// previewHandler runs an authed preview and returns the draft JSON. The admin
// identity is set by auth.Middleware. Errors map to 404 (unknown failure) or
// 422 (not actionable / misconfigured / upstream), never leaking the token.
func previewHandler(timeout time.Duration, run previewFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		identity, ok := auth.IdentityFrom(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		instruction := decodeInstruction(r)
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()

		res, err := run(ctx, id, identity.Token, instruction)
		if err != nil {
			writeActionError(w, id, identity.Login, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(res)
	})
}

// confirmFunc posts the draft previously cached under a token.
type confirmFunc func(ctx context.Context, token, userToken string) (string, error)

// confirmHandler posts the previewed draft named by {"token": ...} and returns
// {"url": ...}. The admin identity is set by auth.Middleware.
func confirmHandler(timeout time.Duration, run confirmFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.IdentityFrom(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var body struct {
			Token string `json:"token"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil || strings.TrimSpace(body.Token) == "" {
			http.Error(w, "missing token", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()

		url, err := run(ctx, strings.TrimSpace(body.Token), identity.Token)
		if err != nil {
			writeActionError(w, "confirm", identity.Login, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"url": url})
	})
}

// decodeInstruction reads an optional {"instruction": ...} from the request
// body, tolerating an absent or empty body.
func decodeInstruction(r *http.Request) string {
	var body struct {
		Instruction string `json:"instruction"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&body)
	return strings.TrimSpace(body.Instruction)
}

// writeActionError maps an action error to a status code without leaking the
// token: an unknown failure or expired preview is 404, everything else is 422.
func writeActionError(w http.ResponseWriter, id, login string, err error) {
	if errors.Is(err, actions.ErrNotFound) || errors.Is(err, actions.ErrPreviewNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	log.Printf("action failed for %s (by %s): %v", id, login, err)
	http.Error(w, err.Error(), http.StatusUnprocessableEntity)
}

// resolveHandler marks a systemic pattern resolved with an optional {"note":...}.
// It attributes the action to the signed-in admin and returns 204 on success.
func resolveHandler(run func(failureID, login, note string) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		identity, ok := auth.IdentityFrom(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var body struct {
			Note string `json:"note"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&body)
		if err := run(id, identity.Login, body.Note); err != nil {
			writeActionError(w, id, identity.Login, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

// unresolveHandler clears a pattern's resolved mark, returning 204 on success.
func unresolveHandler(run func(failureID string) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		identity, ok := auth.IdentityFrom(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if err := run(id); err != nil {
			writeActionError(w, id, identity.Login, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

// spaHandler serves a single-page app from dir: real files are served as-is,
// and any unmatched path falls back to index.html so client-side routes resolve
// on deep links and refreshes. Content-hashed build assets under /assets/ are
// cached immutably; index.html is never cached so a new deploy is picked up
// immediately.
func spaHandler(dir string) http.HandlerFunc {
	index := filepath.Join(dir, "index.html")
	fileServer := http.FileServer(http.Dir(dir))
	serveIndex := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, index)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		clean := filepath.Clean(strings.TrimPrefix(r.URL.Path, "/"))
		// Anything that is not a lexically local path (traversal, absolute) or
		// is the root falls back to index.html rather than touching the disk.
		if clean == "." || !filepath.IsLocal(clean) {
			serveIndex(w, r)
			return
		}
		if info, err := os.Stat(filepath.Join(dir, clean)); err == nil && !info.IsDir() {
			// Vite emits content-hashed filenames under assets/, so they can be
			// cached forever; a content change yields a new name.
			if strings.HasPrefix(clean, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			fileServer.ServeHTTP(w, r)
			return
		}
		serveIndex(w, r)
	}
}

// noCache marks a response as always-revalidate so browsers don't serve a stale
// heuristically-cached copy of the frequently-rewritten data files.
func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}

// noListFS wraps an http.FileSystem to disable directory listings: opening a
// directory returns os.ErrNotExist, so http.FileServer responds 404 instead of
// rendering an index of the tree.
type noListFS struct{ fs http.FileSystem }

func (f noListFS) Open(name string) (http.File, error) {
	file, err := f.fs.Open(name)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	if info.IsDir() {
		file.Close()
		return nil, os.ErrNotExist
	}
	return file, nil
}
