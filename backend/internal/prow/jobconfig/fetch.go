package jobconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

// Base URLs for talking to kubernetes/test-infra on github.com. Vars rather
// than consts so tests can swap them for an httptest.Server.
var (
	rawBaseURL = "https://raw.githubusercontent.com/kubernetes/test-infra/"
	apiBaseURL = "https://api.github.com/repos/kubernetes/test-infra/"
)

// configJobsPrefix scopes discovery to the Prow job tree.
const configJobsPrefix = "config/jobs/"

// downloadWorkers caps how many raw.githubusercontent.com requests can be in
// flight concurrently. 10 keeps total time around 5 to 10s for about 600 files on a
// typical runner while staying well under any plausible CDN burst limits.
const downloadWorkers = 10

// FetchJobConfigs discovers all of a project's Prow job YAMLs from the
// kubernetes/test-infra repository and returns the parsed jobs.
func FetchJobConfigs(ctx context.Context, client *http.Client, cfg *project.Config) ([]models.ProwJob, error) {
	jobs, _, err := FetchJobConfigsAndCatalog(ctx, client, cfg, "")
	return jobs, err
}

// FetchJobConfigsAndCatalog returns dashboard jobs plus definitions that test
// targetRepo. Both results come from one pinned test-infra snapshot.
func FetchJobConfigsAndCatalog(ctx context.Context, client *http.Client, cfg *project.Config, targetRepo string) ([]models.ProwJob, *Catalog, error) {
	sha, err := resolveMasterSHA(ctx, client)
	if err != nil {
		return nil, nil, fmt.Errorf("resolving kubernetes/test-infra master SHA: %w", err)
	}

	files, err := listConfigJobsYAMLs(ctx, client, sha)
	if err != nil {
		return nil, nil, fmt.Errorf("listing config/jobs/ at %s: %w", sha[:7], err)
	}
	log.Printf("  discovered %d candidate YAMLs under %s at test-infra@%s", len(files), configJobsPrefix, sha[:7])

	allJobs, catalog, err := downloadAndParseAll(ctx, client, sha, files, cfg, targetRepo)
	if err != nil {
		return nil, nil, err
	}
	if len(allJobs) == 0 {
		return nil, nil, fmt.Errorf("no jobs labeled with dashboard %q found across %d candidate YAML(s) at test-infra@%s",
			cfg.TestGrid.Dashboard, len(files), sha[:7])
	}
	return allJobs, catalog, nil
}

// resolveMasterSHA returns the current commit SHA of kubernetes/test-infra's
// master branch. Pinning to a single SHA for both tree-listing and raw
// downloads gives every fetcher run a consistent snapshot, so a file that
// appears in the tree won't 404 mid-run because someone pushed a rename.
func resolveMasterSHA(ctx context.Context, client *http.Client) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBaseURL+"commits/master", nil)
	if err != nil {
		return "", err
	}
	// The vnd.github.sha media type returns the plain commit SHA as the
	// response body instead of the full commit JSON.
	req.Header.Set("Accept", "application/vnd.github.sha")
	addGitHubAuth(req)

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, snippet(body))
	}
	sha := strings.TrimSpace(string(body))
	if len(sha) < 7 {
		return "", fmt.Errorf("unexpected SHA response: %q", sha)
	}
	return sha, nil
}

// gitTreeResponse mirrors the parts of the Git Trees API we read.
type gitTreeResponse struct {
	Truncated bool `json:"truncated"`
	Tree      []struct {
		Path string `json:"path"`
		Type string `json:"type"`
	} `json:"tree"`
}

// listConfigJobsYAMLs returns sorted repo-relative YAML paths under
// configJobsPrefix. Truncated responses are a hard error so job discovery never
// silently misses part of the tree.
func listConfigJobsYAMLs(ctx context.Context, client *http.Client, sha string) ([]string, error) {
	url := apiBaseURL + "git/trees/" + sha + "?recursive=1"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	addGitHubAuth(req)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, snippet(body))
	}

	var tr gitTreeResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, fmt.Errorf("parsing tree response: %w", err)
	}
	if tr.Truncated {
		return nil, fmt.Errorf("git tree response was truncated; test-infra has outgrown the recursive-tree cap")
	}

	files := make([]string, 0, 600)
	for _, e := range tr.Tree {
		if e.Type != "blob" {
			continue
		}
		if !strings.HasPrefix(e.Path, configJobsPrefix) {
			continue
		}
		if !strings.HasSuffix(e.Path, ".yaml") {
			continue
		}
		files = append(files, e.Path)
	}
	sort.Strings(files)
	return files, nil
}

// downloadAndParseAll fetches every candidate file in parallel from
// raw.githubusercontent.com pinned to the same SHA and runs them through
// ParseJobConfig, which keeps only jobs whose testgrid-dashboards annotation
// contains cfg.TestGrid.Dashboard. The first file-level error cancels every
// in-flight goroutine and is returned to the caller.
func downloadAndParseAll(ctx context.Context, client *http.Client, sha string, files []string, cfg *project.Config, targetRepo string) ([]models.ProwJob, *Catalog, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	perFile := make([][]models.ProwJob, len(files))
	catalogPerFile := make([][]JobDefinition, len(files))
	sem := make(chan struct{}, downloadWorkers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

	recordErr := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		if firstErr == nil {
			firstErr = err
			cancel()
		}
	}

	for i, file := range files {
		wg.Add(1)
		go func(idx int, f string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}

			body, err := downloadRaw(ctx, client, sha, f)
			if err != nil {
				if ctx.Err() == nil {
					recordErr(fmt.Errorf("fetching %s: %w", f, err))
				}
				return
			}
			jobs, err := ParseJobConfig(body, f, cfg.TestGrid.Dashboard, cfg.EffectiveCategories())
			if err != nil {
				recordErr(fmt.Errorf("parsing %s: %w", f, err))
				return
			}
			definitions, err := ParseCatalog(body, f)
			if err != nil {
				recordErr(fmt.Errorf("parsing catalog %s: %w", f, err))
				return
			}
			perFile[idx] = jobs
			catalogPerFile[idx] = definitions
		}(i, file)
	}
	wg.Wait()

	if firstErr != nil {
		return nil, nil, firstErr
	}

	var all []models.ProwJob
	catalog := &Catalog{Revision: sha, Jobs: map[string]JobDefinition{}}
	for i, jobs := range perFile {
		all = append(all, jobs...)
		for _, definition := range catalogPerFile[i] {
			matchesDashboard := definitionMatchesDashboard(definition, cfg.TestGrid.Dashboard)
			matchesTarget := targetRepo != "" && definition.TestsRepo(targetRepo)
			if !matchesDashboard && !matchesTarget {
				continue
			}
			id := definition.ID()
			key := id
			if _, exists := catalog.Jobs[key]; exists {
				key = id + "::" + definition.ConfigFile
				for n := 2; ; n++ {
					if _, exists := catalog.Jobs[key]; !exists {
						break
					}
					key = fmt.Sprintf("%s::%s::%d", id, definition.ConfigFile, n)
				}
			}
			catalog.Jobs[key] = definition
		}
	}
	return all, catalog, nil
}

// downloadRaw fetches a single file from raw.githubusercontent.com pinned to
// the given commit SHA so the snapshot stays consistent with the tree listing.
func downloadRaw(ctx context.Context, client *http.Client, sha, file string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawBaseURL+sha+"/"+file, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, snippet(body))
	}
	return body, nil
}

// addGitHubAuth attaches the GITHUB_TOKEN when present. The token bumps the
// API rate limit from 60 per hour anonymous to 5000 per hour authenticated.
// It is optional so local `make fetch-data-quick` works without setup.
func addGitHubAuth(req *http.Request) {
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

// snippet returns at most 256 bytes of body so error messages stay readable
// when an upstream returns an HTML rate-limit page.
func snippet(body []byte) string {
	const max = 256
	if len(body) <= max {
		return string(body)
	}
	return string(body[:max]) + "..."
}

// DerivePeriodicPrefix returns the longest "periodic-<x>-" prefix shared by a
// strict majority of periodic jobs. Presubmits are ignored.
func DerivePeriodicPrefix(jobs []models.ProwJob) string {
	const periodic = "periodic-"
	var names []string
	for _, j := range jobs {
		if j.JobType == models.JobTypePeriodic && strings.HasPrefix(j.Name, periodic) {
			names = append(names, j.Name)
		}
	}
	if len(names) == 0 {
		return ""
	}

	// Count every "periodic-<tok-...->" prefix ending at each hyphen boundary
	// across all periodic job names, then pick the longest one shared by a
	// strict majority. Stripping a longer prefix tightens display while the
	// majority threshold prevents over-stripping in dashboards with mixed
	// naming conventions.
	counts := make(map[string]int)
	for _, n := range names {
		rest := strings.TrimPrefix(n, periodic)
		for i, c := range rest {
			if c != '-' {
				continue
			}
			counts[periodic+rest[:i+1]]++
		}
	}

	threshold := len(names)/2 + 1
	var best string
	for prefix, count := range counts {
		if count < threshold {
			continue
		}
		if len(prefix) > len(best) {
			best = prefix
		}
	}
	return best
}

func definitionMatchesDashboard(definition JobDefinition, dashboard string) bool {
	for _, value := range strings.Split(definition.Annotations["testgrid-dashboards"], ",") {
		if strings.TrimSpace(value) == dashboard {
			return true
		}
	}
	return false
}
