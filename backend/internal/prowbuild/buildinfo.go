package prowbuild

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/storage"
)

// startedJSON mirrors the schema of a Prow build's started.json.
type startedJSON struct {
	Timestamp  int64             `json:"timestamp"`
	Repos      map[string]string `json:"repos"`
	RepoCommit string            `json:"repo-commit"`
	RepoVer    string            `json:"repo-version"`
}

// finishedJSON mirrors the schema of a Prow build's finished.json.
type finishedJSON struct {
	Timestamp int64  `json:"timestamp"`
	Passed    bool   `json:"passed"`
	Result    string `json:"result"`
	Revision  string `json:"revision"`
}

// FetchBuildInfo reads started.json and finished.json for the build at loc.
// Missing or unreadable finished.json returns partial info with Result="PENDING".
func FetchBuildInfo(ctx context.Context, b storage.Backend, loc BuildLocation) (*models.BuildInfo, error) {
	buildPath := loc.BuildPath()

	startedData, err := storage.ReadAll(ctx, b, buildPath+"started.json")
	if err != nil {
		return nil, fmt.Errorf("fetching started.json: %w", err)
	}
	var s startedJSON
	if err := json.Unmarshal(startedData, &s); err != nil {
		return nil, fmt.Errorf("parsing started.json: %w", err)
	}

	info := &models.BuildInfo{
		BuildID:     loc.BuildID,
		JobName:     loc.JobName,
		PullNumber:  loc.PullNumber,
		WebURL:      b.WebURL(buildPath),
		ProwURL:     b.ProwURL(buildPath),
		BuildLogURL: b.WebURL(buildPath + "build-log.txt"),
		Started:     time.Unix(s.Timestamp, 0).UTC(),
		Commit:      s.RepoCommit,
		RepoVersion: s.RepoVer,
		RepoRefs:    s.Repos,
	}

	// finished.json is absent while the build is still running.
	finishedData, err := storage.ReadAll(ctx, b, buildPath+"finished.json")
	if err != nil {
		info.Result = "PENDING"
		return info, nil
	}
	var f finishedJSON
	if err := json.Unmarshal(finishedData, &f); err != nil {
		return nil, fmt.Errorf("parsing finished.json: %w", err)
	}
	info.Finished = time.Unix(f.Timestamp, 0).UTC()
	info.Passed = f.Passed
	info.Result = f.Result
	info.Revision = f.Revision
	info.DurationSeconds = float64(f.Timestamp - s.Timestamp)
	return info, nil
}

// junitFileRe matches JUnit XML basenames from common Prow test frameworks.
var junitFileRe = regexp.MustCompile(`^junit[._-].*\.xml$|^junit\.xml$`)

// DiscoverJUnitPaths returns usable JUnit paths for normal ingestion.
func DiscoverJUnitPaths(ctx context.Context, b storage.Backend, loc BuildLocation) ([]string, error) {
	paths, _, err := DiscoverJUnitPathsWithCompleteness(ctx, b, loc)
	return paths, err
}

// DiscoverJUnitPathsWithCompleteness also reports whether the full tree was scanned.
func DiscoverJUnitPathsWithCompleteness(ctx context.Context, b storage.Backend, loc BuildLocation) ([]string, bool, error) {
	paths, complete, _, err := DiscoverJUnitPathsWithStatus(ctx, b, loc)
	return paths, complete, err
}

// DiscoverJUnitPathsWithStatus distinguishes capped trees from retryable listing failures.
func DiscoverJUnitPathsWithStatus(ctx context.Context, b storage.Backend, loc BuildLocation) ([]string, bool, bool, error) {
	artifactsDir := loc.BuildPath() + "artifacts/"
	objects, truncated, err := b.ListTree(ctx, artifactsDir, 2000)
	complete := err == nil && !truncated
	permanentlyTruncated := err == nil && truncated
	if err != nil {
		listing, listErr := b.List(ctx, artifactsDir)
		if listErr != nil {
			return nil, false, false, err
		}
		for _, object := range listing.Files {
			objects = append(objects, object.Name)
		}
	}
	var paths []string
	for _, object := range objects {
		if junitFileRe.MatchString(path.Base(object)) {
			paths = append(paths, artifactsDir+object)
		}
	}
	sort.Strings(paths)
	return paths, complete, permanentlyTruncated, nil
}
