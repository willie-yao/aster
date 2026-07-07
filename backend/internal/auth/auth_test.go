package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// patWith builds a PATAuthenticator with a stubbed login resolver.
func patWith(admins []string, resolve loginResolver) *PATAuthenticator {
	p := NewPATAuthenticator(admins)
	p.resolve = resolve
	return p
}

func req(authz string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/failures/x/create-issue", nil)
	if authz != "" {
		r.Header.Set("Authorization", authz)
	}
	return r
}

func TestPAT_AdminAllowed(t *testing.T) {
	p := patWith([]string{"Alice"}, func(context.Context, string) (string, error) { return "alice", nil })
	id, err := p.Authenticate(context.Background(), req("Bearer ghp_x"))
	if err != nil {
		t.Fatalf("want ok, got %v", err)
	}
	if id.Login != "alice" || id.Token != "ghp_x" {
		t.Errorf("identity = %+v", id)
	}
}

func TestPAT_TokenFormats(t *testing.T) {
	p := patWith([]string{"alice"}, func(context.Context, string) (string, error) { return "alice", nil })
	for _, h := range []string{"Bearer ghp_x", "token ghp_x", "bearer ghp_x"} {
		if _, err := p.Authenticate(context.Background(), req(h)); err != nil {
			t.Errorf("header %q: %v", h, err)
		}
	}
}

func TestPAT_NonAdminForbidden(t *testing.T) {
	p := patWith([]string{"alice"}, func(context.Context, string) (string, error) { return "mallory", nil })
	_, err := p.Authenticate(context.Background(), req("Bearer ghp_x"))
	if !errors.Is(err, ErrNotAdmin) {
		t.Fatalf("want ErrNotAdmin, got %v", err)
	}
}

func TestPAT_InvalidToken(t *testing.T) {
	p := patWith([]string{"alice"}, func(context.Context, string) (string, error) { return "", errors.New("401") })
	_, err := p.Authenticate(context.Background(), req("Bearer bad"))
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("want ErrInvalidToken, got %v", err)
	}
}

func TestPAT_NoToken(t *testing.T) {
	p := patWith([]string{"alice"}, func(context.Context, string) (string, error) { return "alice", nil })
	_, err := p.Authenticate(context.Background(), req(""))
	if !errors.Is(err, ErrNoToken) {
		t.Fatalf("want ErrNoToken, got %v", err)
	}
}

func TestPAT_EmptyAllowlistFailsClosed(t *testing.T) {
	p := patWith(nil, func(context.Context, string) (string, error) { return "alice", nil })
	_, err := p.Authenticate(context.Background(), req("Bearer ghp_x"))
	if !errors.Is(err, ErrNotAdmin) {
		t.Fatalf("empty allowlist must fail closed, got %v", err)
	}
}

func TestMiddleware_StatusMapping(t *testing.T) {
	ok := func(context.Context, string) (string, error) { return "alice", nil }
	cases := []struct {
		name     string
		resolve  loginResolver
		authz    string
		want     int
		wantBody string
	}{
		{"admin 200", ok, "Bearer ghp_x", http.StatusOK, "handled alice"},
		{"no token 401", ok, "", http.StatusUnauthorized, ""},
		{"invalid 401", func(context.Context, string) (string, error) { return "", errors.New("x") }, "Bearer bad", http.StatusUnauthorized, ""},
		{"not admin 403", func(context.Context, string) (string, error) { return "mallory", nil }, "Bearer ghp_x", http.StatusForbidden, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := patWith([]string{"alice"}, tc.resolve)
			var gotIdentity *Identity
			h := Middleware(p, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				id, _ := IdentityFrom(r.Context())
				gotIdentity = id
				w.Write([]byte("handled " + id.Login))
			}))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req(tc.authz))
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
