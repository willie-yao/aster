package transport

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/willie-yao/aster/backend/internal/modelprovider"
)

type unsupportedTransport struct {
	api string
}

func (t unsupportedTransport) Complete(context.Context, Request) (*Response, error) {
	return nil, fmt.Errorf("unsupported AI API %q", t.api)
}

const defaultModelHTTPResponseBytes int64 = 8 << 20

func readModelResponseBody(body io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		limit = defaultModelHTTPResponseBytes
	}
	raw, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("model response exceeds %d bytes", limit)
	}
	return raw, nil
}

// httpAPIClient holds the connection pool and headers shared by model API
// transports and the best-effort /models probe.
type httpAPIClient struct {
	httpClient            *http.Client
	endpoint              string
	token                 string
	extraHeaders          map[string]string
	serviceTier           string
	onServiceTierFallback ServiceTierFallbackFunc
}

func newHTTPAPIClient(endpoint, token string, extraHeaders map[string]string) *httpAPIClient {
	provider := http.DefaultTransport.(*http.Transport).Clone()
	provider.MaxIdleConns = 32
	provider.MaxIdleConnsPerHost = 16
	return &httpAPIClient{
		// Request deadlines come from the caller's context. A fixed client timeout
		// would override per-failure budgets for slow reasoning endpoints.
		httpClient: &http.Client{
			Transport:     provider,
			CheckRedirect: modelRedirectPolicy(endpoint),
		},
		endpoint:     endpoint,
		token:        token,
		extraHeaders: extraHeaders,
	}
}

func modelRedirectPolicy(endpoint string) func(*http.Request, []*http.Request) error {
	configured, err := url.Parse(endpoint)
	return func(next *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("model endpoint stopped after 10 redirects")
		}
		if err != nil || !strings.EqualFold(next.URL.Scheme, configured.Scheme) || !strings.EqualFold(next.URL.Host, configured.Host) {
			return fmt.Errorf("model endpoint redirected to a different origin")
		}
		return nil
	}
}

func (c *httpAPIClient) setRequestHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	for name, value := range modelprovider.EndpointHeaders(c.endpoint) {
		req.Header.Set(name, value)
	}
	for k, v := range c.extraHeaders {
		req.Header.Set(k, v)
	}
}
