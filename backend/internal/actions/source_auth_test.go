package actions

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/willie-yao/aster/backend/internal/githubapp"
)

func TestAnalysisSourcePreflightUsesReadAuth(t *testing.T) {
	t.Setenv("BOT_TOKEN", "write-identity")
	for _, token := range []string{"read-identity", ""} {
		t.Run(fmt.Sprintf("authenticated=%t", token != ""), func(t *testing.T) {
			calls := 0
			github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				calls++
				wantAuth := ""
				if token != "" {
					wantAuth = "Bearer " + token
				}
				if req.Method != http.MethodGet || req.Header.Get("Authorization") != wantAuth {
					t.Errorf("unexpected source preflight method=%s read-auth=%t", req.Method, req.Header.Get("Authorization") == wantAuth)
				}
				switch req.URL.Path {
				case "/repos/example/project":
					_, _ = fmt.Fprint(w, `{"default_branch":"main"}`)
				case "/repos/example/project/git/ref/heads/main":
					_, _ = fmt.Fprint(w, `{"object":{"sha":"`+analysisFixRevision+`"}}`)
				case "/repos/example/project/git/commits/" + analysisFixRevision:
					_, _ = fmt.Fprint(w, `{"tree":{"sha":"tree"}}`)
				default:
					t.Errorf("unexpected source preflight path %s", req.URL.Path)
					http.NotFound(w, req)
				}
			}))
			defer github.Close()
			original := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = original })
			transport := original.(*http.Transport).Clone()
			transport.Proxy = nil
			transport.DialTLSContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				if address != "api.github.com:443" {
					return nil, fmt.Errorf("external request blocked by source preflight test")
				}
				return (&net.Dialer{}).DialContext(ctx, network, github.Listener.Addr().String())
			}
			http.DefaultTransport = transport
			t.Cleanup(transport.CloseIdleConnections)
			service := NewService(exactAnalysisConfig(), t.TempDir(), AIConfig{SourceAuth: githubapp.StaticReadAuth(token)})
			base, err := service.analysisRevisionClient().ResolveBase(t.Context(), "example", "project", "")
			if err != nil || base.Branch != "main" || base.HeadSHA != analysisFixRevision || base.TreeSHA != "tree" || calls != 3 {
				t.Fatalf("source preflight base=%+v calls=%d err=%v", base, calls, err)
			}
		})
	}
}
