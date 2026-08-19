package ai

import (
	"regexp"
	"strings"

	"github.com/willie-yao/aster/backend/internal/artifacts"
	"github.com/willie-yao/aster/backend/internal/models"
)

// Cause ownership. A build pins an immutable revision only for the project's
// own source repository, so a path in a dependency can never be verified the
// way relevant_files and file_links are. Without a structured owner the model's
// correct upstream finding survives only as freeform prose, which the
// publication sanitizer then strips because the path is ungrounded. Recording
// the owning repository separately keeps that finding while leaving every
// verified-source and Fix gate exactly as strict as before.

// repoSlugRE matches a bare "owner/repo" GitHub reference.
var repoSlugRE = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)

// maxCauseLocationFiles bounds the published path hints for one cause.
const maxCauseLocationFiles = 10

// normalizeCauseRepository reduces a model-supplied repository reference to a
// bare "owner/repo". It accepts a GitHub URL, a scheme-less github.com path, or
// the slug itself, and returns "" when the value is not a usable reference.
// Only github.com is accepted: a repository elsewhere would be rendered as a
// GitHub link and attributed to the wrong owner.
func normalizeCauseRepository(raw string) string {
	value := strings.TrimSpace(raw)
	value = strings.Trim(value, "`'\"<>(),;:")
	hadScheme := false
	for _, scheme := range []string{"https://", "http://"} {
		if trimmed, found := strings.CutPrefix(value, scheme); found {
			value, hadScheme = trimmed, true
			break
		}
	}
	value = strings.TrimPrefix(value, "www.")
	host, rest, hasHost := strings.Cut(value, "/")
	if strings.Contains(host, ".") {
		// A host-shaped leading segment must be GitHub, otherwise the reference
		// names a repository this product cannot address.
		if !hasHost || !strings.EqualFold(host, "github.com") {
			return ""
		}
		value = rest
	} else if hadScheme {
		// "https://owner/repo" is not a repository reference.
		return ""
	}
	value = strings.TrimSuffix(value, ".git")
	value = strings.Trim(value, "/")
	// A deeper reference such as "owner/repo/tree/main" carries the repo in its
	// first two segments; anything shorter is not a repository.
	segments := strings.Split(value, "/")
	if len(segments) < 2 {
		return ""
	}
	value = segments[0] + "/" + segments[1]
	if !repoSlugRE.MatchString(value) {
		return ""
	}
	return value
}

// normalizeCauseLocation validates one model-supplied cause location against
// the configured project repository. It returns nil when ownership cannot be
// established, because an unattributed cause is the existing behavior while a
// wrong attribution would misdirect the reader.
//
// verified are the analysis's published relevant files, already proven by a
// read at the pinned revision. A project-owned location carries only those, so
// this field never becomes a second, weaker channel for project paths. An
// external location carries the model's hints as-is; they are explicitly
// unverified and are never promoted into relevant_files or file_links.
func normalizeCauseLocation(location *models.AnalysisCauseLocation, sourceOwner, sourceName string, verified []string) *models.AnalysisCauseLocation {
	if location == nil {
		return nil
	}
	repository := normalizeCauseRepository(location.Repository)
	if repository == "" {
		return nil
	}
	// Ownership is meaningful only relative to a known project repository.
	// Without one, "external" cannot be distinguished from "our own code".
	if strings.TrimSpace(sourceOwner) == "" || strings.TrimSpace(sourceName) == "" {
		return nil
	}
	configured := sourceOwner + "/" + sourceName
	external := !strings.EqualFold(repository, configured)
	if !external {
		// Canonicalize to the configured spelling so a case variation cannot
		// produce two identities for the same repository.
		repository = configured
	}
	out := &models.AnalysisCauseLocation{Repository: repository, External: external}
	out.Files = causeLocationFiles(location.Files, external, verified)
	return out
}

func causeLocationFiles(files []string, external bool, verified []string) []string {
	known := map[string]bool{}
	for _, file := range verified {
		known[strings.ToLower(file)] = true
	}
	seen := map[string]bool{}
	out := make([]string, 0, min(len(files), maxCauseLocationFiles))
	for _, file := range files {
		clean, err := artifacts.SafePath(strings.TrimSpace(trailingParenRe.ReplaceAllString(file, "")))
		if err != nil || clean == "" || !isSourceCitation(clean) || seen[clean] {
			continue
		}
		// A project-owned path is authoritative only when the analysis already
		// proved it at the pinned revision, so reuse that exact set instead of
		// re-deciding grounding here. Conversely a path proven to be a project
		// file cannot also be a dependency file: keep the proven reading and
		// drop the contradictory hint, so one path never means both.
		if known[strings.ToLower(clean)] != !external {
			continue
		}
		seen[clean] = true
		out = append(out, clean)
		if len(out) == maxCauseLocationFiles {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// externalRemediationFallback replaces the generic project-automation sentence
// when a cause is owned by a dependency. Naming the repository that must change
// is materially more useful than telling a maintainer to run project tooling
// that cannot reach the defect.
//
// It deliberately carries no file path. This text is assigned after the
// ungrounded-path sanitizer has run, so any path placed here would skip that
// gate and then be offered to the file-link resolver, which verifies relative
// paths against the project's repository. A dependency path that happened to
// collide with a real project path would become a verified project link and an
// actionable source file. The paths stay in the structured cause location,
// which is published as the unverified hints they are.
func externalRemediationFallback(location *models.AnalysisCauseLocation) string {
	return "This failure is caused by code in " + location.Repository +
		", not by this project, so the upstream change must land there." +
		" Track that change, then rerun the failing job once this project consumes a version that includes it."
}

// causeLocationsAgree reports whether two locations name the same owner with
// the same ownership classification.
func causeLocationsAgree(left, right *models.AnalysisCauseLocation) bool {
	if left == nil || right == nil {
		return false
	}
	return left.External == right.External && strings.EqualFold(left.Repository, right.Repository)
}

// MergeCauseLocations returns the shared location for a set of analyses, or nil
// when any member lacks one or the members disagree. Ownership is only reported
// for a group when every build it covers reached the same conclusion.
func MergeCauseLocations(locations []*models.AnalysisCauseLocation) *models.AnalysisCauseLocation {
	if len(locations) == 0 {
		return nil
	}
	merged := locations[0]
	if merged == nil {
		return nil
	}
	for _, location := range locations[1:] {
		if !causeLocationsAgree(merged, location) {
			return nil
		}
	}
	// File hints can differ across builds even when ownership agrees. Union them
	// in first-seen order so the group keeps every reported location.
	out := &models.AnalysisCauseLocation{Repository: merged.Repository, External: merged.External}
	seen := map[string]bool{}
	for _, location := range locations {
		for _, file := range location.Files {
			if seen[file] || len(out.Files) == maxCauseLocationFiles {
				continue
			}
			seen[file] = true
			out.Files = append(out.Files, file)
		}
	}
	return out
}
