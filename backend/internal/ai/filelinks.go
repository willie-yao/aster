package ai

import (
	"context"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// File-link verification. The UI links file citations in an analysis to GitHub,
// but a bare path like "test/e2e/clusterctl_upgrade_test.go" is ambiguous: it
// may live in the project's own repo or in an upstream repo, and guessing wrong
// produces a broken link. Browsers can't probe GitHub because of CORS, so the
// fetcher resolves each cited source path to a repo and verifies it exists with
// HTTP 200 against raw.githubusercontent.com, recording only the links that resolve. The
// UI then links a path only if it is present in the verified map.
//
// Resolution is generic: nothing here knows about any specific project or
// ecosystem. A path is resolved against the project's own source repo, then a
// Go vanity import host via `?go-get=1` when the first segment is a host, then
// an "owner/repo/path" GitHub reference. The first interpretation that verifies wins.

// trailingParenRe strips a trailing parenthetical annotation, such as a line
// annotation, before resolving. Mirrors the frontend's fileToUrl cleaning.
var trailingParenRe = regexp.MustCompile(`\s*\(.*\)$`)

// pathTokenRe extracts candidate file paths from prose: one or more
// "/"-separated segments ending in a known extension. Mirrors the frontend
// PATH_RE so the keys it produces match the tokens the UI looks up. Only
// source-file extensions are verified as GitHub links.
var pathTokenRe = regexp.MustCompile(`(?:[\w.-]+/)+[\w.-]+\.(?:go|ya?ml|sh|json|tpl|md|log|txt|xml|out|conf)\b`)

// sourceExtRe matches source-file extensions. Run artifacts are linked against
// the build's GCS tree.
var sourceExtRe = regexp.MustCompile(`\.(?:go|yaml|yml|sh|json|tpl|md)$`)

// goImportMetaRe extracts the go-import meta tag's content. It may span
// multiple lines and has shape "<import-prefix> <vcs> <repo-url>".
var goImportMetaRe = regexp.MustCompile(`(?s)<meta\s+name="go-import"\s+content="([^"]+)"`)

// maxLinkCandidates caps verification work per analysis against pathological
// prose.
const maxLinkCandidates = 60

// rawContentBase and goGetScheme are origins for file-existence checks and
// vanity import resolution. Vars so tests can point them at a stub server.
var (
	rawContentBase = "https://raw.githubusercontent.com"
	goGetScheme    = "https://"
)

// FileLinkResolver verifies source-file links without requiring model runtime setup.
type FileLinkResolver struct {
	service Service
}

// NewFileLinkResolver creates a reusable source-file verifier for one project.
func NewFileLinkResolver(owner, repo string) *FileLinkResolver {
	return &FileLinkResolver{service: Service{sourceRepoOwner: owner, sourceRepoName: repo}}
}

// Resolve returns the verified source-file links for one accepted analysis.
func (r *FileLinkResolver) Resolve(ctx context.Context, client *http.Client, tc *models.TestCase) map[string]string {
	if r == nil {
		return map[string]string{}
	}
	return r.service.resolveFileLinks(ctx, client, tc)
}

// resolveFileLinks builds the verified GitHub link map for one analysis. It
// gathers candidate source paths from relevant_files and the analysis prose,
// resolves each to a verified GitHub blob URL, and returns only the paths that
// resolve. The map is always non-nil so the published JSON carries
// "file_links": {...}. An empty map means "verified, nothing to link" and is
// distinct from absent on older data.
func (s *Service) resolveFileLinks(ctx context.Context, client *http.Client, tc *models.TestCase) map[string]string {
	links := map[string]string{}
	if tc.AIAnalysis == nil {
		return links
	}

	// Collect distinct candidate source paths: explicit relevant_files plus
	// paths cited in the prose.
	seen := map[string]struct{}{}
	add := func(p string) {
		clean := strings.TrimSpace(trailingParenRe.ReplaceAllString(p, ""))
		// Artifact-tree paths are linked client-side against the build's GCS
		// URL; only source files are verified as GitHub links here.
		if clean == "" || !sourceExtRe.MatchString(clean) ||
			strings.HasPrefix(clean, "artifacts/") || strings.HasPrefix(clean, "clusters/") {
			return
		}
		seen[clean] = struct{}{}
	}
	for _, f := range tc.AIAnalysis.RelevantFiles {
		add(f)
	}
	prose := tc.AIAnalysis.RootCause + "\n" + tc.AIAnalysis.SuggestedFix
	if tc.AISummary != nil {
		prose += "\n" + tc.AISummary.Summary
	}
	for _, m := range pathTokenRe.FindAllString(prose, -1) {
		add(m)
	}

	n := 0
	for clean := range seen {
		if n >= maxLinkCandidates {
			break
		}
		n++
		if url, ok := s.resolveSourceLink(ctx, client, clean); ok {
			links[clean] = url
		}
	}
	return links
}

// resolveSourceLink resolves a cleaned source path to a verified GitHub blob
// URL, trying the project repo, then for a leading host segment an explicit
// github.com path or Go vanity import, then an owner/repo/path reference.
// Returns ok=false if nothing verifies.
func (s *Service) resolveSourceLink(ctx context.Context, client *http.Client, clean string) (string, bool) {
	segs := strings.Split(clean, "/")
	if len(segs) < 2 {
		return "", false
	}

	// The project's own repo, with the path as cited. Tried first so a project
	// directory whose name contains a dot, such as ".github/workflows", is not
	// mistaken for a vanity host below.
	if s.sourceRepoOwner != "" && s.sourceRepoName != "" &&
		s.verifyGitHubFile(ctx, client, s.sourceRepoOwner, s.sourceRepoName, clean) {
		return blobURL(s.sourceRepoOwner, s.sourceRepoName, clean), true
	}

	// A leading host segment containing a dot denotes an explicit github.com
	// path or a Go vanity import path.
	if strings.Contains(segs[0], ".") {
		if segs[0] == "github.com" && len(segs) >= 4 {
			owner, repo, inRepo := segs[1], segs[2], strings.Join(segs[3:], "/")
			if s.verifyGitHubFile(ctx, client, owner, repo, inRepo) {
				return blobURL(owner, repo, inRepo), true
			}
			return "", false
		}
		if owner, repo, inRepo, ok := s.resolveVanity(ctx, client, clean); ok &&
			s.verifyGitHubFile(ctx, client, owner, repo, inRepo) {
			return blobURL(owner, repo, inRepo), true
		}
		return "", false
	}

	// An explicit "owner/repo/path" GitHub reference.
	if len(segs) >= 3 {
		owner, repo, inRepo := segs[0], segs[1], strings.Join(segs[2:], "/")
		if s.verifyGitHubFile(ctx, client, owner, repo, inRepo) {
			return blobURL(owner, repo, inRepo), true
		}
	}
	return "", false
}

// resolveVanity resolves a Go vanity import path to its backing GitHub repo via
// the standard `?go-get=1` meta tag, memoized per module. Returns the repo
// owner/name and the file's path within that repo.
func (s *Service) resolveVanity(ctx context.Context, client *http.Client, clean string) (owner, repo, inRepo string, ok bool) {
	segs := strings.Split(clean, "/")
	if len(segs) < 3 {
		return "", "", "", false
	}
	module := segs[0] + "/" + segs[1] // host/first-segment (the usual module root)

	var prefix, ghRepo string
	if cached, hit := s.linkVerifyCache.Load("go-get:" + module); hit {
		v := cached.(vanityResult)
		prefix, ghRepo = v.prefix, v.repo
	} else {
		prefix, ghRepo = fetchGoImport(ctx, client, module)
		s.linkVerifyCache.Store("go-get:"+module, vanityResult{prefix: prefix, repo: ghRepo})
	}
	if ghRepo == "" || prefix == "" || !strings.HasPrefix(clean, prefix+"/") {
		return "", "", "", false
	}
	o, r, ok := ownerRepoFromGitHubURL(ghRepo)
	if !ok {
		return "", "", "", false
	}
	return o, r, strings.TrimPrefix(clean, prefix+"/"), true
}

type vanityResult struct{ prefix, repo string }

// fetchGoImport requests "<scheme><module>?go-get=1" and returns the go-import
// meta's import-path prefix and repo URL. Empty on any failure.
func fetchGoImport(ctx context.Context, client *http.Client, module string) (prefix, repo string) {
	reqCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, goGetScheme+module+"?go-get=1", nil)
	if err != nil {
		return "", ""
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", ""
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", ""
	}
	m := goImportMetaRe.FindSubmatch(body)
	if m == nil {
		return "", ""
	}
	fields := strings.Fields(string(m[1])) // "<prefix> <vcs> <repo-url>"
	if len(fields) < 3 {
		return "", ""
	}
	return fields[0], fields[2]
}

// ownerRepoFromGitHubURL extracts owner/repo from a github.com repo URL.
func ownerRepoFromGitHubURL(url string) (owner, repo string, ok bool) {
	const host = "github.com/"
	i := strings.Index(url, host)
	if i < 0 {
		return "", "", false
	}
	parts := strings.SplitN(strings.TrimSuffix(url[i+len(host):], ".git"), "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// verifyGitHubFile reports whether a file exists in a repo's default branch,
// memoized per run by the raw URL.
func (s *Service) verifyGitHubFile(ctx context.Context, client *http.Client, owner, repo, inRepoPath string) bool {
	rawURL := rawContentBase + "/" + owner + "/" + repo + "/HEAD/" + inRepoPath
	if v, ok := s.linkVerifyCache.Load(rawURL); ok {
		return v.(bool)
	}
	exists := headOK(ctx, client, rawURL)
	s.linkVerifyCache.Store(rawURL, exists)
	return exists
}

func blobURL(owner, repo, inRepoPath string) string {
	return "https://github.com/" + owner + "/" + repo + "/blob/HEAD/" + inRepoPath
}

// headOK issues a bounded HEAD request and reports a 2xx response.
func headOK(ctx context.Context, client *http.Client, url string) bool {
	reqCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodHead, url, nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}
