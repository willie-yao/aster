package ghpr

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/githubapp"
)

func TestReadClientStaticAndAnonymous(t *testing.T) {
	for _, token := range []string{"", "read-token"} {
		t.Run(token, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				want := ""
				if token != "" {
					want = "Bearer " + token
				}
				if r.Header.Get("Authorization") != want {
					t.Error("read credential was not applied")
				}
				writeJSON(w, http.StatusOK, map[string]any{"number": 7})
			}))
			defer srv.Close()
			client := NewReadClient(srv.Client(), githubapp.StaticReadAuth(token))
			client.base = srv.URL
			for range 2 {
				pr, err := client.GetPullRequest(t.Context(), "o", "r", 7)
				if err != nil || pr.Number != 7 {
					t.Fatalf("GetPullRequest = %v, %v", pr, err)
				}
			}
			if calls != 2 {
				t.Fatalf("calls = %d, want 2", calls)
			}
		})
	}
}

func TestReadClientRejectsWritesAndNonRepositoryEndpoints(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	client := NewReadClient(srv.Client(), githubapp.StaticReadAuth("read-token"))
	client.base = srv.URL
	if err := client.post(t.Context(), srv.URL+"/repos/o/r/git/refs", nil, nil); err == nil || !strings.Contains(err.Error(), "cannot perform POST") {
		t.Fatalf("post = %v", err)
	}
	if _, err := client.AuthedLogin(t.Context()); err == nil || !strings.Contains(err.Error(), "repository endpoint") {
		t.Fatalf("AuthedLogin = %v", err)
	}
	if calls != 0 {
		t.Fatalf("rejected operations made %d HTTP requests", calls)
	}
}

type readAppTransport func(*http.Request) (*http.Response, error)

func (f readAppTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestReadClientAppScopesRepositoryAndPermission(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ASTER_APP_ID", "1")
	t.Setenv("ASTER_APP_PRIVATE_KEY", string(pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	})))
	minted := map[string]int{}
	client := &http.Client{Transport: readAppTransport(func(req *http.Request) (*http.Response, error) {
		body := ""
		switch req.URL.Path {
		case "/repos/o/r/installation":
			body = `{"id":1}`
		case "/repos/other/r/installation":
			body = `{"id":2}`
		case "/app/installations/1/access_tokens", "/app/installations/2/access_tokens":
			var scope struct {
				Repositories []string          `json:"repositories"`
				Permissions  map[string]string `json:"permissions"`
			}
			if err := json.NewDecoder(req.Body).Decode(&scope); err != nil {
				t.Fatal(err)
			}
			if len(scope.Repositories) != 1 || scope.Repositories[0] != "r" || len(scope.Permissions) != 1 {
				t.Fatal("read token broadened its repository or permissions")
			}
			permission := ""
			for name, level := range scope.Permissions {
				if level != "read" {
					t.Fatal("read token requested write permission")
				}
				permission = name
			}
			token := strings.Split(req.URL.Path, "/")[3] + "-" + permission
			minted[token]++
			body = fmt.Sprintf(`{"token":%q,"expires_at":%q}`, token, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		case "/repos/o/r/pulls/7", "/repos/other/r/pulls/7":
			want := "Bearer 1-pull_requests"
			if strings.Contains(req.URL.Path, "/other/") {
				want = "Bearer 2-pull_requests"
			}
			if req.Header.Get("Authorization") != want {
				t.Fatal("pull request read used another repository or permission's token")
			}
			body = `{"number":7}`
		case "/repos/o/r/git/commits/" + strings.Repeat("a", 40):
			if req.Header.Get("Authorization") != "Bearer 1-contents" {
				t.Fatal("source read did not use a contents-only token")
			}
			body = `{"tree":{"sha":"` + strings.Repeat("b", 40) + `"}}`
		default:
			t.Fatalf("unexpected GitHub request: %s %s", req.Method, req.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	auth, err := githubapp.NewReadAuthFromEnv(client, "static-fallback")
	if err != nil {
		t.Fatal(err)
	}
	reader := NewReadClient(client, auth)
	for _, owner := range []string{"o", "other", "o"} {
		if _, err := reader.GetPullRequest(t.Context(), owner, "r", 7); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := reader.ResolveCommitTree(t.Context(), "o", "r", strings.Repeat("a", 40)); err != nil {
		t.Fatal(err)
	}
	if len(minted) != 3 || minted["1-pull_requests"] != 1 || minted["2-pull_requests"] != 1 || minted["1-contents"] != 1 {
		t.Fatalf("token exchanges = %v", minted)
	}
}
