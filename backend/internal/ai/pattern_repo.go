package ai

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/actionverify"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
)

// githubRepoReader is a bound tools.RepoReader over one GitHub repo at a ref.
// It backs the pattern agent's repotree tools so the correlation model can grep
// and read real source files before naming a fix target.
//
// ListTree uses the recursive git-trees API (one call, memoized by the tool
// Cache), so it needs a token to clear GitHub's 60/hr anonymous limit on a busy
// run. ReadFile uses the raw.githubusercontent.com CDN, which serves public
// files without a token or API-rate limit, so file reads never spend the token
// budget.
type githubRepoReader struct {
	owner  string
	repo   string
	ref    string
	token  string
	client *http.Client
	mu     sync.Mutex
}

func (r *githubRepoReader) SourceIdentity() (string, string, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.owner, r.repo, r.ref
}
func (r *githubRepoReader) ResolveRef(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ref != "HEAD" {
		return nil
	}
	u := fmt.Sprintf("%s/repos/%s/%s/commits/HEAD", githubAPIBase, r.owner, r.repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("resolving %s/%s HEAD: %s", r.owner, r.repo, resp.Status)
	}
	var out struct {
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return err
	}
	if !fullSourceCommitRE.MatchString(out.SHA) {
		return fmt.Errorf("resolving %s/%s HEAD returned invalid commit", r.owner, r.repo)
	}
	r.ref = strings.ToLower(out.SHA)
	return nil
}

func (r *githubRepoReader) resolvedRef() string { r.mu.Lock(); defer r.mu.Unlock(); return r.ref }

var githubAPIBase = "https://api.github.com"
var githubArchiveHost = "codeload.github.com"

// NewGitHubRepoReader binds a reader to owner/repo at ref. Empty ref means the
// default branch (HEAD). Empty token falls back to anonymous access. The
// returned reader grounds the pattern agent's repotree tools.
func NewGitHubRepoReader(owner, repo, ref, token string) tools.RepoReader {
	if ref == "" {
		ref = "HEAD"
	}
	return &githubRepoReader{
		owner:  owner,
		repo:   repo,
		ref:    ref,
		token:  token,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

// ListTree returns the repo's blob paths at the bound ref via the recursive
// git-trees API.
func (r *githubRepoReader) ListTree(ctx context.Context) ([]string, error) {
	ref := r.resolvedRef()
	u := fmt.Sprintf("%s/repos/%s/%s/git/trees/%s?recursive=1", githubAPIBase,
		r.owner, r.repo, url.PathEscape(ref))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "prow-ai-dashboard")
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("listing %s/%s tree: %s", r.owner, r.repo, resp.Status)
	}
	var out struct {
		Tree []struct {
			Path string `json:"path"`
			Type string `json:"type"`
		} `json:"tree"`
	}
	if err := json.Unmarshal(rb, &out); err != nil {
		return nil, fmt.Errorf("decoding %s/%s tree: %w", r.owner, r.repo, err)
	}
	paths := make([]string, 0, len(out.Tree))
	for _, e := range out.Tree {
		if e.Type == "blob" && e.Path != "" {
			paths = append(paths, e.Path)
		}
	}
	return paths, nil
}

// ReadFile returns a file's content at the bound ref. Authenticated readers use
// the GitHub contents API so private repositories work. Anonymous readers use
// the public raw CDN. found is false when the file does not exist.
func (r *githubRepoReader) ReadFile(ctx context.Context, path string) (string, bool, error) {
	var err error
	path, err = artifacts.SafePath(strings.TrimSpace(path))
	if err != nil {
		return "", false, fmt.Errorf("invalid repository path: %w", err)
	}
	if path == "" {
		return "", false, fmt.Errorf("invalid repository path: path is empty")
	}
	escaped := strings.Join(mapSegments(strings.Split(path, "/"), url.PathEscape), "/")
	u := fmt.Sprintf("%s/%s/%s/%s/%s", rawContentBase, r.owner, r.repo, url.PathEscape(r.resolvedRef()), escaped)
	if r.token != "" {
		u = fmt.Sprintf("%s/repos/%s/%s/contents/%s?ref=%s", githubAPIBase, r.owner, r.repo, escaped, url.QueryEscape(r.resolvedRef()))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", false, err
	}
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
		req.Header.Set("Accept", "application/vnd.github.raw+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	}
	req.Header.Set("User-Agent", "prow-ai-dashboard")
	resp, err := r.client.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("reading %s: %s", path, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", false, fmt.Errorf("reading %s: %w", path, err)
	}
	return string(body), true, nil
}

const (
	maxSourceArchiveCompressedBytes = 64 << 20
	maxSourceArchiveExpandedBytes   = 256 << 20
	maxSourceGoBytes                = 32 << 20
	maxSourceFileBytes              = 8 << 20
)

// ReadSourceArchive fetches bounded Go source and the complete regular-file path set.
func (r *githubRepoReader) ReadSourceArchive(ctx context.Context) (actionverify.Archive, error) {
	ref := r.resolvedRef()
	u := fmt.Sprintf("%s/repos/%s/%s/tarball/%s", githubAPIBase, r.owner, r.repo, url.PathEscape(ref))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return actionverify.Archive{}, err
	}
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "prow-ai-dashboard")
	client := *r.client
	previousRedirect := client.CheckRedirect
	client.CheckRedirect = func(redirect *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("stopped after 10 source archive redirects")
		}
		if previousRedirect != nil {
			if err := previousRedirect(redirect, via); err != nil {
				return err
			}
		}
		if r.token == "" || len(via) == 0 {
			return nil
		}
		targetHost := redirect.URL.Hostname()
		sameOrigin := strings.EqualFold(redirect.URL.Scheme, via[0].URL.Scheme) && strings.EqualFold(redirect.URL.Host, via[0].URL.Host)
		if !sameOrigin && (redirect.URL.Scheme != "https" || !strings.EqualFold(targetHost, githubArchiveHost)) {
			return fmt.Errorf("refusing authenticated source archive redirect to %s", redirect.URL.Host)
		}
		redirect.Header.Set("Authorization", "Bearer "+r.token)
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return actionverify.Archive{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return actionverify.Archive{}, fmt.Errorf("reading %s/%s source archive: %s", r.owner, r.repo, resp.Status)
	}
	gzipReader, err := gzip.NewReader(io.LimitReader(resp.Body, maxSourceArchiveCompressedBytes+1))
	if err != nil {
		return actionverify.Archive{}, fmt.Errorf("opening %s/%s source archive: %w", r.owner, r.repo, err)
	}
	defer gzipReader.Close()

	archive := actionverify.Archive{Paths: map[string]bool{}, GoFiles: map[string]string{}}
	expandedBytes := int64(0)
	goBytes := int64(0)
	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return actionverify.Archive{}, fmt.Errorf("reading %s/%s source archive: %w", r.owner, r.repo, err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		if header.Size < 0 || expandedBytes+header.Size > maxSourceArchiveExpandedBytes {
			return actionverify.Archive{}, fmt.Errorf("source archive exceeds expanded-byte limit")
		}
		expandedBytes += header.Size
		_, filePath, ok := strings.Cut(strings.TrimPrefix(header.Name, "./"), "/")
		if !ok || filePath == "" {
			continue
		}
		clean, err := artifacts.SafePath(filePath)
		if err != nil || clean == "" {
			return actionverify.Archive{}, fmt.Errorf("source archive contains unsafe path %q", filePath)
		}
		archive.Paths[clean] = true
		if !strings.HasSuffix(clean, ".go") {
			continue
		}
		if header.Size > maxSourceFileBytes || goBytes+header.Size > maxSourceGoBytes {
			return actionverify.Archive{}, fmt.Errorf("source archive Go files exceed verification limits")
		}
		body, err := io.ReadAll(io.LimitReader(tarReader, maxSourceFileBytes+1))
		if err != nil || int64(len(body)) != header.Size {
			return actionverify.Archive{}, fmt.Errorf("reading source archive file %s: incomplete content", clean)
		}
		goBytes += int64(len(body))
		archive.GoFiles[clean] = string(body)
	}
	return archive, nil
}

// mapSegments applies f to each element, used to escape path segments while
// preserving the slashes between them.
func mapSegments(in []string, f func(string) string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = f(s)
	}
	return out
}
