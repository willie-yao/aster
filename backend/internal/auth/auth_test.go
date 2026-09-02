package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMiddleware_StatusMapping(t *testing.T) {
	cases := []struct {
		name     string
		auth     Authenticator
		want     int
		wantBody string
	}{
		{name: "admin 200", auth: staticAuthenticator{id: &Identity{Login: "alice"}}, want: http.StatusOK, wantBody: "handled alice"},
		{name: "no token 401", auth: staticAuthenticator{err: ErrNoToken}, want: http.StatusUnauthorized},
		{name: "invalid 401", auth: staticAuthenticator{err: ErrInvalidToken}, want: http.StatusUnauthorized},
		{name: "not admin 403", auth: staticAuthenticator{err: ErrNotAdmin}, want: http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotIdentity *Identity
			h := Middleware(tc.auth, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				id, _ := IdentityFrom(r.Context())
				gotIdentity = id
				w.Write([]byte("handled " + id.Login))
			}))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/failures/x/create-issue", nil))
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
			if tc.want == http.StatusOK && (gotIdentity == nil || gotIdentity.Login != "alice") {
				t.Errorf("identity not propagated to handler: %+v", gotIdentity)
			}
			if tc.wantBody != "" && rec.Body.String() != tc.wantBody {
				t.Errorf("body = %q, want %q", rec.Body.String(), tc.wantBody)
			}
		})
	}
}

func TestMiddlewareAddsPrivateHeadersOnSuccessAndFailure(t *testing.T) {
	for _, testCase := range []struct {
		name string
		auth Authenticator
		want int
	}{
		{name: "success", auth: staticAuthenticator{id: &Identity{Login: "alice"}}, want: http.StatusNoContent},
		{name: "failure", auth: staticAuthenticator{err: ErrNoToken}, want: http.StatusUnauthorized},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			handler := Middleware(testCase.auth, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
			if recorder.Code != testCase.want {
				t.Fatalf("status = %d", recorder.Code)
			}
			if got := recorder.Header().Get("Cache-Control"); got != "private, no-store" {
				t.Fatalf("Cache-Control = %q", got)
			}
			vary := strings.Join(recorder.Header().Values("Vary"), ",")
			if !strings.Contains(vary, "Cookie") || !strings.Contains(vary, "Authorization") {
				t.Fatalf("Vary = %q", vary)
			}
		})
	}
}

type staticAuthenticator struct {
	id  *Identity
	err error
}

func (a staticAuthenticator) Authenticate(context.Context, *http.Request) (*Identity, error) {
	return a.id, a.err
}

func TestSetPrivateResponseHeadersPreservesVary(t *testing.T) {
	header := http.Header{"Vary": {"Accept-Encoding"}}
	SetPrivateResponseHeaders(header)
	SetPrivateResponseHeaders(header)
	got := strings.Join(header.Values("Vary"), ",")
	for _, value := range []string{"Accept-Encoding", "Cookie", "Authorization"} {
		if strings.Count(strings.ToLower(got), strings.ToLower(value)) != 1 {
			t.Fatalf("Vary = %q", got)
		}
	}
}
