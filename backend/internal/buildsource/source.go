// Package buildsource resolves one configured repository to the immutable
// revision tested by a build.
package buildsource

import (
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

var (
	fullCommitRE = regexp.MustCompile(`^[0-9a-fA-F]{40}([0-9a-fA-F]{24})?$`)
	hexRefRE     = regexp.MustCompile(`^[0-9a-fA-F]+$`)
)

// Source identifies one configured repository at the immutable commit tested by a build.
type Source struct {
	Owner    string
	Name     string
	Revision string
}

// Resolve fails closed unless repository-specific build metadata identifies one exact revision.
func Resolve(build models.BuildInfo, owner, name string) (Source, bool) {
	owner, name = strings.TrimSpace(owner), strings.TrimSpace(name)
	if owner == "" || name == "" {
		return Source{}, false
	}
	wanted := strings.ToLower(owner + "/" + name)
	var revision string
	mutableMatches := 0
	matched := false
	for repo, value := range build.RepoRefs {
		if strings.ToLower(strings.TrimSpace(repo)) != wanted {
			continue
		}
		matched = true
		if candidate, ok := NormalizeRevision(value); ok {
			if revision != "" && revision != candidate {
				return Source{}, false
			}
			revision = candidate
			continue
		}
		if !mutableRef(value) {
			return Source{}, false
		}
		mutableMatches++
	}
	if !matched {
		return Source{}, false
	}
	if revision != "" {
		if mutableMatches != 0 {
			return Source{}, false
		}
		return Source{Owner: owner, Name: name, Revision: revision}, true
	}
	if mutableMatches != 1 || len(build.RepoRefs) != 1 {
		return Source{}, false
	}
	commit, commitOK := bareRevision(build.Commit)
	repoVersion, repoVersionOK := bareRevision(build.RepoVersion)
	if !commitOK || !repoVersionOK || commit != repoVersion {
		return Source{}, false
	}
	return Source{Owner: owner, Name: name, Revision: commit}, true
}

// NormalizeRevision accepts a bare full SHA or one ref-qualified full SHA.
func NormalizeRevision(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if revision, ok := bareRevision(value); ok {
		return revision, true
	}
	if strings.Count(value, ":") != 1 || strings.Contains(value, ",") {
		return "", false
	}
	ref, revision, _ := strings.Cut(value, ":")
	if strings.TrimSpace(ref) == "" {
		return "", false
	}
	return bareRevision(revision)
}

func bareRevision(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if !fullCommitRE.MatchString(value) {
		return "", false
	}
	return strings.ToLower(value), true
}

func mutableRef(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && !strings.EqualFold(value, "ambiguous") && !hexRefRE.MatchString(value) && !strings.ContainsAny(value, ",:")
}

// VerifiedPaths returns safe repository-local GitHub blob paths for a source.
// An empty source revision accepts any immutable or mutable link revision for
// callers that only need a repository allowlist.
func VerifiedPaths(fileLinks map[string]string, source Source) []string {
	if len(fileLinks) == 0 || strings.TrimSpace(source.Owner) == "" || strings.TrimSpace(source.Name) == "" {
		return nil
	}
	links := make([]string, 0, len(fileLinks))
	for _, raw := range fileLinks {
		links = append(links, raw)
	}
	slices.Sort(links)
	var files []string
	for _, raw := range links {
		parsed, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Hostname(), "github.com") {
			continue
		}
		parts := strings.Split(strings.Trim(parsed.EscapedPath(), "/"), "/")
		if len(parts) < 5 || !strings.EqualFold(parts[0], source.Owner) || !strings.EqualFold(parts[1], source.Name) || parts[2] != "blob" {
			continue
		}
		linkRevision, err := url.PathUnescape(parts[3])
		if err != nil || source.Revision != "" && !strings.EqualFold(linkRevision, source.Revision) {
			continue
		}
		decoded, err := url.PathUnescape(strings.Join(parts[4:], "/"))
		if err != nil {
			continue
		}
		clean := path.Clean(decoded)
		if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") || strings.Contains(clean, `\`) {
			continue
		}
		files = append(files, clean)
	}
	slices.Sort(files)
	return slices.Compact(files)
}
