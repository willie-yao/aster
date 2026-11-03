package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestReadAuthRenewsAndScopesTokens(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var mintCalls, reads int
	fail := false
	var scopes []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			if strings.Contains(r.URL.Path, "/missing/") {
				http.NotFound(w, r)
				return
			}
			_, _ = io.WriteString(w, `{"id":7}`)
		case r.URL.Path == "/app/installations/7/access_tokens":
			mintCalls++
			var body struct {
				Repositories []string          `json:"repositories"`
				Permissions  map[string]string `json:"permissions"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if len(body.Repositories) != 1 || len(body.Permissions) != 1 {
				t.Error("read token was not narrowed")
			}
			for permission, access := range body.Permissions {
				if access != "read" {
					t.Error("read token requested write access")
				}
				scopes = append(scopes, body.Repositories[0]+":"+permission)
			}
			if fail {
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, "secret-from-error-body")
				return
			}
			_, _ = fmt.Fprintf(w, `{"token":"token-%d","expires_at":%q}`, mintCalls, now.Add(time.Hour).Format(time.RFC3339))
		default:
			reads++
			if r.Header.Get("Authorization") != fmt.Sprintf("Bearer token-%d", mintCalls) {
				t.Error("stale or absent credential")
			}
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	defer upstream.Close()
	app, err := New(upstream.Client(), Credentials{AppID: "1", PrivateKey: pkcs1PEM(t, testKey(t))})
	if err != nil {
		t.Fatal(err)
	}
	app.base = upstream.URL
	app.now = func() time.Time { return now }
	auth := &ReadAuth{app: app}
	client := upstream.Client()
	read := func(owner, repo, permission string) error {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, upstream.URL+"/repos/"+owner+"/"+repo+"/contents/a", nil)
		resp, err := auth.Do(client, req, owner, repo, permission)
		if resp != nil {
			_ = resp.Body.Close()
		}
		return err
	}
	for range 2 {
		if err := read("o", "r", "contents"); err != nil {
			t.Fatal(err)
		}
	}
	if mintCalls != 1 {
		t.Fatalf("initial exchanges=%d, want 1", mintCalls)
	}
	now = now.Add(56 * time.Minute)
	if err := read("o", "r", "contents"); err != nil {
		t.Fatal(err)
	}
	if mintCalls != 2 {
		t.Fatalf("renewal exchanges=%d, want 2", mintCalls)
	}
	if err := read("o", "other", "contents"); err != nil {
		t.Fatal(err)
	}
	if err := read("o", "r", "pull_requests"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(scopes, ","); got != "r:contents,r:contents,other:contents,r:pull_requests" {
		t.Fatalf("scopes=%s", got)
	}
	if err := read("missing", "public", "contents"); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("missing installation=%v", err)
	}
	now = now.Add(time.Hour)
	fail = true
	before := reads
	if err := read("o", "r", "contents"); err == nil || strings.Contains(err.Error(), "secret-from-error-body") {
		t.Fatalf("renewal error=%v", err)
	}
	if reads != before {
		t.Fatal("failed renewal sent a repository request")
	}
	fail = false
	if err := read("o", "r", "contents"); err != nil {
		t.Fatal(err)
	}
}

type readRoundTripper func(*http.Request) (*http.Response, error)

func (f readRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func readResponse(req *http.Request, code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Status: fmt.Sprintf("%d %s", code, http.StatusText(code)), Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}
}

func TestReadAuthConcurrentRenewal(t *testing.T) {
	pem := pkcs1PEM(t, testKey(t))
	for _, failure := range []bool{false, true} {
		t.Run(fmt.Sprintf("failure=%t", failure), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				now := time.Now()
				release := make(chan struct{})
				var exchanges atomic.Int32
				client := &http.Client{Transport: readRoundTripper(func(req *http.Request) (*http.Response, error) {
					if strings.HasSuffix(req.URL.Path, "/installation") {
						return readResponse(req, 200, `{"id":7}`), nil
					}
					exchanges.Add(1)
					<-release
					if failure {
						return readResponse(req, 503, "unavailable"), nil
					}
					return readResponse(req, 201, fmt.Sprintf(`{"token":"renewed","expires_at":%q}`, now.Add(time.Hour).Format(time.RFC3339))), nil
				})}
				app, err := New(client, Credentials{AppID: "1", PrivateKey: pem})
				if err != nil {
					t.Fatal(err)
				}
				app.now = func() time.Time { return now }
				app.tokens["read:contents:o/r"] = cachedToken{token: "old", expiresAt: now.Add(4 * time.Minute)}
				var wg sync.WaitGroup
				for range 20 {
					wg.Go(func() {
						token, err := app.installationToken(t.Context(), "o", "r", "contents")
						if failure {
							if err == nil || token != "" {
								t.Error("failed renewal returned a token")
							}
						} else if err != nil || token != "renewed" {
							t.Error("renewal failed")
						}
					})
				}
				synctest.Wait()
				if exchanges.Load() != 1 {
					t.Fatalf("concurrent exchanges=%d", exchanges.Load())
				}
				ctx, cancel := context.WithCancel(t.Context())
				waiter := make(chan error, 1)
				go func() {
					_, err := app.installationToken(ctx, "o", "r", "contents")
					waiter <- err
				}()
				synctest.Wait()
				cancel()
				if err := <-waiter; !errors.Is(err, context.Canceled) {
					t.Errorf("cancelled waiter=%v", err)
				}
				close(release)
				wg.Wait()
				if exchanges.Load() != 1 {
					t.Fatalf("exchanges after completion=%d", exchanges.Load())
				}
			})
		})
	}
}

func TestReadAuthPrecedence(t *testing.T) {
	pem := pkcs1PEM(t, testKey(t))
	for _, tc := range []struct {
		name, id, key      string
		wantApp, wantError bool
	}{
		{name: "static"},
		{name: "app", id: "1", key: pem, wantApp: true},
		{name: "missing key", id: "1", wantError: true},
		{name: "missing id", key: pem, wantError: true},
		{name: "invalid key", id: "1", key: "invalid", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvAppID, tc.id)
			t.Setenv(EnvPrivateKey, tc.key)
			auth, err := NewReadAuthFromEnv(nil, "static-secret")
			if (err != nil) != tc.wantError {
				t.Fatalf("constructor error=%v", err)
			}
			if tc.wantError {
				return
			}
			if (auth.app != nil) != tc.wantApp {
				t.Fatal("wrong authentication precedence")
			}
			if tc.wantApp && auth.token != "" {
				t.Fatal("App retained fallback credential")
			}
		})
	}
}

func TestReadAuthStaticAnonymousAndRedirects(t *testing.T) {
	for _, token := range []string{"", "fixed"} {
		t.Run("token="+token, func(t *testing.T) {
			auth := StaticReadAuth(token)
			calls := 0
			client := &http.Client{Transport: readRoundTripper(func(req *http.Request) (*http.Response, error) {
				calls++
				want := ""
				if token != "" {
					want = "Bearer " + token
				}
				if req.Header.Get("Authorization") != want {
					t.Error("wrong authorization")
				}
				return readResponse(req, 200, "ok"), nil
			})}
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://api.github.com/repos/o/r/contents/a", nil)
			for range 2 {
				resp, err := auth.Do(client, req, "o", "r", "contents")
				if err != nil {
					t.Fatal(err)
				}
				_ = resp.Body.Close()
			}
			if calls != 2 || req.Header.Get("Authorization") != "" {
				t.Fatal("request was mutated or skipped")
			}
		})
	}
	for _, tc := range []struct {
		target  string
		allowed bool
	}{
		{"https://codeload.github.com/o/r/legacy.tar.gz/main", true},
		{"https://api.github.com/repos/o/r/tarball/sha", true},
		{"https://codeload.github.com/o/other/legacy.tar.gz/main", false},
		{"https://api.github.com/repos/o/other/tarball/main", false},
		{"https://api.github.com/repos/o/r/../other/tarball/main", false},
		{"https://api.github.com/repos/o/r/%2e%2e/other/tarball/main", false},
		{"https://attacker.example/repos/o/r/tarball/main", false},
		{"http://codeload.github.com/o/r/legacy.tar.gz/main", false},
	} {
		t.Run(tc.target, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: readRoundTripper(func(req *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					resp := readResponse(req, 302, "")
					resp.Header.Set("Location", tc.target)
					return resp, nil
				}
				if req.Header.Get("Authorization") != "Bearer fixed" {
					t.Error("redirect lost authentication")
				}
				return readResponse(req, 200, "ok"), nil
			})}
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://api.github.com/repos/o/r/tarball/main", nil)
			resp, err := StaticReadAuth("fixed").Do(client, req, "o", "r", "contents")
			if resp != nil {
				_ = resp.Body.Close()
			}
			if (err == nil) != tc.allowed {
				t.Fatalf("redirect err=%v", err)
			}
			if !tc.allowed && calls != 1 {
				t.Fatal("credential sent outside repository")
			}
		})
	}
}

func TestReadTokensDoNotReuseWriteTokens(t *testing.T) {
	server := newAppServer(t)
	app := server.client(t)
	writeToken, err := app.InstallationToken(t.Context(), "o", "r")
	if err != nil {
		t.Fatal(err)
	}
	readToken, err := app.installationToken(t.Context(), "o", "r", "contents")
	if err != nil {
		t.Fatal(err)
	}
	if writeToken == readToken {
		t.Fatal("read reused write token")
	}
	again, err := app.InstallationToken(t.Context(), "o", "r")
	if err != nil || again != writeToken {
		t.Fatal("read replaced write token")
	}
}

func TestReadAuthRenewalWaiterSurvivesLeaderCancellation(t *testing.T) {
	pem := pkcs1PEM(t, testKey(t))
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprintf("deadline=%t", deadline), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				now := time.Now()
				var exchanges atomic.Int32
				client := &http.Client{Transport: readRoundTripper(func(req *http.Request) (*http.Response, error) {
					if strings.HasSuffix(req.URL.Path, "/installation") {
						return readResponse(req, 200, `{"id":7}`), nil
					}
					if exchanges.Add(1) == 1 {
						<-req.Context().Done()
						return nil, req.Context().Err()
					}
					return readResponse(req, 201, fmt.Sprintf(`{"token":"renewed","expires_at":%q}`, now.Add(time.Hour).Format(time.RFC3339))), nil
				})}
				app, err := New(client, Credentials{AppID: "1", PrivateKey: pem})
				if err != nil {
					t.Fatal(err)
				}
				app.now = func() time.Time { return now }
				app.tokens["read:contents:o/r"] = cachedToken{token: "old", expiresAt: now.Add(4 * time.Minute)}
				leaderCtx, cancel := context.WithCancel(t.Context())
				defer cancel()
				wantErr := context.Canceled
				if deadline {
					leaderCtx, cancel = context.WithTimeout(t.Context(), time.Second)
					defer cancel()
					wantErr = context.DeadlineExceeded
				}
				leaderDone := make(chan error, 1)
				go func() {
					token, err := app.installationToken(leaderCtx, "o", "r", "contents")
					if token != "" {
						t.Error("cancelled leader returned a token")
					}
					leaderDone <- err
				}()
				synctest.Wait()
				waiterDone := make(chan error, 1)
				go func() {
					token, err := app.installationToken(t.Context(), "o", "r", "contents")
					if token != "renewed" {
						t.Error("live waiter did not renew")
					}
					waiterDone <- err
				}()
				synctest.Wait()
				if exchanges.Load() != 1 {
					t.Fatalf("initial exchanges=%d, want 1", exchanges.Load())
				}
				if deadline {
					time.Sleep(time.Second)
				} else {
					cancel()
				}
				if err := <-leaderDone; !errors.Is(err, wantErr) {
					t.Fatalf("leader error=%v", err)
				}
				if err := <-waiterDone; err != nil {
					t.Fatalf("waiter error=%v", err)
				}
				if exchanges.Load() != 2 {
					t.Fatalf("renewal exchanges=%d, want 2", exchanges.Load())
				}
			})
		})
	}
}

func TestReadAuthRenewalWaiterDoesNotRetryClientTimeout(t *testing.T) {
	pem := pkcs1PEM(t, testKey(t))
	synctest.Test(t, func(t *testing.T) {
		now := time.Now()
		var exchanges atomic.Int32
		client := &http.Client{Timeout: time.Second, Transport: readRoundTripper(func(req *http.Request) (*http.Response, error) {
			if strings.HasSuffix(req.URL.Path, "/installation") {
				return readResponse(req, 200, `{"id":7}`), nil
			}
			if exchanges.Add(1) == 1 {
				<-req.Context().Done()
				return nil, req.Context().Err()
			}
			return readResponse(req, 201, fmt.Sprintf(`{"token":"unexpected-retry","expires_at":%q}`, now.Add(time.Hour).Format(time.RFC3339))), nil
		})}
		app, err := New(client, Credentials{AppID: "1", PrivateKey: pem})
		if err != nil {
			t.Fatal(err)
		}
		app.now = func() time.Time { return now }
		leaderCtx := t.Context()
		results := make(chan error, 2)
		request := func() {
			token, err := app.installationToken(leaderCtx, "o", "r", "contents")
			if token != "" {
				t.Error("timeout returned a token")
			}
			results <- err
		}
		go request()
		synctest.Wait()
		go request()
		synctest.Wait()
		time.Sleep(time.Second)
		for range 2 {
			if err := <-results; !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("timeout error=%v", err)
			}
		}
		if err := leaderCtx.Err(); err != nil {
			t.Fatalf("caller context ended: %v", err)
		}
		if exchanges.Load() != 1 {
			t.Fatalf("client timeout triggered %d exchanges, want 1", exchanges.Load())
		}
	})
}
