package ai

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/willie-yao/aster/backend/internal/artifacts"
	"github.com/willie-yao/aster/backend/internal/buildsource"
	"github.com/willie-yao/aster/backend/internal/models"
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
//
// The resolved map is part of the published analysis content hash, so it must
// be a function of the analysis and its pinned revision alone. Three rules keep
// it that way: an inconclusive check never counts as absence, a definite answer
// at an immutable revision is resolved once and then reused by later publication
// passes instead of re-queried, and an inconclusive check keeps whatever link a
// previous pass already published for that path at that revision.

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

// LinkVerificationKeyPrefix namespaces persisted link verifications in the
// private AI cache.
const LinkVerificationKeyPrefix = "filelink:v1:"

// linkAbsenceTTL bounds how long a recorded absence is trusted. A verified
// presence at an immutable revision is permanent, but GitHub has been observed
// serving spurious 404s, so absence is re-checked eventually rather than hiding
// real evidence for the whole cache lifetime.
const linkAbsenceTTL = 24 * time.Hour

// linkVerification is the outcome of one file-existence check. Only 2xx and 404
// are definite. A transport error, rate limit, or 5xx is unverified and must not
// be recorded as "absent": FileLinks feeds the published analysis content hash,
// so treating a transient GitHub failure as absence changes the identity of an
// unchanged analysis and invalidates in-flight chat and fix sessions.
type linkVerification uint8

const (
	linkUnverified linkVerification = iota
	linkPresent
	linkAbsent
)

// linkVerificationRecord is the persisted shape of one definite check.
type linkVerificationRecord struct {
	Present bool `json:"present"`
}

// LinkVerificationStore persists file-existence answers at immutable revisions
// so a published link map does not depend on live GitHub availability.
type LinkVerificationStore interface {
	LoadLinkVerification(key string) (present bool, ok bool)
	StoreLinkVerification(key string, present bool)
}

// LinkVerificationCacheKey names one persisted check at an immutable revision.
func LinkVerificationCacheKey(owner, repo, revision, inRepoPath string) string {
	return LinkVerificationKeyPrefix + owner + "/" + repo + "@" + revision + ":" + inRepoPath
}

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

// SetVerificationStore installs the durable store for immutable-revision
// checks. Without it every publication pass re-verifies against GitHub.
func (r *FileLinkResolver) SetVerificationStore(store LinkVerificationStore) {
	if r == nil {
		return
	}
	r.service.SetLinkVerificationStore(store)
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
	// The incoming map is what a previous pass published for this analysis. It
	// is the fallback when GitHub cannot answer for a path at an immutable ref.
	published := tc.AIAnalysis.FileLinks
	// Strings the analysis attributed to a dependency must never be resolved
	// against the project's repository. Both a dependency path and a repository
	// slug such as "nats-io/nats.go" are path-shaped, so a collision with a real
	// project path would publish a verified link to an unrelated project file
	// and make it an actionable Fix source.
	foreign := foreignSourceCandidates(tc.AIAnalysis.CauseLocation)

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
		if foreign[canonicalLinkCandidate(clean)] {
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
		if ref != "" && ref != "HEAD" {
			url, verification := s.resolveConfiguredSourceLinkAtRef(ctx, client, clean, ref)
			switch {
			case verification == linkPresent:
				links[clean] = url
			case verification == linkUnverified:
				// A once-verified path stays valid at an immutable revision, so
				// keep the published link rather than letting a transient GitHub
				// failure change this analysis's content hash.
				if retained, ok := publishedLinkAtRef(published, clean, ref); ok {
					links[clean] = retained
				}
			}
			continue
		}
		if url, ok := s.resolveSourceLink(ctx, client, clean); ok {
			links[clean] = url
		}
	}
	return links
}

// canonicalLinkCandidate reduces a cited path to the form the project-relative
// resolvers ultimately use, so equality checks against it cannot be defeated by
// an equivalent spelling such as a "./" prefix.
func canonicalLinkCandidate(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.TrimPrefix(value, "./")
	return strings.TrimPrefix(value, "/")
}

// foreignSourceCandidates returns the canonical strings an analysis attributed
// to a dependency, which must never be verified as project-relative paths. A
// project-owned location contributes nothing: its files are already the
// analysis's verified project reads.
func foreignSourceCandidates(location *models.AnalysisCauseLocation) map[string]bool {
	if location == nil || !location.External {
		return nil
	}
	out := make(map[string]bool, len(location.Files)+1)
	if location.Repository != "" {
		out[canonicalLinkCandidate(location.Repository)] = true
	}
	for _, file := range location.Files {
		out[canonicalLinkCandidate(file)] = true
	}
	return out
}

// publishedLinkAtRef returns the already published link for a path when it is
// pinned to the same immutable revision.
func publishedLinkAtRef(published map[string]string, clean, ref string) (string, bool) {
	if _, ok := buildsource.NormalizeRevision(ref); !ok {
		return "", false
	}
	link, ok := published[clean]
	if !ok || !strings.Contains(link, "/blob/"+ref+"/") {
		return "", false
	}
	return link, true
}

func (s *Service) resolveConfiguredSourceLinkAtRef(ctx context.Context, client *http.Client, clean, ref string) (string, linkVerification) {
	path := normalizeConfiguredSourcePath(clean, s.sourceRepoOwner, s.sourceRepoName)
	path, err := artifacts.SafePath(path)
	if err != nil || path == "" {
		return "", linkAbsent
	}
	if verification := s.verifyGitHubFileAtRef(ctx, client, s.sourceRepoOwner, s.sourceRepoName, ref, path); verification != linkPresent {
		return "", verification
	}
	return blobURLAtRef(s.sourceRepoOwner, s.sourceRepoName, ref, path), linkPresent
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
		s.verifyGitHubFile(ctx, client, s.sourceRepoOwner, s.sourceRepoName, clean) == linkPresent {
		return blobURL(s.sourceRepoOwner, s.sourceRepoName, clean), true
	}

	// A leading host segment containing a dot denotes an explicit github.com
	// path or a Go vanity import path.
	if strings.Contains(segs[0], ".") {
		if segs[0] == "github.com" && len(segs) >= 4 {
			owner, repo, inRepo := segs[1], segs[2], strings.Join(segs[3:], "/")
			if s.verifyGitHubFile(ctx, client, owner, repo, inRepo) == linkPresent {
				return blobURL(owner, repo, inRepo), true
			}
			return "", false
		}
		if owner, repo, inRepo, ok := s.resolveVanity(ctx, client, clean); ok &&
			s.verifyGitHubFile(ctx, client, owner, repo, inRepo) == linkPresent {
			return blobURL(owner, repo, inRepo), true
		}
		return "", false
	}

	// An explicit "owner/repo/path" GitHub reference.
	if len(segs) >= 3 {
		owner, repo, inRepo := segs[0], segs[1], strings.Join(segs[2:], "/")
		if s.verifyGitHubFile(ctx, client, owner, repo, inRepo) == linkPresent {
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
// memoized per run by the probe URL.
func (s *Service) verifyGitHubFile(ctx context.Context, client *http.Client, owner, repo, inRepoPath string) linkVerification {
	return s.verifyGitHubFileAtRef(ctx, client, owner, repo, "HEAD", inRepoPath)
}

// verifyGitHubFileAtRef resolves one file-existence question. Definite answers
// are memoized for the run and, at an immutable revision, persisted so later
// publication passes reuse them instead of re-querying GitHub. An unverified
// outcome is never recorded.
func (s *Service) verifyGitHubFileAtRef(ctx context.Context, client *http.Client, owner, repo, ref, inRepoPath string) linkVerification {
	inRepoPath = citationStripRE.ReplaceAllString(strings.TrimSpace(inRepoPath), "")
	probeURL, useAPI := s.linkProbeURL(owner, repo, ref, inRepoPath)
	if v, ok := s.linkVerifyCache.Load(probeURL); ok {
		return v.(linkVerification)
	}
	persistKey := ""
	if revision, ok := buildsource.NormalizeRevision(ref); ok {
		persistKey = LinkVerificationCacheKey(owner, repo, revision, inRepoPath)
		if store := s.linkVerifications(); store != nil {
			if present, ok := store.LoadLinkVerification(persistKey); ok {
				verification := linkAbsent
				if present {
					verification = linkPresent
				}
				s.linkVerifyCache.Store(probeURL, verification)
				return verification
			}
		}
	}
	verification := s.probeGitHubFile(ctx, client, probeURL, useAPI)
	// Memoize every outcome for the run so one degraded endpoint cannot turn
	// each citing analysis into another request, but persist only definite
	// answers so an unverified probe never outlives this pass.
	s.linkVerifyCache.Store(probeURL, verification)
	if persistKey != "" && verification != linkUnverified {
		if store := s.linkVerifications(); store != nil {
			store.StoreLinkVerification(persistKey, verification == linkPresent)
		}
	}
	return verification
}

// linkProbeURL returns the existence-check URL for one file and whether it
// addresses the authenticated contents API rather than the raw content host.
func (s *Service) linkProbeURL(owner, repo, ref, inRepoPath string) (string, bool) {
	if s.githubReadToken == "" {
		return rawContentBase + "/" + owner + "/" + repo + "/" + ref + "/" + inRepoPath, false
	}
	segments := strings.Split(inRepoPath, "/")
	for i := range segments {
		segments[i] = url.PathEscape(segments[i])
	}
	return githubAPIBase + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) +
		"/contents/" + strings.Join(segments, "/") + "?ref=" + url.QueryEscape(ref), true
}

// probeGitHubFile issues one bounded existence check.
func (s *Service) probeGitHubFile(ctx context.Context, client *http.Client, probeURL string, useAPI bool) linkVerification {
	reqCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	method := http.MethodHead
	if useAPI {
		method = http.MethodGet
	}
	req, err := http.NewRequestWithContext(reqCtx, method, probeURL, nil)
	if err != nil {
		return linkUnverified
	}
	if useAPI {
		req.Header.Set("Authorization", "Bearer "+s.githubReadToken)
		req.Header.Set("Accept", "application/vnd.github.raw+json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return linkUnverified
	}
	defer resp.Body.Close()
	return classifyLinkStatus(resp.StatusCode)
}

// classifyLinkStatus maps an HTTP status to a verification outcome. Only 404
// proves absence; every other non-2xx status leaves the question open.
func classifyLinkStatus(status int) linkVerification {
	switch {
	case status >= 200 && status < 300:
		return linkPresent
	case status == http.StatusNotFound:
		return linkAbsent
	default:
		return linkUnverified
	}
}

func blobURL(owner, repo, inRepoPath string) string {
	return blobURLAtRef(owner, repo, "HEAD", inRepoPath)
}

func blobURLAtRef(owner, repo, ref, inRepoPath string) string {
	return "https://github.com/" + owner + "/" + repo + "/blob/" + ref + "/" + inRepoPath
}
