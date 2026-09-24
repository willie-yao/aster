package prowbuild

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/willie-yao/aster/backend/internal/buildsource"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/storage"
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

type cloneRecord struct {
	Refs struct {
		Org     string            `json:"org"`
		Repo    string            `json:"repo"`
		BaseRef string            `json:"base_ref"`
		Pulls   []json.RawMessage `json:"pulls"`
	} `json:"refs"`
	FinalSHA string `json:"final_sha"`
	Failed   bool   `json:"failed"`
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
	PinBuildRepoRefs(ctx, b, loc, info)

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

// PinBuildRepoRefs fills bare branch refs with per-repository tested commits
// when the optional clone records identify them unambiguously.
func PinBuildRepoRefs(ctx context.Context, b storage.Backend, loc BuildLocation, info *models.BuildInfo) bool {
	if info == nil || loc.JobType != models.JobTypePeriodic || loc.PullNumber != "" {
		return false
	}
	needsPin := false
	for _, ref := range info.RepoRefs {
		if bareBranchRef(ref) {
			needsPin = true
			break
		}
	}
	if !needsPin {
		return false
	}

	data, err := storage.ReadAll(ctx, b, loc.BuildPath()+"clone-records.json")
	if err != nil {
		if !errors.Is(err, storage.ErrNotFound) {
			log.Printf("    ⚠ %s/%s: reading clone-records.json: %v", loc.JobName, loc.BuildID, err)
		}
		return false
	}
	var records []cloneRecord
	if err := json.Unmarshal(data, &records); err != nil {
		log.Printf("    ⚠ %s/%s: parsing clone-records.json: %v", loc.JobName, loc.BuildID, err)
		return false
	}
	revisions := make(map[string]string)
	conflicts := make(map[string]bool)
	for _, record := range records {
		repo := record.Refs.Org + "/" + record.Refs.Repo
		if record.Refs.Org == "" || record.Refs.Repo == "" {
			continue
		}
		ref, ok := info.RepoRefs[repo]
		if !ok || !bareBranchRef(ref) {
			continue
		}
		if len(record.Refs.Pulls) != 0 {
			conflicts[repo] = true
			continue
		}
		if record.Failed || strings.ContainsAny(record.FinalSHA, ",:") {
			continue
		}
		sha, ok := buildsource.NormalizeRevision(record.FinalSHA)
		if !ok {
			continue
		}
		if ref != record.Refs.BaseRef {
			conflicts[repo] = true
			continue
		}
		if previous, seen := revisions[repo]; seen && previous != sha {
			conflicts[repo] = true
		}
		revisions[repo] = sha
	}
	pinned := false
	for repo, sha := range revisions {
		if !conflicts[repo] {
			info.RepoRefs[repo] += ":" + sha
			pinned = true
		}
	}
	return pinned
}

func bareBranchRef(ref string) bool {
	return ref != "" && !strings.EqualFold(ref, "ambiguous") &&
		!strings.ContainsAny(ref, ",: \t\r\n") &&
		strings.Trim(ref, "0123456789abcdefABCDEF") != ""
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
	found := make(map[string]struct{})

	listing, rootErr := b.List(ctx, artifactsDir)
	if rootErr == nil {
		for _, object := range listing.Files {
			if junitFileRe.MatchString(path.Base(object.Name)) {
				found[object.Name] = struct{}{}
			}
		}
	}

	objects, truncated, treeErr := b.ListTree(ctx, artifactsDir, 2000)
	if treeErr == nil {
		for _, object := range objects {
			if junitFileRe.MatchString(path.Base(object)) {
				found[object] = struct{}{}
			}
		}
	}
	if rootErr != nil && treeErr != nil {
		return nil, false, false, errors.Join(
			fmt.Errorf("listing root artifacts %s: %w", artifactsDir, rootErr),
			fmt.Errorf("listing artifact tree %s: %w", artifactsDir, treeErr),
		)
	}

	paths := make([]string, 0, len(found))
	for object := range found {
		paths = append(paths, artifactsDir+object)
	}
	sort.Strings(paths)
	complete := treeErr == nil && !truncated
	permanentlyTruncated := treeErr == nil && truncated
	return paths, complete, permanentlyTruncated, nil
}

// PullHeadRevision returns the pull request head commit a presubmit build
// checked out. It delegates to buildsource, which owns repository-ref parsing.
func PullHeadRevision(refs map[string]string, repo, pullNumber string) (string, bool) {
	return buildsource.PullHeadRevision(refs, repo, pullNumber)
}
