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
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/actions"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/auth"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/output"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/redact"
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

// ActionRequestRunner persists asynchronous drafts for later authenticated review.
type ActionRequestRunner interface {
	CreateRequest(failureID, kind, login, userToken, instruction, supersedesID string) (actions.ActionRequestView, error)
	GetRequest(id, login string) (actions.ActionRequestView, error)
	ConfirmRequest(ctx context.Context, id, login, userToken string) (string, error)
	CancelRequest(ctx context.Context, id, login string) (actions.ActionRequestView, error)
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
	// Auth enables admin-gated operator features. Actions additionally enables
	// write endpoints. With no Auth the server stays read-only.
	Auth                auth.Authenticator
	Actions             ActionRunner
	AnalysisChat        AnalysisChatRunner
	AnalysisCorrections AnalysisCorrectionRunner
	SourceInvestigation SourceInvestigationRunner
	// ChatFix bridges one selected chat response into the existing fix preview.
	ChatFix ChatFixRunner
	// ActionTimeout bounds a single action. Zero uses defaultActionTimeout.
	ActionTimeout time.Duration
	// AnalysisChatTimeout bounds one conversation turn.
	AnalysisChatTimeout time.Duration
	// SourceInvestigationTimeout bounds one read-only source run.
	SourceInvestigationTimeout time.Duration
	// AuthMode is advertised to the frontend: "oauth" (show a sign-in button),
	// "proxy" (auth handled upstream), or "dev" for local use.
	AuthMode string
	// LoginURL is where the frontend sends admins to sign in (oauth mode).
	LoginURL string
	// TrustedOrigins are extra public origins the CSRF guard accepts in addition
	// to the request host, given as full origins ("https://host") or bare hosts.
	// Needed when a reverse proxy (e.g. Azure Front Door) terminates the public
	// hostname but forwards a different Host to this server, so the browser's
	// Origin never equals r.Host. Empty keeps strict same-origin behavior.
	TrustedOrigins []string
	// HSTSEnabled adds a one-year Strict-Transport-Security policy. Local HTTP
	// development leaves it disabled.
	HSTSEnabled bool
	// AIUsageEnabled exposes private usage data when authentication is configured.
	AIUsageEnabled bool
}

// Capabilities tells the frontend which deploy mode it is talking to and which
// server-only features are available. Its absence (static Pages mode) means
// read-only.
type Capabilities struct {
	// Mode is "server" when served by this binary.
	Mode string `json:"mode"`
	// Features gates additive interactive UI. All false at read parity.
	Features Features `json:"features"`
	// Auth describes how the frontend should authenticate for operator features.
	// Nil when no authenticated feature is available.
	Auth *AuthInfo `json:"auth,omitempty"`
}

// AuthInfo tells the frontend how admins sign in for operator features.
type AuthInfo struct {
	// Mode is "oauth", "proxy", or local-only "dev".
	Mode string `json:"mode"`
	// LoginURL is the sign-in redirect for oauth mode.
	LoginURL string `json:"login_url,omitempty"`
}

// Features enumerates the optional interactive capabilities.
type Features struct {
	// Actions enables on-page create-issue / propose-fix buttons.
	Actions bool `json:"actions"`
	// AnalysisCritiqueVersion lets action UIs apply the current critique gate.
	AnalysisCritiqueVersion int `json:"analysis_critique_version,omitempty"`
	// ActionRequests enables persisted asynchronous draft generation.
	ActionRequests bool `json:"action_requests,omitempty"`
	// AnalysisTraces enables the private analysis-trace API and UI.
	AnalysisTraces bool `json:"analysis_traces,omitempty"`
	// FetchStatus enables the private aggregate fetch progress API and banner.
	FetchStatus bool `json:"fetch_status,omitempty"`
	// AIUsage enables the private token and cost API.
	AIUsage bool `json:"ai_usage,omitempty"`
	// AnalysisChat enables authenticated conversations about one published analysis.
	AnalysisChat        bool `json:"analysis_chat,omitempty"`
	AnalysisCorrections bool `json:"analysis_corrections,omitempty"`
	SourceInvestigation bool `json:"source_investigation,omitempty"`
	// ChatFix enables server-validated chat context for fix previews.
	ChatFix              bool   `json:"chat_fix,omitempty"`
	ChatFixMinConfidence string `json:"chat_fix_min_confidence,omitempty"`
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

	if opts.SourceInvestigation != nil && opts.AnalysisChat == nil {
		return nil, fmt.Errorf("server: source investigation requires analysis chat")
	}
	if opts.ChatFix != nil && (opts.AnalysisChat == nil || opts.Actions == nil) {
		return nil, fmt.Errorf("server: chat fix requires analysis chat and actions")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})

	// Authenticated operator features share one identity and auth-route setup.
	caps := opts.Capabilities
	if opts.Auth != nil {
		caps.Auth = &AuthInfo{Mode: opts.AuthMode, LoginURL: opts.LoginURL}
		caps.Features.AnalysisTraces = true
		caps.Features.FetchStatus = true
		if reg, ok := opts.Auth.(authRegistrar); ok {
			reg.Register(mux)
		}
		mux.Handle("GET /api/analysis-traces",
			auth.Middleware(opts.Auth, analysisTracesHandler(opts.DataDir, false)))
		mux.Handle("GET /api/analysis-traces/download",
			auth.Middleware(opts.Auth, analysisTracesHandler(opts.DataDir, true)))
		if opts.AIUsageEnabled {
			caps.Features.AIUsage = true
			mux.Handle("GET /api/ai-usage", auth.Middleware(opts.Auth, aiUsageHandler(opts.DataDir, false, time.Now)))
			mux.Handle("GET /api/ai-usage/download", auth.Middleware(opts.Auth, aiUsageHandler(opts.DataDir, true, time.Now)))
		}
		mux.Handle("/api/fetch-status",
			readOnly(auth.Middleware(opts.Auth, fetchStatusHandler(opts.DataDir))))
	}

	if opts.Auth != nil && opts.AnalysisChat != nil {
		caps.Features.AnalysisChat = true
		timeout := opts.AnalysisChatTimeout
		if timeout <= 0 {
			timeout = defaultAnalysisChatTimeout
		}
		trusted := trustedOriginSet(opts.TrustedOrigins)
		guard := func(next http.Handler) http.Handler { return csrfGuard(trusted, next) }
		mux.Handle("POST /api/analysis-chat/sessions",
			auth.Middleware(opts.Auth, guard(createAnalysisChatSessionHandler(opts.AnalysisChat))))
		mux.Handle("POST /api/analysis-chat/sessions/lookup",
			auth.Middleware(opts.Auth, guard(findAnalysisChatSessionHandler(opts.AnalysisChat))))
		mux.Handle("GET /api/analysis-chat/sessions/{id}",
			auth.Middleware(opts.Auth, getAnalysisChatSessionHandler(opts.AnalysisChat)))
		mux.Handle("POST /api/analysis-chat/sessions/{id}/messages",
			auth.Middleware(opts.Auth, guard(sendAnalysisChatMessageHandler(timeout, opts.AnalysisChat))))
		mux.Handle("POST /api/analysis-chat/sessions/{id}/messages/stream",
			auth.Middleware(opts.Auth, guard(streamAnalysisChatMessageHandler(timeout, opts.AnalysisChat))))
		mux.Handle("POST /api/analysis-chat/sessions/{id}/requests/{requestID}/cancel",
			auth.Middleware(opts.Auth, guard(cancelAnalysisChatMessageHandler(opts.AnalysisChat))))
	}

	if opts.Auth != nil && opts.SourceInvestigation != nil {
		caps.Features.SourceInvestigation = true
		timeout := opts.SourceInvestigationTimeout
		if timeout <= 0 {
			timeout = 10 * time.Minute
		}
		trusted := trustedOriginSet(opts.TrustedOrigins)
		guard := func(next http.Handler) http.Handler { return csrfGuard(trusted, next) }
		mux.Handle("POST /api/analysis-chat/sessions/{id}/source-investigations",
			auth.Middleware(opts.Auth, guard(sourceInvestigationHandler(timeout, opts.SourceInvestigation))))
		mux.Handle("POST /api/analysis-chat/sessions/{id}/source-investigations/stream",
			auth.Middleware(opts.Auth, guard(streamSourceInvestigationHandler(timeout, opts.SourceInvestigation))))
		mux.Handle("GET /api/analysis-chat/sessions/{id}/source-investigations/{requestID}",
			auth.Middleware(opts.Auth, getSourceInvestigationHandler(opts.SourceInvestigation)))
		mux.Handle("POST /api/analysis-chat/sessions/{id}/source-investigations/{requestID}/cancel",
			auth.Middleware(opts.Auth, guard(cancelSourceInvestigationHandler(opts.SourceInvestigation))))
	}

	if opts.Auth != nil && opts.ChatFix != nil {
		caps.Features.ChatFix = true
		if strings.TrimSpace(caps.Features.ChatFixMinConfidence) == "" {
			caps.Features.ChatFixMinConfidence = "high"
		}
		timeout := opts.ActionTimeout
		if timeout <= 0 {
			timeout = defaultActionTimeout
		}
		trusted := trustedOriginSet(opts.TrustedOrigins)
		guard := func(next http.Handler) http.Handler { return csrfGuard(trusted, next) }
		mux.Handle("POST /api/analysis-chat/sessions/{id}/requests/{requestID}/fix/preview",
			auth.Middleware(opts.Auth, guard(previewChatFixHandler(timeout, opts.ChatFix))))
	}

	if opts.Auth != nil && opts.AnalysisCorrections != nil {
		caps.Features.AnalysisCorrections = true
		trusted := trustedOriginSet(opts.TrustedOrigins)
		guard := func(next http.Handler) http.Handler { return csrfGuard(trusted, next) }
		mux.Handle("POST /api/analysis-chat/sessions/{id}/requests/{requestID}/correction/preview",
			auth.Middleware(opts.Auth, guard(previewAnalysisCorrectionHandler(opts.AnalysisCorrections))))
		mux.Handle("POST /api/analysis-corrections/confirm",
			auth.Middleware(opts.Auth, guard(confirmAnalysisCorrectionHandler(opts.AnalysisCorrections))))
		mux.Handle("POST /api/analysis-corrections/{id}/revoke",
			auth.Middleware(opts.Auth, guard(revokeAnalysisCorrectionHandler(opts.AnalysisCorrections))))
	}

	// Write actions require both auth and an action runner.
	if opts.Auth != nil && opts.Actions != nil {
		caps.Features.Actions = true
		caps.Features.AnalysisCritiqueVersion = ai.CurrentCritiqueVersion()
		timeout := opts.ActionTimeout
		if timeout <= 0 {
			timeout = defaultActionTimeout
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
		if requests, ok := opts.Actions.(ActionRequestRunner); ok {
			caps.Features.ActionRequests = true
			mux.Handle("POST /api/failures/{id}/{action}/requests",
				auth.Middleware(opts.Auth, guard(createActionRequestHandler(requests.CreateRequest))))
			mux.Handle("GET /api/action-requests/{id}",
				auth.Middleware(opts.Auth, getActionRequestHandler(requests.GetRequest)))
			mux.Handle("POST /api/action-requests/{id}/confirm",
				auth.Middleware(opts.Auth, guard(confirmActionRequestHandler(timeout, requests.ConfirmRequest))))
			mux.Handle("POST /api/action-requests/{id}/cancel",
				auth.Middleware(opts.Auth, guard(cancelActionRequestHandler(timeout, requests.CancelRequest))))
		}
	}
	mux.Handle("/api/capabilities", readOnly(capabilitiesHandler(caps)))

	// /data/* serves the fetcher output tree (manifest.json, dashboard.json,
	// jobs/*.json, flakiness.json, search-index.json) at read parity. Directory
	// listings are disabled so it serves files, not a browsable tree. The data
	// is rewritten every fetch, so mark it no-cache: the browser revalidates via
	// If-Modified-Since (cheap 304 when unchanged) instead of serving a stale
	// copy from its heuristic cache.
	dataFS := http.FileServer(noListFS{http.Dir(opts.DataDir)})
	mux.Handle("/data/", readOnly(http.StripPrefix("/data/", noCache(dataFS))))

	if opts.StaticDir != "" {
		if info, err := os.Stat(opts.StaticDir); err != nil {
			return nil, fmt.Errorf("server: static dir %q: %w", opts.StaticDir, err)
		} else if !info.IsDir() {
			return nil, fmt.Errorf("server: static dir %q is not a directory", opts.StaticDir)
		}
		mux.Handle("/", readOnly(spaHandler(opts.StaticDir)))
	}

	return compressResponses(securityHeaders(opts.HSTSEnabled, mux)), nil
}

const contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; object-src 'none'; base-uri 'self'; frame-ancestors 'none'; form-action 'self'; connect-src 'self'; img-src 'self' data:; font-src 'self'"

const permissionsPolicy = "accelerometer=(), autoplay=(), camera=(), geolocation=(), gyroscope=(), magnetometer=(), microphone=(), payment=(), usb=()"

// securityHeaders wraps the handler with conservative response headers. Scripts
// stay same-origin only. MUI runtime styles require style-src 'unsafe-inline'.
func securityHeaders(hsts bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("Permissions-Policy", permissionsPolicy)
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		if hsts {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

// readOnly limits a handler to safe retrieval methods.
func readOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
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
	if errors.Is(err, actions.ErrNotFound) || errors.Is(err, actions.ErrPreviewNotFound) || errors.Is(err, actions.ErrRequestNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if errors.Is(err, actions.ErrPreviewPending) {
		http.Error(w, "action request is already being processed", http.StatusConflict)
		return
	}
	if errors.Is(err, actions.ErrPreviewSuperseded) {
		http.Error(w, "action request was replaced by a newer attempt", http.StatusConflict)
		return
	}
	if errors.Is(err, actions.ErrPreviewOutcomeUnknown) {
		http.Error(w, "GitHub action outcome is unknown", http.StatusConflict)
		return
	}
	if errors.Is(err, actions.ErrPreviewTargetChanged) {
		http.Error(w, "action target changed; generate a new preview", http.StatusConflict)
		return
	}
	if errors.Is(err, actions.ErrRemediationAlreadyPresent) {
		http.Error(w, "the proposed remediation already exists at the grounded commit; check whether the finding is stale, regressed, or misclassified", http.StatusConflict)
		return
	}
	if errors.Is(err, actions.ErrRemediationInconclusive) {
		log.Printf("action source verification inconclusive for %s (by %s): %s", id, login, safeOperatorError(err))
		http.Error(w, "source verification was inconclusive; investigate the grounded source before filing", http.StatusConflict)
		return
	}
	log.Printf("action failed for %s (by %s): %s", id, login, safeOperatorError(err))
	http.Error(w, "action request could not be completed", http.StatusUnprocessableEntity)
}

func safeOperatorError(err error) string {
	if err == nil {
		return "unknown error"
	}
	value := redact.Credentials(redact.URLs(strings.TrimSpace(err.Error())))
	value = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > 500 {
		value = value[:500] + "..."
	}
	return value
}

type createActionRequestFunc func(failureID, kind, login, userToken, instruction, supersedesID string) (actions.ActionRequestView, error)

func createActionRequestHandler(run createActionRequestFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.IdentityFrom(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var body struct {
			Instruction         string `json:"instruction"`
			SupersedesRequestID string `json:"supersedes_request_id"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
		if err := decoder.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		var extra any
		if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		view, err := run(
			r.PathValue("id"),
			r.PathValue("action"),
			identity.Login,
			identity.Token,
			strings.TrimSpace(body.Instruction),
			strings.TrimSpace(body.SupersedesRequestID),
		)
		if err != nil {
			writeActionError(w, r.PathValue("id"), identity.Login, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(view)
	})
}

type getActionRequestFunc func(id, login string) (actions.ActionRequestView, error)

func getActionRequestHandler(run getActionRequestFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.IdentityFrom(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		view, err := run(r.PathValue("id"), identity.Login)
		if err != nil {
			writeActionError(w, r.PathValue("id"), identity.Login, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(view)
	})
}

type confirmActionRequestFunc func(context.Context, string, string, string) (string, error)

func confirmActionRequestHandler(timeout time.Duration, run confirmActionRequestFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.IdentityFrom(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		url, err := run(ctx, r.PathValue("id"), identity.Login, identity.Token)
		if err != nil {
			writeActionError(w, r.PathValue("id"), identity.Login, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"url": url})
	})
}

type cancelActionRequestFunc func(context.Context, string, string) (actions.ActionRequestView, error)

func cancelActionRequestHandler(timeout time.Duration, run cancelActionRequestFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.IdentityFrom(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		view, err := run(ctx, r.PathValue("id"), identity.Login)
		if err != nil {
			writeActionError(w, r.PathValue("id"), identity.Login, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if view.Status == actions.RequestCancelling {
			w.WriteHeader(http.StatusAccepted)
		}
		_ = json.NewEncoder(w).Encode(view)
	})
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
			// cached forever; a content change yields a new name. Root assets keep
			// stable names and must revalidate on deploy.
			if strings.HasPrefix(clean, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "no-cache")
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

// noListFS wraps an http.FileSystem to disable directory listings and hide
// operational files: opening a directory or a non-published file (the AI cache
// and write-automation state) returns os.ErrNotExist, so http.FileServer
// responds 404 instead of listing the tree or serving operational metadata.
type noListFS struct{ fs http.FileSystem }

func (f noListFS) Open(name string) (http.File, error) {
	if invalidDataPath(name) || hiddenDataPath(name) || hiddenDataBasename(name) {
		return nil, os.ErrNotExist
	}
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

func invalidDataPath(name string) bool {
	if !utf8.ValidString(name) || strings.ContainsRune(name, '\\') {
		return true
	}
	return strings.IndexFunc(name, unicode.IsControl) >= 0
}

func hiddenDataPath(name string) bool {
	for _, segment := range strings.Split(path.Clean("/"+name), "/") {
		if strings.HasPrefix(segment, ".") && segment != "." && segment != ".." {
			return true
		}
	}
	return false
}

func hiddenDataBasename(name string) bool {
	base := path.Base(path.Clean("/" + name))
	for _, hidden := range output.NonPublishedFiles {
		if strings.EqualFold(base, hidden) {
			return true
		}
	}
	return false
}
