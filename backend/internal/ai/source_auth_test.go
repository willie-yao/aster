package ai

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/githubapp"
	"github.com/willie-yao/aster/backend/internal/models"
)

type sourceReadRoundTripper func(*http.Request) (*http.Response, error)

func (f sourceReadRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func sourceAppReadFixture(t *testing.T, installationStatus int) (*githubapp.ReadAuth, *http.Client, *int) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(githubapp.EnvAppID, "1")
	t.Setenv(githubapp.EnvPrivateKey, string(pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	})))
	reads := 0
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/repos/owner/repo/installation":
			if installationStatus != http.StatusOK {
				w.WriteHeader(installationStatus)
				_, _ = fmt.Fprint(w, `{"message":"installation unavailable"}`)
				return
			}
			_, _ = fmt.Fprint(w, `{"id":42}`)
		case "/app/installations/42/access_tokens":
			var scope struct {
				Repositories []string          `json:"repositories"`
				Permissions  map[string]string `json:"permissions"`
			}
			if err := json.NewDecoder(req.Body).Decode(&scope); err != nil {
				t.Errorf("token request: %v", err)
			}
			if len(scope.Repositories) != 1 || scope.Repositories[0] != "repo" || scope.Permissions["contents"] != "read" {
				t.Errorf("token scope = %+v", scope)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"token":"installation-read","expires_at":%q}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		case "/repos/owner/repo/contents/pkg/source.go":
			reads++
			if req.Header.Get("Authorization") != "Bearer installation-read" {
				t.Errorf("source Authorization = %q", req.Header.Get("Authorization"))
			}
			_, _ = fmt.Fprint(w, "package source\n")
		default:
			t.Errorf("unexpected GitHub request %s", req.URL.Path)
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(fixture.Close)
	destination, err := url.Parse(fixture.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: sourceReadRoundTripper(func(req *http.Request) (*http.Response, error) {
		if req.URL.Hostname() != "api.github.com" {
			t.Errorf("unexpected request host %s", req.URL.Host)
			return nil, fmt.Errorf("unexpected request host")
		}
		routed := req.Clone(req.Context())
		routed.URL.Scheme, routed.URL.Host = destination.Scheme, destination.Host
		return fixture.Client().Transport.RoundTrip(routed)
	})}
	auth, err := githubapp.NewReadAuthFromEnv(client, "static-fallback")
	if err != nil {
		t.Fatal(err)
	}
	return auth, client, &reads
}

func TestSourceToolsAndCitationProbesUseAppReadAuth(t *testing.T) {
	auth, client, reads := sourceAppReadFixture(t, http.StatusOK)
	service := NewService(ServiceConfig{SourceRepoOwner: "owner", SourceRepoName: "repo", GitHubReadAuth: auth})
	const revision = "0123456789abcdef0123456789abcdef01234567"
	catalog, err := service.sourceCatalogForBuild(&models.BuildResult{BuildInfo: models.BuildInfo{
		RepoRefs: map[string]string{"owner/repo": "main:" + revision},
	}})
	if err != nil {
		t.Fatal(err)
	}
	source, ok := catalog.Primary()
	if !ok {
		t.Fatal("source catalog omitted the primary repository")
	}
	reader := source.Reader.(*githubRepoReader)
	reader.client = client
	for range 2 {
		content, found, err := reader.ReadFile(t.Context(), "pkg/source.go")
		if err != nil || !found || content != "package source\n" {
			t.Fatalf("source read found=%t err=%v", found, err)
		}
	}
	if got := service.verifyGitHubFileAtRef(t.Context(), client, "owner", "repo", revision, "pkg/source.go"); got != linkPresent {
		t.Fatalf("source citation verification = %v", got)
	}
	if *reads != 3 {
		t.Fatalf("authenticated source reads = %d, want 3", *reads)
	}
}

func TestSourceToolsAndCitationProbesDoNotDowngradeAppFailure(t *testing.T) {
	auth, client, reads := sourceAppReadFixture(t, http.StatusNotFound)
	reader := NewGitHubRepoReader("owner", "repo", "HEAD", auth).(*githubRepoReader)
	reader.client = client
	if _, _, err := reader.ReadFile(t.Context(), "pkg/source.go"); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("source read error = %v", err)
	}
	service := &Service{githubReadAuth: auth}
	if got := service.verifyGitHubFileAtRef(t.Context(), client, "owner", "repo", "HEAD", "pkg/source.go"); got != linkUnverified {
		t.Fatalf("source citation verification = %v", got)
	}
	if *reads != 0 {
		t.Fatalf("source reads after App failure = %d", *reads)
	}
}
