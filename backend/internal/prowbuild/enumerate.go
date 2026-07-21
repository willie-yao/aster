package prowbuild

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/storage"
)

// ListRecentBuilds returns up to count builds for job, newest first.
//
// For periodics it lists logs/<job>/ and treats each numeric subdirectory as a
// build ID.
//
// For presubmits it lists pr-logs/directory/<job>/, a flat index of
// <buildID>.txt files spanning every PR that has triggered this job, where each
// .txt body is the path pr-logs/pull/<org_repo>/<pr#>/<job>/<buildID>. Build IDs
// are sorted descending and resolved by reading their .txt body.
// Resolutions are validated against the job's Repo + Name + buildID so a
// same-named presubmit in a different repo cannot bleed in; invalid candidates
// are skipped until count valid Builds are gathered or candidates run out.
func ListRecentBuilds(ctx context.Context, b storage.Backend, job *models.ProwJob, count int) ([]Build, error) {
	if job == nil {
		return nil, fmt.Errorf("prowbuild: nil ProwJob")
	}
	if count <= 0 {
		return nil, nil
	}
	loc := JobLocation{JobType: job.JobType, Repo: job.Repo}
	indexPrefix := loc.JobIndexPrefix(job.Name)

	switch job.JobType {
	case models.JobTypePeriodic:
		ids, err := listPeriodicBuildIDs(ctx, b, indexPrefix, count)
		if err != nil {
			return nil, err
		}
		out := make([]Build, len(ids))
		for i, id := range ids {
			out[i] = Build{ID: id}
		}
		return out, nil
	case models.JobTypePresubmit:
		if job.Repo == "" {
			return nil, fmt.Errorf("prowbuild: presubmit job %q requires Repo", job.Name)
		}
		ids, err := listPresubmitCandidateIDs(ctx, b, indexPrefix)
		if err != nil {
			return nil, err
		}
		expectedRepoSeg := strings.ReplaceAll(job.Repo, "/", "_")
		out := make([]Build, 0, count)
		for _, id := range ids {
			if len(out) >= count {
				break
			}
			pull, ok := resolvePresubmitPath(ctx, b, indexPrefix, id, expectedRepoSeg, job.Name)
			if !ok {
				continue
			}
			out = append(out, Build{ID: id, PullNumber: pull})
		}
		return out, nil
	default:
		return nil, fmt.Errorf("prowbuild: unknown JobType %q for %q", job.JobType, job.Name)
	}
}

// listPeriodicBuildIDs lists logs/<job>/ and returns its numeric build-ID
// subdirectories, sorted descending and capped at max.
func listPeriodicBuildIDs(ctx context.Context, b storage.Backend, prefix string, max int) ([]string, error) {
	listing, err := b.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, d := range listing.Dirs {
		seg := strings.TrimSuffix(d, "/")
		if isNumeric(seg) {
			ids = append(ids, seg)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	if max < len(ids) {
		ids = ids[:max]
	}
	return ids, nil
}

// listPresubmitCandidateIDs lists pr-logs/directory/<job>/ and returns every
// <buildID>.txt basename as a numeric build ID, sorted descending.
func listPresubmitCandidateIDs(ctx context.Context, b storage.Backend, prefix string) ([]string, error) {
	listing, err := b.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, f := range listing.Files {
		base := strings.TrimSuffix(f.Name, ".txt")
		if base != f.Name && isNumeric(base) {
			ids = append(ids, base)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	return ids, nil
}

// splitPresubmitRef extracts the pr-logs/pull segments from an index entry.
// It anchors on "pr-logs/pull/" because entries may be bucket-relative paths or
// absolute storage URLs.
func splitPresubmitRef(body string) ([]string, bool) {
	body = strings.TrimSpace(body)
	idx := strings.Index(body, "pr-logs/pull/")
	if idx < 0 {
		return nil, false
	}
	parts := strings.Split(body[idx:], "/")
	if len(parts) != 6 {
		return nil, false
	}
	return parts, true
}

// resolvePresubmitPath reads pr-logs/directory/<job>/<buildID>.txt and returns
// the pull number if the body resolves to a path matching the expected
// expected repo segment, job name, and build ID. Any mismatch returns ok=false.
func resolvePresubmitPath(ctx context.Context, b storage.Backend, indexPrefix, buildID, expectedRepoSeg, expectedJobName string) (string, bool) {
	body, err := storage.ReadAll(ctx, b, indexPrefix+buildID+".txt")
	if err != nil {
		return "", false
	}
	parts, ok := splitPresubmitRef(string(body))
	if !ok {
		return "", false
	}
	if parts[2] != expectedRepoSeg || parts[4] != expectedJobName || parts[5] != buildID {
		return "", false
	}
	if !isNumeric(parts[3]) {
		return "", false
	}
	return parts[3], true
}

// isNumeric reports whether s is non-empty and all digits.
func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// ListPullBuilds lists recent builds for one presubmit job on one pull request.
func ListPullBuilds(ctx context.Context, b storage.Backend, repo, pullNumber, jobName string, count int) ([]Build, error) {
	if repo == "" || pullNumber == "" || jobName == "" {
		return nil, fmt.Errorf("prowbuild: repo, pull number, and job name are required")
	}
	loc := JobLocation{JobType: models.JobTypePresubmit, Repo: repo}
	prefix := loc.jobPath(jobName, pullNumber)
	ids, err := listPeriodicBuildIDs(ctx, b, prefix, count)
	if err != nil {
		return nil, err
	}
	out := make([]Build, len(ids))
	for i, id := range ids {
		out[i] = Build{ID: id, PullNumber: pullNumber}
	}
	return out, nil
}
