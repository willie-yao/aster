package ai

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
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
var pathTokenRe = regexp.MustCompile(`(?:[\w.@-]+/)+[\w.-]+\.(?:go|ya?ml|sh|json|tpl|md|log|txt|xml|out|conf|py|js|jsx|ts|tsx|java|rs|c|cc|cpp|h|hpp|proto|sql|toml|mod|sum)\b`)

// sourceExtRe matches source-file extensions. Run artifacts are linked against
// the build's GCS tree.
var sourceExtRe = regexp.MustCompile(`\.(?:go|yaml|yml|sh|json|tpl|md|py|js|jsx|ts|tsx|java|rs|c|cc|cpp|h|hpp|proto|sql|toml|mod|sum)$`)

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
func NewFileLinkResolver(owner, repo string, token ...string) *FileLinkResolver {
	r := &FileLinkResolver{}
	r.service.sourceRepoOwner = owner
	r.service.sourceRepoName = repo
	if len(token) > 0 {
		r.service.githubReadToken = token[0]
	}
	return r
}

// Resolve returns the verified source-file links for one accepted analysis.
func (r *FileLinkResolver) Resolve(ctx context.Context, client *http.Client, tc *models.TestCase) map[string]string {
	if r == nil {
		return map[string]string{}
	}
	return r.service.resolveFileLinks(ctx, client, tc)
}

// ResolveAtRef verifies source links at one immutable build commit.
func (r *FileLinkResolver) ResolveAtRef(ctx context.Context, client *http.Client, tc *models.TestCase, ref string) map[string]string {
	if r == nil {
		return map[string]string{}
	}
	return r.service.resolveFileLinksAtRef(ctx, client, tc, ref)
}

// resolveFileLinks builds the verified GitHub link map for one analysis. It
// gathers candidate source paths from relevant_files and the analysis prose,
// resolves each to a verified GitHub blob URL, and returns only the paths that
// resolve. The map is always non-nil so the published JSON carries
// "file_links": {...}. An empty map means "verified, nothing to link" and is
// distinct from absent on older data.
func (s *Service) resolveFileLinks(ctx context.Context, client *http.Client, tc *models.TestCase) map[string]string {
	return s.resolveFileLinksAtRef(ctx, client, tc, "HEAD")
}

func (s *Service) resolveFileLinksAtRef(ctx context.Context, client *http.Client, tc *models.TestCase, ref string) map[string]string {
	links := map[string]string{}
	if tc.AIAnalysis == nil {
		return links
	}

	// Collect distinct candidate source paths from relevant files, source-search
	// suggestions, and paths cited in the prose.
	seen := map[string]struct{}{}
	candidates := make([]string, 0, maxLinkCandidates)
	add := func(p string) {
		clean := strings.TrimSpace(trailingParenRe.ReplaceAllString(p, ""))
		// Artifact-tree paths are linked client-side against the build's GCS
		// URL; only source files are verified as GitHub links here.
		if clean == "" || !sourceExtRe.MatchString(clean) ||
			strings.HasPrefix(clean, "artifacts/") || strings.HasPrefix(clean, "clusters/") {
			return
		}
		if _, ok := seen[clean]; ok {
			return
		}
		seen[clean] = struct{}{}
		candidates = append(candidates, clean)
	}
	for _, f := range tc.AIAnalysis.RelevantFiles {
		add(f)
	}
	for _, f := range tc.AIAnalysis.SearchSuggestions {
		add(f)
	}
	prose := tc.AIAnalysis.RootCause + "\n" + tc.AIAnalysis.SuggestedFix
	if tc.AISummary != nil {
		prose += "\n" + tc.AISummary.Summary
	}
	for _, m := range pathTokenRe.FindAllString(prose, -1) {
		add(m)
	}

	for index, clean := range candidates {
		if index >= maxLinkCandidates {
			break
		}
		var url string
		var ok bool
		if ref != "" && ref != "HEAD" {
			url, ok = s.resolveConfiguredSourceLinkAtRef(ctx, client, clean, ref)
		} else {
			url, ok = s.resolveSourceLink(ctx, client, clean)
		}
		if ok {
			links[clean] = url
		}
	}
	return links
}

func (s *Service) resolveConfiguredSourceLinkAtRef(ctx context.Context, client *http.Client, clean, ref string) (string, bool) {
	path := normalizeConfiguredSourcePath(clean, s.sourceRepoOwner, s.sourceRepoName)
	path, err := artifacts.SafePath(path)
	if err != nil || path == "" || !s.verifyGitHubFileAtRef(ctx, client, s.sourceRepoOwner, s.sourceRepoName, ref, path) {
		return "", false
	}
	return blobURLAtRef(s.sourceRepoOwner, s.sourceRepoName, ref, path), true
}

func normalizeConfiguredSourcePath(clean, owner, repo string) string {
	clean = strings.TrimSpace(clean)
	blobPrefix := "https://github.com/" + owner + "/" + repo + "/blob/"
	schemeLessBlobPrefix := strings.TrimPrefix(blobPrefix, "https://")
	if strings.HasPrefix(strings.ToLower(clean), strings.ToLower(blobPrefix)) || strings.HasPrefix(strings.ToLower(clean), strings.ToLower(schemeLessBlobPrefix)) {
		if strings.HasPrefix(strings.ToLower(clean), strings.ToLower(schemeLessBlobPrefix)) {
			blobPrefix = schemeLessBlobPrefix
		}
		rest := clean[len(blobPrefix):]
		if slash := strings.Index(rest, "/"); slash >= 0 {
			return rest[slash+1:]
		}
		return ""
	}
	prefixes := []string{"github.com/" + owner + "/" + repo + "/", owner + "/" + repo + "/", repo + "/"}
	for _, prefix := range prefixes {
		if strings.HasPrefix(strings.ToLower(clean), strings.ToLower(prefix)) {
			return clean[len(prefix):]
		}
	}
	if i := strings.Index(clean, "@v"); i > 0 {
		if slash := strings.Index(clean[i:], "/"); slash >= 0 {
			root := clean[:i]
			rootLower := strings.ToLower(root)
			if rootLower == strings.ToLower(repo) || rootLower == strings.ToLower(owner+"/"+repo) || rootLower == strings.ToLower("github.com/"+owner+"/"+repo) || rootLower == strings.ToLower("sigs.k8s.io/"+repo) {
				clean = clean[i+slash+1:]
			} else {
				return ""
			}
		}
	}
	return strings.TrimPrefix(clean, "./")
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
	return s.verifyGitHubFileAtRef(ctx, client, owner, repo, "HEAD", inRepoPath)
}

func (s *Service) verifyGitHubFileAtRef(ctx context.Context, client *http.Client, owner, repo, ref, inRepoPath string) bool {
	inRepoPath = citationStripRE.ReplaceAllString(strings.TrimSpace(inRepoPath), "")
	if s.githubReadToken != "" {
		segments := strings.Split(inRepoPath, "/")
		for i := range segments {
			segments[i] = url.PathEscape(segments[i])
		}
		u := githubAPIBase + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/contents/" + strings.Join(segments, "/") + "?ref=" + url.QueryEscape(ref)
		if v, ok := s.linkVerifyCache.Load(u); ok {
			return v.(bool)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return false
		}
		req.Header.Set("Authorization", "Bearer "+s.githubReadToken)
		req.Header.Set("Accept", "application/vnd.github.raw+json")
		resp, err := client.Do(req)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		exists := resp.StatusCode >= 200 && resp.StatusCode < 300
		s.linkVerifyCache.Store(u, exists)
		return exists
	}
	rawURL := rawContentBase + "/" + owner + "/" + repo + "/" + ref + "/" + inRepoPath
	if v, ok := s.linkVerifyCache.Load(rawURL); ok {
		return v.(bool)
	}
	exists := headOK(ctx, client, rawURL)
	s.linkVerifyCache.Store(rawURL, exists)
	return exists
}

func blobURL(owner, repo, inRepoPath string) string {
	return blobURLAtRef(owner, repo, "HEAD", inRepoPath)
}

func blobURLAtRef(owner, repo, ref, inRepoPath string) string {
	return "https://github.com/" + owner + "/" + repo + "/blob/" + ref + "/" + inRepoPath
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
