package onboard

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const (
	targetPromptExtractionChunkBytes = 12_000
	maxPromptExtractionChunkBytes    = 16_000
	maxPromptChunkEvidenceItems      = 1
	maxPromptChunkMergedItems        = 3
	maxPromptChunkNestedItems        = 4
	maxPromptChunkStringLength       = 300
	maxPromptExtractionAttempts      = 2
	maxPromptUnresolvedItems         = 12
	maxPromptMetadataClaims          = 3
)

type indexedPromptSource struct {
	Index  int
	Source promptSource
}

type promptSourceChunk struct {
	Sources []indexedPromptSource
	Bytes   int
}

func chunkPromptSources(sources []promptSource) ([]promptSourceChunk, error) {
	sorted := append([]promptSource(nil), sources...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Path != sorted[j].Path {
			return sorted[i].Path < sorted[j].Path
		}
		if sorted[i].StartLine != sorted[j].StartLine {
			return sorted[i].StartLine < sorted[j].StartLine
		}
		if sorted[i].EndLine != sorted[j].EndLine {
			return sorted[i].EndLine < sorted[j].EndLine
		}
		if sorted[i].Kind != sorted[j].Kind {
			return sorted[i].Kind < sorted[j].Kind
		}
		return sorted[i].Text < sorted[j].Text
	})

	var chunks []promptSourceChunk
	current := promptSourceChunk{}
	for i, source := range sorted {
		indexed := indexedPromptSource{Index: i + 1, Source: source}
		bytes := len(renderPromptSource(indexed))
		if bytes > maxPromptExtractionChunkBytes {
			return nil, fmt.Errorf("prompt source %q serializes to %d bytes, limit %d", source.Path, bytes, maxPromptExtractionChunkBytes)
		}
		if len(current.Sources) > 0 && current.Bytes+bytes > targetPromptExtractionChunkBytes {
			chunks = append(chunks, current)
			current = promptSourceChunk{}
		}
		current.Sources = append(current.Sources, indexed)
		current.Bytes += bytes
	}
	if len(current.Sources) > 0 {
		chunks = append(chunks, current)
	}
	return chunks, nil
}

func mergePromptEvidence(chunks []promptEvidence) promptEvidence {
	merged := mergePromptEvidenceUntrimmed(chunks)
	trimMergedPromptEvidence(&merged)
	return merged
}

func mergePromptEvidencePrioritized(primary, secondary promptEvidence) promptEvidence {
	primary = mergePromptEvidence([]promptEvidence{primary})
	secondary = mergePromptEvidence([]promptEvidence{secondary})
	for {
		merged := mergePromptEvidenceUntrimmed([]promptEvidence{primary, secondary})
		if promptEvidenceEncodedSize(merged) <= maxPromptEvidenceText {
			return merged
		}
		if !removeLastPromptEvidenceItem(&secondary) {
			return primary
		}
	}
}

func mergePromptEvidenceUntrimmed(chunks []promptEvidence) promptEvidence {
	merged := emptyPromptEvidence()
	architecture := map[string]int{}
	lifecycle := map[string]int{}
	flavors := map[string]int{}
	triage := map[string]int{}
	repositories := map[string]int{}
	artifacts := map[string]int{}
	patterns := map[string]int{}
	transients := map[string]int{}
	unresolved := map[string]struct{}{}

	for _, chunk := range chunks {
		mergeEvidenceClaims(&merged.Architecture, architecture, chunk.Architecture)
		mergeEvidenceClaims(&merged.DiagnosticLifecycle, lifecycle, chunk.DiagnosticLifecycle)
		mergeEvidenceClaims(&merged.TestFlavors, flavors, chunk.TestFlavors)
		mergeEvidenceClaims(&merged.TriageOrder, triage, chunk.TriageOrder)
		mergeEvidenceClaims(&merged.Repositories, repositories, chunk.Repositories)
		mergeArtifactEvidence(&merged.Artifacts, artifacts, chunk.Artifacts)
		mergeFailurePatternEvidence(&merged.FailurePatterns, patterns, chunk.FailurePatterns)
		mergeTransientEvidence(&merged.TransientRules, transients, chunk.TransientRules)
		for _, item := range chunk.Unresolved {
			key := normalizedEvidenceKey(item)
			if key == "" || len(merged.Unresolved) >= maxPromptEvidenceItems {
				continue
			}
			if _, ok := unresolved[key]; ok {
				continue
			}
			unresolved[key] = struct{}{}
			merged.Unresolved = append(merged.Unresolved, item)
		}
	}

	return merged
}

func emptyPromptEvidence() promptEvidence {
	return promptEvidence{
		Architecture:        []evidenceClaim{},
		DiagnosticLifecycle: []evidenceClaim{},
		TestFlavors:         []evidenceClaim{},
		Artifacts:           []artifactEvidence{},
		FailurePatterns:     []failurePatternEvidence{},
		TransientRules:      []transientEvidence{},
		TriageOrder:         []evidenceClaim{},
		Repositories:        []evidenceClaim{},
		Unresolved:          []string{},
	}
}

func mergeEvidenceClaims(dst *[]evidenceClaim, indexes map[string]int, values []evidenceClaim) {
	for _, value := range values {
		key := normalizedEvidenceKey(value.Text)
		if index, ok := indexes[key]; ok {
			(*dst)[index].Sources = mergeEvidenceRefs((*dst)[index].Sources, value.Sources)
			continue
		}
		if key == "" || len(*dst) >= maxPromptEvidenceItems {
			continue
		}
		value.Sources = mergeEvidenceRefs(nil, value.Sources)
		indexes[key] = len(*dst)
		*dst = append(*dst, value)
	}
}

func mergeArtifactEvidence(dst *[]artifactEvidence, indexes map[string]int, values []artifactEvidence) {
	for _, value := range values {
		key := artifactEvidenceKey(value.PathPattern)
		if index, ok := indexes[key]; ok {
			(*dst)[index].Sources = mergeEvidenceRefs((*dst)[index].Sources, value.Sources)
			continue
		}
		if key == "" || len(*dst) >= maxPromptEvidenceItems {
			continue
		}
		value.Sources = mergeEvidenceRefs(nil, value.Sources)
		indexes[key] = len(*dst)
		*dst = append(*dst, value)
	}
}

func mergeFailurePatternEvidence(dst *[]failurePatternEvidence, indexes map[string]int, values []failurePatternEvidence) {
	for _, value := range values {
		key := normalizedEvidenceKey(value.Name)
		if index, ok := indexes[key]; ok {
			(*dst)[index].Sources = mergeEvidenceRefs((*dst)[index].Sources, value.Sources)
			continue
		}
		if key == "" || len(*dst) >= maxPromptEvidenceItems {
			continue
		}
		value.Sources = mergeEvidenceRefs(nil, value.Sources)
		if len(value.RequiredEvidence) > maxPromptEvidenceItems {
			value.RequiredEvidence = value.RequiredEvidence[:maxPromptEvidenceItems]
		}
		indexes[key] = len(*dst)
		*dst = append(*dst, value)
	}
}

func mergeTransientEvidence(dst *[]transientEvidence, indexes map[string]int, values []transientEvidence) {
	for _, value := range values {
		key := normalizedEvidenceKey(value.Class)
		if index, ok := indexes[key]; ok {
			(*dst)[index].Sources = mergeEvidenceRefs((*dst)[index].Sources, value.Sources)
			continue
		}
		if key == "" || len(*dst) >= maxPromptEvidenceItems {
			continue
		}
		value.Sources = mergeEvidenceRefs(nil, value.Sources)
		indexes[key] = len(*dst)
		*dst = append(*dst, value)
	}
}

func mergeEvidenceRefs(existing, additional []evidenceRef) []evidenceRef {
	out := append([]evidenceRef(nil), existing...)
	seen := make(map[string]struct{}, len(out))
	for _, ref := range out {
		seen[evidenceRefKey(ref)] = struct{}{}
	}
	for _, ref := range additional {
		if len(out) >= maxPromptEvidenceItems {
			break
		}
		key := evidenceRefKey(ref)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, ref)
	}
	return out
}

func evidenceRefKey(ref evidenceRef) string {
	encoded, _ := json.Marshal(ref)
	return string(encoded)
}

func normalizedEvidenceKey(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func trimMergedPromptEvidence(e *promptEvidence) {
	for promptEvidenceEncodedSize(*e) > maxPromptEvidenceText {
		if !removeLastPromptEvidenceItem(e) {
			return
		}
	}
}

func removeLastPromptEvidenceItem(e *promptEvidence) bool {
	switch {
	case len(e.Unresolved) > 0:
		e.Unresolved = e.Unresolved[:len(e.Unresolved)-1]
	case len(e.Repositories) > 0:
		e.Repositories = e.Repositories[:len(e.Repositories)-1]
	case len(e.TriageOrder) > 0:
		e.TriageOrder = e.TriageOrder[:len(e.TriageOrder)-1]
	case len(e.TransientRules) > 0:
		e.TransientRules = e.TransientRules[:len(e.TransientRules)-1]
	case len(e.FailurePatterns) > 0:
		e.FailurePatterns = e.FailurePatterns[:len(e.FailurePatterns)-1]
	case len(e.Artifacts) > 0:
		e.Artifacts = e.Artifacts[:len(e.Artifacts)-1]
	case len(e.TestFlavors) > 0:
		e.TestFlavors = e.TestFlavors[:len(e.TestFlavors)-1]
	case len(e.DiagnosticLifecycle) > 0:
		e.DiagnosticLifecycle = e.DiagnosticLifecycle[:len(e.DiagnosticLifecycle)-1]
	case len(e.Architecture) > 0:
		e.Architecture = e.Architecture[:len(e.Architecture)-1]
	default:
		return false
	}
	return true
}

func promptEvidenceEncodedSize(e promptEvidence) int {
	encoded, _ := json.Marshal(e)
	return len(encoded)
}

func validatePromptEvidenceRevision(initial, revised promptEvidence) error {
	allowedRefs := map[string]struct{}{}
	for _, ref := range allPromptEvidenceRefs(initial) {
		allowedRefs[evidenceRefKey(ref)] = struct{}{}
	}
	for _, ref := range allPromptEvidenceRefs(revised) {
		if _, ok := allowedRefs[evidenceRefKey(ref)]; !ok {
			return &promptEvidenceValidationError{stage: promptStageStructuredRevision, code: "revision-source", field: "sources"}
		}
	}

	if err := validateRevisedClaims(initial.Architecture, revised.Architecture); err != nil {
		return err
	}
	if err := validateRevisedClaims(initial.DiagnosticLifecycle, revised.DiagnosticLifecycle); err != nil {
		return err
	}
	if err := validateRevisedClaims(initial.TestFlavors, revised.TestFlavors); err != nil {
		return err
	}
	if err := validateRevisedClaims(initial.TriageOrder, revised.TriageOrder); err != nil {
		return err
	}
	initialRepositories := evidenceClaimsByExactText(initial.Repositories)
	for _, claim := range revised.Repositories {
		matched, ok := initialRepositories[exactEvidenceValue(claim.Text)]
		if !ok || claim.Text != matched.Text || !evidenceRefsEqual(claim.Sources, matched.Sources) {
			return revisionContentError("repositories")
		}
	}
	initialArtifacts := artifactsByKey(initial.Artifacts)
	for _, item := range revised.Artifacts {
		matched, ok := initialArtifacts[artifactEvidenceKey(item.PathPattern)]
		if !ok || item.PathPattern != matched.PathPattern || !evidenceRefsEqual(item.Sources, matched.Sources) || item.Purpose != matched.Purpose {
			return revisionContentError("artifacts")
		}
	}
	initialPatterns := failurePatternsByKey(initial.FailurePatterns)
	for _, item := range revised.FailurePatterns {
		matched, ok := initialPatterns[normalizedEvidenceKey(item.Name)]
		if !ok || item.Name != matched.Name || !evidenceRefsEqual(item.Sources, matched.Sources) {
			return revisionContentError("failure_patterns")
		}
		if item.Signal != matched.Signal || item.DoNotConclude != matched.DoNotConclude ||
			item.RemediationLimit != matched.RemediationLimit ||
			!evidenceStringsEqual(item.RequiredEvidence, matched.RequiredEvidence) {
			return revisionContentError("failure_patterns")
		}
	}
	initialTransients := transientRulesByKey(initial.TransientRules)
	for _, item := range revised.TransientRules {
		matched, ok := initialTransients[normalizedEvidenceKey(item.Class)]
		if !ok || item.Class != matched.Class || !evidenceRefsEqual(item.Sources, matched.Sources) ||
			item.OnlyIf != matched.OnlyIf || item.NotTransientIf != matched.NotTransientIf {
			return revisionContentError("transient_rules")
		}
	}
	if !evidenceStringsEqual(revised.Unresolved, initial.Unresolved) {
		return revisionContentError("unresolved")
	}
	return nil
}

func validateRevisedClaims(initial, revised []evidenceClaim) error {
	allowed := map[string][]evidenceClaim{}
	for _, claim := range initial {
		key := exactEvidenceValue(claim.Text)
		allowed[key] = append(allowed[key], claim)
	}
	for _, claim := range revised {
		key := exactEvidenceValue(claim.Text)
		candidates := allowed[key]
		matched := -1
		for i, candidate := range candidates {
			if claim.Text == candidate.Text && evidenceRefsEqual(claim.Sources, candidate.Sources) {
				matched = i
				break
			}
		}
		if matched < 0 {
			return revisionContentError("claims")
		}
		allowed[key] = append(candidates[:matched], candidates[matched+1:]...)
	}
	return nil
}

func revisionContentError(field string) error {
	return &promptEvidenceValidationError{stage: promptStageStructuredRevision, code: "revision-content", field: field}
}

func allPromptEvidenceRefs(e promptEvidence) []evidenceRef {
	var refs []evidenceRef
	appendClaims := func(claims []evidenceClaim) {
		for _, claim := range claims {
			refs = append(refs, claim.Sources...)
		}
	}
	appendClaims(e.Architecture)
	appendClaims(e.DiagnosticLifecycle)
	appendClaims(e.TestFlavors)
	appendClaims(e.TriageOrder)
	appendClaims(e.Repositories)
	for _, item := range e.Artifacts {
		refs = append(refs, item.Sources...)
	}
	for _, item := range e.FailurePatterns {
		refs = append(refs, item.Sources...)
	}
	for _, item := range e.TransientRules {
		refs = append(refs, item.Sources...)
	}
	return refs
}

func evidenceClaimsByExactText(values []evidenceClaim) map[string]evidenceClaim {
	out := make(map[string]evidenceClaim, len(values))
	for _, value := range values {
		out[exactEvidenceValue(value.Text)] = value
	}
	return out
}

func artifactsByKey(values []artifactEvidence) map[string]artifactEvidence {
	out := make(map[string]artifactEvidence, len(values))
	for _, value := range values {
		out[artifactEvidenceKey(value.PathPattern)] = value
	}
	return out
}

func failurePatternsByKey(values []failurePatternEvidence) map[string]failurePatternEvidence {
	out := make(map[string]failurePatternEvidence, len(values))
	for _, value := range values {
		out[normalizedEvidenceKey(value.Name)] = value
	}
	return out
}

func transientRulesByKey(values []transientEvidence) map[string]transientEvidence {
	out := make(map[string]transientEvidence, len(values))
	for _, value := range values {
		out[normalizedEvidenceKey(value.Class)] = value
	}
	return out
}

func evidenceRefsEqual(values, allowed []evidenceRef) bool {
	if len(values) != len(allowed) {
		return false
	}
	counts := make(map[string]int, len(allowed))
	for _, ref := range allowed {
		counts[evidenceRefKey(ref)]++
	}
	for _, ref := range values {
		key := evidenceRefKey(ref)
		if counts[key] == 0 {
			return false
		}
		counts[key]--
	}
	return true
}

func evidenceStringsEqual(values, allowed []string) bool {
	if len(values) != len(allowed) {
		return false
	}
	counts := make(map[string]int, len(allowed))
	for _, value := range allowed {
		counts[exactEvidenceValue(value)]++
	}
	for _, value := range values {
		key := exactEvidenceValue(value)
		if counts[key] == 0 {
			return false
		}
		counts[key]--
	}
	return true
}

func exactEvidenceValue(value string) string {
	return strings.TrimSpace(value)
}

func artifactEvidenceKey(value string) string {
	return strings.TrimSpace(value)
}
