package ai

import (
	"regexp"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

var fullSourceCommitRE = regexp.MustCompile(`^[0-9a-fA-F]{40}([0-9a-fA-F]{24})?$`)

// BuildSource identifies one configured repository at the immutable commit tested by a build.
type BuildSource struct {
	Owner    string
	Name     string
	Revision string
}

// ResolveBuildSource fails closed when build metadata does not identify one exact source commit.
func ResolveBuildSource(build models.BuildInfo, owner, name string) (BuildSource, bool) {
	owner, name = strings.TrimSpace(owner), strings.TrimSpace(name)
	if owner == "" || name == "" {
		return BuildSource{}, false
	}
	wanted := strings.ToLower(owner + "/" + name)
	var revision string
	for repo, value := range build.RepoRefs {
		if strings.ToLower(strings.TrimSpace(repo)) != wanted {
			continue
		}
		candidate, ok := exactBuildRevision(value)
		if !ok || revision != "" && revision != candidate {
			return BuildSource{}, false
		}
		revision = candidate
	}
	if revision == "" && len(build.RepoRefs) == 0 {
		var ok bool
		revision, ok = exactBuildRevision(build.RepoVersion)
		if !ok {
			return BuildSource{}, false
		}
	}
	if revision == "" {
		return BuildSource{}, false
	}
	return BuildSource{Owner: owner, Name: name, Revision: revision}, true
}

func exactBuildRevision(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if fullSourceCommitRE.MatchString(value) {
		return strings.ToLower(value), true
	}
	if strings.Count(value, ":") != 1 || strings.Contains(value, ",") {
		return "", false
	}
	_, value, _ = strings.Cut(value, ":")
	value = strings.TrimSpace(value)
	if !fullSourceCommitRE.MatchString(value) {
		return "", false
	}
	return strings.ToLower(value), true
}
