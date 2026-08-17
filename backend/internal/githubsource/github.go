// Package githubsource reads exact repository files from GitHub at a pinned
// revision. It is read-only and bounded, and backs source-grounded validation
// of model-generated citations.
package githubsource

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
)

const (
	maxSourceFileBytes   = 4 << 20
	maxSourceTreeBytes   = 8 << 20
	maxSourceTreeEntries = 100000
)

// Reader reads exact public or token-authenticated GitHub source.
type Reader struct {
	base  *url.URL
	api   *url.URL
	token string
	http  *http.Client
}

// NewReader builds a bounded raw GitHub source client.
func NewReader(base, token string) *Reader {
	base = strings.TrimSpace(base)
	if base == "" {
		base = "https://raw.githubusercontent.com"
	}
	parsed, _ := url.Parse(strings.TrimRight(base, "/"))
	api, _ := url.Parse("https://api.github.com")
	if parsed != nil && !strings.EqualFold(parsed.Hostname(), "raw.githubusercontent.com") {
		copy := *parsed
		copy.Path = strings.TrimRight(copy.Path, "/")
		api = &copy
	}
	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 0 && !strings.EqualFold(req.URL.Host, via[0].URL.Host) {
				req.Header.Del("Authorization")
			}
			return nil
		},
	}
	return &Reader{base: parsed, api: api, token: strings.TrimSpace(token), http: client}
}

// ListFiles lists regular files at the pinned revision.
func (r *Reader) ListFiles(ctx context.Context, repo sourceinvestigation.Repository) ([]string, error) {
	if r == nil || r.api == nil || r.http == nil {
		return nil, fmt.Errorf("source reader is not configured")
	}
	endpoint := *r.api
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/repos/" + repo.Owner + "/" + repo.Name + "/git/trees/" + repo.Revision
	query := endpoint.Query()
	query.Set("recursive", "1")
	endpoint.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := r.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSourceTreeBytes+1))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK || len(body) > maxSourceTreeBytes {
		return nil, fmt.Errorf("GitHub source tree is unavailable")
	}
	var payload struct {
		Truncated bool `json:"truncated"`
		Tree      []struct {
			Path string `json:"path"`
			Type string `json:"type"`
		} `json:"tree"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.Truncated || len(payload.Tree) > maxSourceTreeEntries {
		return nil, fmt.Errorf("GitHub source tree is invalid")
	}
	files := make([]string, 0, len(payload.Tree))
	for _, entry := range payload.Tree {
		clean := path.Clean(strings.TrimSpace(entry.Path))
		if entry.Type != "blob" || clean == "." || clean == ".." || clean != entry.Path || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") || strings.Contains(clean, "\\") {
			continue
		}
		files = append(files, clean)
	}
	return files, nil
}

// ReadFile fetches one path at the pinned revision.
func (r *Reader) ReadFile(
	ctx context.Context,
	repo sourceinvestigation.Repository,
	file string,
) (string, error) {
	if r == nil || r.base == nil || r.http == nil {
		return "", fmt.Errorf("source reader is not configured")
	}
	clean := path.Clean(strings.TrimSpace(file))
	if clean == "." || clean == ".." || clean != file || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") || strings.Contains(clean, "\\") {
		return "", fmt.Errorf("unsafe source path")
	}
	endpoint := *r.base
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/" + repo.Owner + "/" + repo.Name + "/" + repo.Revision + "/" + clean
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return "", err
	}
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close() //nolint:errcheck
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSourceFileBytes+1))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub source returned HTTP %d", resp.StatusCode)
	}
	if len(body) > maxSourceFileBytes {
		return "", fmt.Errorf("source file exceeds %d bytes", maxSourceFileBytes)
	}
	if bytes.IndexByte(body, 0) >= 0 {
		return "", fmt.Errorf("source file is binary")
	}
	return string(body), nil
}
