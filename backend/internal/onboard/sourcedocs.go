package onboard

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	maxPromptSources          = 8
	maxPromptSourceBytes      = 12_000
	maxPromptSourceTotalBytes = 48_000
	maxPromptSourceFetchBytes = 1 << 20
	maxPromptSourceAttempts   = 30
)

var githubRawBaseURL = "https://raw.githubusercontent.com"

var promptVersionPathComponent = regexp.MustCompile(`(?i)^(?:v?(?:[0-9]+\.)+[0-9]+|v[0-9]+(?:alpha|beta)[0-9]+)$`)

type promptSource struct {
	Path      string `json:"path"`
	Kind      string `json:"kind"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Text      string `json:"text"`
}

type promptJobSummary struct {
	Name       string
	Type       string
	ConfigFile string
	Repo       string
	Branches   []string
	Dashboards []string
}

type promptDraftInput struct {
	ProjectName string
	SourceRepo  Repo
	Jobs        []promptJobSummary
	Sources     []promptSource
}

type promptSourceCandidate struct {
	Path string
	Kind string
	Size int
}

var promptSourceKeywords = map[string]int{
	"artifact": 70, "collect": 60, "logger": 55, "debug": 55,
	"troubleshoot": 65, "e2e": 55, "test": 35, "template": 45,
	"flavor": 50, "bootstrap": 55, "cloud-init": 60, "controller": 40,
	"machine": 40, "cluster": 35, "prow": 50,
}

type promptSourceFetchResult struct {
	Sources          []promptSource
	RevisionDuration time.Duration
	TreeDuration     time.Duration
	ExcerptDuration  time.Duration
	Attempts         int
}

func fetchPromptSources(ctx context.Context, client *http.Client, repo Repo, jobs []promptJobSummary, token string, credentials ...string) ([]promptSource, error) {
	result, err := fetchPromptSourcesDetailed(ctx, client, repo, jobs, token, credentials...)
	return result.Sources, err
}

func fetchPromptSourcesDetailed(ctx context.Context, client *http.Client, repo Repo, jobs []promptJobSummary, token string, credentials ...string) (promptSourceFetchResult, error) {
	var result promptSourceFetchResult
	revisionStart := time.Now()
	branch, err := defaultBranch(ctx, client, repo.Owner, repo.Name, token)
	if err != nil {
		result.RevisionDuration = time.Since(revisionStart)
		return result, sourcePromptFailure(promptStageSourceRevision, err)
	}
	revision, err := resolvePromptSourceRevision(ctx, client, repo.Owner, repo.Name, branch, token)
	result.RevisionDuration = time.Since(revisionStart)
	if err != nil {
		return result, sourcePromptFailure(promptStageSourceRevision, err)
	}

	treeStart := time.Now()
	candidates, err := listPromptSourceCandidates(ctx, client, repo.Owner, repo.Name, revision, token)
	result.TreeDuration = time.Since(treeStart)
	if err != nil {
		return result, sourcePromptFailure(promptStageSourceTree, err)
	}
	candidatePaths := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		candidatePaths[candidate.Path] = struct{}{}
	}
	references := make(map[string]struct{})
	for _, job := range jobs {
		if _, ok := candidatePaths[job.ConfigFile]; ok {
			references[job.ConfigFile] = struct{}{}
		}
	}

	excerptStart := time.Now()
	selectedKinds := make(map[string]int)
	selectedFamilies := make(map[string]struct{})
	selectedContent := make(map[[32]byte]struct{})
	attempted := make(map[string]bool)
	sources := make([]promptSource, 0, maxPromptSources)
	total := 0
	fetchFailures := 0
	var lastFetchErr error
	for result.Attempts < maxPromptSourceAttempts && len(sources) < maxPromptSources && total < maxPromptSourceTotalBytes {
		sortPromptSourceCandidates(candidates, references, selectedKinds, len(sources))
		var candidate promptSourceCandidate
		found := false
		for _, item := range candidates {
			if attempted[item.Path] {
				continue
			}
			if _, duplicateFamily := selectedFamilies[promptSourceFamily(item.Path)]; duplicateFamily {
				attempted[item.Path] = true
				continue
			}
			candidate, found = item, true
			break
		}
		if !found {
			break
		}
		attempted[candidate.Path] = true
		result.Attempts++
		text, err := fetchRawSource(ctx, client, repo.Owner, repo.Name, revision, candidate.Path, token)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				result.ExcerptDuration = time.Since(excerptStart)
				return result, sourcePromptFailure(promptStageSourceExcerpt, err)
			}
			fetchFailures++
			lastFetchErr = err
			continue
		}
		if strings.TrimSpace(text) == "" || !isPromptSourceText(text) {
			continue
		}
		text = redactPromptText(text, credentials...)
		remaining := maxPromptSourceTotalBytes - total
		limit := maxPromptSourceBytes
		if remaining < limit {
			limit = remaining
		}
		source := selectPromptSourceExcerpt(candidate, text, limit)
		if strings.TrimSpace(source.Text) == "" {
			continue
		}
		contentKey := sha256.Sum256([]byte(source.Text))
		if _, duplicateContent := selectedContent[contentKey]; duplicateContent {
			continue
		}
		selectedContent[contentKey] = struct{}{}
		selectedFamilies[promptSourceFamily(candidate.Path)] = struct{}{}
		sources = append(sources, source)
		selectedKinds[candidate.Kind]++
		total += len(source.Text)
		if candidate.Kind == "markdown" {
			for _, referenced := range referencedPromptPaths(source.Text, candidatePaths) {
				references[referenced] = struct{}{}
			}
		}
	}
	result.ExcerptDuration = time.Since(excerptStart)
	sort.Slice(sources, func(i, j int) bool { return sources[i].Path < sources[j].Path })
	result.Sources = sources
	if len(sources) == 0 && result.Attempts > 0 && fetchFailures == result.Attempts {
		return result, sourcePromptFailure(promptStageSourceExcerpt, lastFetchErr)
	}
	return result, nil
}

func sourcePromptFailure(stage promptPreparationStage, err error) *promptPreparationFailure {
	category := promptFailureSourceUnavailable
	if isPromptDeadline(err) {
		category = promptFailureTimedOut
	}
	return &promptPreparationFailure{Stage: stage, Category: category, cause: err}
}

func listPromptSourceCandidates(ctx context.Context, client *http.Client, owner, repo, branch, token string) ([]promptSourceCandidate, error) {
	var out struct {
		Truncated bool `json:"truncated"`
		Tree      []struct {
			Path string `json:"path"`
			Type string `json:"type"`
			Size int    `json:"size"`
		} `json:"tree"`
	}
	endpoint := fmt.Sprintf("%s/repos/%s/%s/git/trees/%s?recursive=1", githubAPIBaseURL, owner, repo, branch)
	if err := ghJSON(ctx, client, endpoint, token, &out); err != nil {
		return nil, err
	}
	if out.Truncated {
		return nil, fmt.Errorf("source repository tree response is truncated")
	}
	candidates := make([]promptSourceCandidate, 0, len(out.Tree))
	for _, entry := range out.Tree {
		kind, ok := promptSourceKind(entry.Path)
		if entry.Type != "blob" || !ok || excludedPromptSourcePath(entry.Path) || entry.Size > 1<<20 {
			continue
		}
		candidates = append(candidates, promptSourceCandidate{Path: entry.Path, Kind: kind, Size: entry.Size})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Path < candidates[j].Path })
	return candidates, nil
}

func promptSourceKind(filename string) (string, bool) {
	switch strings.ToLower(path.Ext(filename)) {
	case ".md":
		return "markdown", true
	case ".go":
		return "go", true
	case ".yaml", ".yml":
		return "yaml", true
	case ".sh":
		return "shell", true
	default:
		return "", false
	}
}

func excludedPromptSourcePath(filename string) bool {
	if filename == "" || strings.HasPrefix(filename, "/") || containsControl(filename) {
		return true
	}
	lower := strings.ToLower(filename)
	for _, component := range strings.Split(lower, "/") {
		switch component {
		case "vendor", "third_party", "thirdparty", ".github", "node_modules", "dist", "build", "_output", "generated":
			return true
		}
	}
	base := path.Base(lower)
	return strings.HasPrefix(base, "zz_generated.") || strings.HasSuffix(base, ".pb.go") || strings.HasSuffix(base, "_generated.go")
}

func sortPromptSourceCandidates(candidates []promptSourceCandidate, references map[string]struct{}, selectedKinds map[string]int, selected int) {
	sort.SliceStable(candidates, func(i, j int) bool {
		si := promptSourceScore(candidates[i], references, selectedKinds, selected)
		sj := promptSourceScore(candidates[j], references, selectedKinds, selected)
		if si != sj {
			return si > sj
		}
		return candidates[i].Path < candidates[j].Path
	})
}

func promptSourceScore(candidate promptSourceCandidate, references map[string]struct{}, selectedKinds map[string]int, selected int) int {
	lower := strings.ToLower(candidate.Path)
	base := strings.ToLower(path.Base(candidate.Path))
	score := 0
	if _, ok := references[candidate.Path]; ok {
		score += 1000
	}
	switch {
	case lower == "readme.md":
		score += 140
	case base == "readme.md":
		score += 45
	case strings.HasPrefix(lower, "docs/"):
		score += 70
	}
	for keyword, weight := range promptSourceKeywords {
		if strings.Contains(lower, keyword) {
			score += weight
		}
	}
	if strings.HasPrefix(lower, "test/e2e/data/") {
		score -= 120
	}
	if strings.HasPrefix(lower, "templates/test/ci/") {
		score -= 90
	}
	if candidate.Size > maxPromptSourceBytes {
		score -= min(candidate.Size/maxPromptSourceBytes, 4) * 10
	}
	switch candidate.Kind {
	case "yaml":
		score += 30
	case "go", "shell":
		score += 25
	case "markdown":
		score += 20
	}
	if selectedKinds[candidate.Kind] == 0 {
		score += 35
	}
	if selected >= 2 && selectedKinds["go"]+selectedKinds["yaml"]+selectedKinds["shell"] == 0 && candidate.Kind != "markdown" {
		score += 100
	}
	score -= strings.Count(candidate.Path, "/") * 3
	return score
}

func promptSourceFamily(filename string) string {
	parts := strings.Split(filename, "/")
	for i, part := range parts {
		if promptVersionPathComponent.MatchString(part) {
			parts[i] = "<version>"
		}
	}
	return strings.Join(parts, "/")
}

func selectPromptSourceExcerpt(candidate promptSourceCandidate, text string, limit int) promptSource {
	text = sanitizePromptSourceText(text)
	lines := strings.Split(text, "\n")
	if len(text) <= limit {
		return promptSource{Path: candidate.Path, Kind: candidate.Kind, StartLine: 1, EndLine: len(lines), Text: text}
	}
	anchor, best := 0, 0
	for i, line := range lines {
		score := 0
		lower := strings.ToLower(line)
		for keyword, weight := range promptSourceKeywords {
			if strings.Contains(lower, keyword) {
				score += weight
			}
		}
		if score > best {
			anchor, best = i, score
		}
	}
	start, end, excerpt := promptExcerptWindow(lines, anchor, best > 0, limit)
	return promptSource{Path: candidate.Path, Kind: candidate.Kind, StartLine: start + 1, EndLine: end + 1, Text: excerpt}
}

func promptExcerptWindow(lines []string, anchor int, useAnchor bool, limit int) (int, int, string) {
	if !useAnchor {
		anchor = 0
	}
	if len(lines[anchor]) >= limit {
		return anchor, anchor, truncateUTF8Bytes(lines[anchor], limit)
	}
	start, end := anchor, anchor
	total := len(lines[anchor])
	for {
		expanded := false
		if start > 0 && total+1+len(lines[start-1]) <= limit {
			start--
			total += 1 + len(lines[start])
			expanded = true
		}
		if end+1 < len(lines) && total+1+len(lines[end+1]) <= limit {
			end++
			total += 1 + len(lines[end])
			expanded = true
		}
		if !expanded {
			break
		}
	}
	return start, end, strings.Join(lines[start:end+1], "\n")
}

func referencedPromptPaths(text string, candidates map[string]struct{}) []string {
	found := make(map[string]struct{})
	fields := strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune("._/-", r)
	})
	for _, field := range fields {
		field = strings.Trim(field, "/")
		if _, ok := candidates[field]; ok {
			found[field] = struct{}{}
		}
	}
	out := make([]string, 0, len(found))
	for item := range found {
		out = append(out, item)
	}
	sort.Strings(out)
	return out
}

func isPromptSourceText(text string) bool {
	return utf8.ValidString(text) && !strings.ContainsRune(text, '\x00')
}

func sanitizePromptSourceText(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	text = strings.ToValidUTF8(text, "�")
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || !unicode.IsControl(r) {
			return r
		}
		return -1
	}, text)
}

func truncateUTF8Bytes(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	text = text[:limit]
	for !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}
	return text
}

func containsControl(text string) bool {
	return strings.IndexFunc(text, unicode.IsControl) >= 0
}

func redactPromptCredentials(sources []promptSource, credentials ...string) {
	for i := range sources {
		sources[i].Text = redactPromptText(sources[i].Text, credentials...)
	}
}

func redactPromptText(text string, credentials ...string) string {
	credentials = sortedUniqueStrings(credentials)
	sort.Slice(credentials, func(i, j int) bool {
		if len(credentials[i]) != len(credentials[j]) {
			return len(credentials[i]) > len(credentials[j])
		}
		return credentials[i] < credentials[j]
	})
	for _, credential := range credentials {
		text = strings.ReplaceAll(text, credential, strings.Repeat("*", len(credential)))
	}
	return text
}

func defaultBranch(ctx context.Context, client *http.Client, owner, repo, token string) (string, error) {
	var out struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := ghJSON(ctx, client, fmt.Sprintf("%s/repos/%s/%s", githubAPIBaseURL, owner, repo), token, &out); err != nil {
		return "", err
	}
	if out.DefaultBranch == "" {
		return "main", nil
	}
	return out.DefaultBranch, nil
}

func resolvePromptSourceRevision(ctx context.Context, client *http.Client, owner, repo, branch, token string) (string, error) {
	endpoint := fmt.Sprintf("%s/repos/%s/%s/commits/%s", githubAPIBaseURL, owner, repo, url.PathEscape(branch))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github.sha")
	req.Header.Set("User-Agent", "prow-ai-dashboard")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("resolving source revision: %s", resp.Status)
	}
	revision := strings.TrimSpace(string(body))
	if len(revision) < 7 || strings.IndexFunc(revision, func(r rune) bool {
		return !strings.ContainsRune("0123456789abcdefABCDEF", r)
	}) >= 0 {
		return "", fmt.Errorf("source revision response was invalid")
	}
	return revision, nil
}

func fetchRawSource(ctx context.Context, client *http.Client, owner, repo, branch, filename, token string) (string, error) {
	parts := strings.Split(filename, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	endpoint := fmt.Sprintf("%s/%s/%s/%s/%s", githubRawBaseURL, owner, repo, branch, strings.Join(parts, "/"))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("raw source %s: %s", filename, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPromptSourceFetchBytes+1))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func ghJSON(ctx context.Context, client *http.Client, endpoint, token string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "prow-ai-dashboard")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", endpoint, resp.Status)
	}
	return json.Unmarshal(body, out)
}
