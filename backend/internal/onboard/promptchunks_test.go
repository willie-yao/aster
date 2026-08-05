package onboard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestChunkPromptSourcesDeterministicAndBounded(t *testing.T) {
	sources := []promptSource{
		{Path: "docs/c.md", Kind: "markdown", StartLine: 1, EndLine: 1, Text: strings.Repeat("c", 7_000)},
		{Path: "docs/a.md", Kind: "markdown", StartLine: 1, EndLine: 1, Text: strings.Repeat("a", 7_000)},
		{Path: "docs/b.md", Kind: "markdown", StartLine: 1, EndLine: 1, Text: strings.Repeat("b", 7_000)},
	}
	first, err := chunkPromptSources(sources)
	if err != nil {
		t.Fatal(err)
	}
	second, err := chunkPromptSources([]promptSource{sources[1], sources[2], sources[0]})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(first) != fmt.Sprint(second) {
		t.Fatalf("chunking changed with input order:\nfirst=%v\nsecond=%v", first, second)
	}
	if len(first) != 3 {
		t.Fatalf("chunks = %d, want 3", len(first))
	}
	for i, chunk := range first {
		if chunk.Bytes > maxPromptExtractionChunkBytes {
			t.Fatalf("chunk %d bytes = %d, limit %d", i, chunk.Bytes, maxPromptExtractionChunkBytes)
		}
	}
	var paths []string
	for _, chunk := range first {
		for _, source := range chunk.Sources {
			paths = append(paths, source.Source.Path)
		}
	}
	if got := strings.Join(paths, ","); got != "docs/a.md,docs/b.md,docs/c.md" {
		t.Fatalf("source order = %s", got)
	}
}

func TestChunkPromptSourcesRejectsOversizedSerializedSource(t *testing.T) {
	_, err := chunkPromptSources([]promptSource{{Path: "docs/large.md", Kind: "markdown", StartLine: 1, EndLine: 1, Text: strings.Repeat("x", maxPromptExtractionChunkBytes)}})
	if err == nil {
		t.Fatal("expected oversized source to be rejected")
	}
}

func TestPromptEvidencePhaseRejectsFieldsFromAnotherPhase(t *testing.T) {
	input := promptTestInput("Project", []promptSource{{Path: "docs/a.md", Kind: "markdown", StartLine: 1, EndLine: 1, Text: "Project docs."}})
	raw := []byte(`{"architecture":[],"diagnostic_lifecycle":[],"repositories":[],"artifacts":[]}`)
	var target promptEvidence
	err := decodeAndValidatePromptEvidencePhase(raw, promptExtractionPhases[0], input, nil, maxPromptChunkEvidenceItems, maxPromptChunkNestedItems, &target)
	var validation *promptEvidenceValidationError
	if !errors.As(err, &validation) || validation.code != "decode" || validation.field != "artifacts" {
		t.Fatalf("error = %#v", err)
	}
}

func TestPromptEvidencePhaseStringLimitCountsUnicodeCharacters(t *testing.T) {
	ref := evidenceRef{Path: strings.Repeat("é", maxPromptChunkStringLength), StartLine: 1, EndLine: 1}
	evidence := emptyPromptEvidence()
	evidence.Architecture = []evidenceClaim{{Text: strings.Repeat("é", maxPromptChunkStringLength), Sources: []evidenceRef{ref}}}
	if err := validatePromptEvidenceStringLimit(evidence, maxPromptChunkStringLength); err != nil {
		t.Fatalf("300 Unicode characters rejected: %v", err)
	}
	evidence.Architecture[0].Text += "é"
	if err := validatePromptEvidenceStringLimit(evidence, maxPromptChunkStringLength); err == nil {
		t.Fatal("301 Unicode characters were accepted")
	}
	evidence.Architecture[0].Text = "short"
	evidence.Architecture[0].Sources[0].Path += "é"
	if err := validatePromptEvidenceStringLimit(evidence, maxPromptChunkStringLength); err == nil {
		t.Fatal("301-character source path was accepted")
	}
}

func TestChunkEvidenceLimitIsValidatedWithoutSchemaSupport(t *testing.T) {
	input := promptTestInput("Project", []promptSource{{Path: "docs/a.md", Kind: "markdown", StartLine: 1, EndLine: 1, Text: "grounded source"}})
	evidence := emptyPromptEvidence()
	for i := 0; i < maxPromptChunkEvidenceItems+1; i++ {
		evidence.Architecture = append(evidence.Architecture, evidenceClaim{Text: fmt.Sprintf("Claim %d", i)})
	}
	var target promptEvidence
	err := decodeAndValidatePromptEvidenceWithLimit([]byte(evidenceJSON(evidence)), input, nil, maxPromptChunkEvidenceItems, maxPromptChunkNestedItems, &target)
	var validation *promptEvidenceValidationError
	if !errors.As(err, &validation) || validation.code != "item-limit" || validation.stage != promptStageEvidenceExtraction {
		t.Fatalf("error = %#v", err)
	}
}

func TestGeneratePromptBodyMergesChunkedEvidence(t *testing.T) {
	input, first, second := twoChunkPromptInput()
	c := &stubCompleter{outputs: []string{evidenceJSON(first), evidenceJSON(second)}}
	result, err := generatePromptBodyDetailed(context.Background(), c, input)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExtractionChunks != 2 || result.CompletedExtractionChunks != 2 || result.ExtractionAttempts != 6 || c.calls != 6 {
		t.Fatalf("result=%+v calls=%d", result, c.calls)
	}
	for _, want := range []string{first.Architecture[0].Text, second.DiagnosticLifecycle[0].Text} {
		if !strings.Contains(result.Body, want) {
			t.Fatalf("merged body missing %q:\n%s", want, result.Body)
		}
	}
	for _, request := range c.users[:len(promptExtractionPhases)] {
		if strings.Contains(request, "Initialization B precedes readiness checks") {
			t.Fatal("first chunk request included second chunk source text")
		}
	}
	for _, request := range c.users[len(promptExtractionPhases) : 2*len(promptExtractionPhases)] {
		if strings.Contains(request, "Controller A reconciles Project resources") {
			t.Fatal("second chunk request included first chunk source text")
		}
	}
}

func TestGeneratePromptBodyRetriesInvalidChunkOnce(t *testing.T) {
	input := promptTestInput("Project", []promptSource{{Path: "docs/a.md", Kind: "markdown", StartLine: 1, EndLine: 1, Text: "Controller reconciles Project resources."}})
	evidence := emptyPromptEvidence()
	evidence.Architecture = []evidenceClaim{{Text: "Controller reconciles Project resources.", Sources: []evidenceRef{{Path: "docs/a.md", StartLine: 1, EndLine: 1}}}}
	c := &stubCompleter{
		out:          evidenceJSON(evidence),
		physicalErrs: []error{errors.New("invalid structured response")},
	}
	result, err := generatePromptBodyDetailed(context.Background(), c, input)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExtractionAttempts != 4 || result.CompletedExtractionChunks != 1 || c.calls != 4 {
		t.Fatalf("result=%+v calls=%d", result, c.calls)
	}
	if !strings.Contains(c.systems[1], promptEvidenceExtractionRetryInstruction) {
		t.Fatal("retry instruction was not applied")
	}
}

func TestGeneratePromptBodyChunkFailureRejectsPartialEvidence(t *testing.T) {
	input, first, _ := twoChunkPromptInput()
	c := &stubCompleter{out: evidenceJSON(first), physicalErrs: []error{nil, nil, nil, errors.New("private provider response"), errors.New("private provider response")}}
	result, err := generatePromptBodyDetailed(context.Background(), c, input)
	if err == nil {
		t.Fatal("expected chunk failure")
	}
	if result.Body != "" || result.ExtractionChunks != 2 || result.CompletedExtractionChunks != 1 || result.ExtractionAttempts != 5 || c.calls != 5 {
		t.Fatalf("partial extraction was retained: result=%+v calls=%d", result, c.calls)
	}
}

func TestGeneratePromptBodyChunkCancellationPropagates(t *testing.T) {
	input, first, _ := twoChunkPromptInput()
	c := &stubCompleter{out: evidenceJSON(first), physicalErrs: []error{nil, nil, nil, context.Canceled}}
	result, err := generatePromptBodyDetailed(context.Background(), c, input)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	if result.ExtractionChunks != 2 || result.CompletedExtractionChunks != 1 || result.ExtractionAttempts != 4 {
		t.Fatalf("result = %+v", result)
	}
}

func TestMergePromptEvidenceCombinesDuplicateReferences(t *testing.T) {
	firstRef := evidenceRef{Path: "docs/a.md", StartLine: 1, EndLine: 1}
	secondRef := evidenceRef{Path: "docs/b.md", StartLine: 2, EndLine: 2}
	first := emptyPromptEvidence()
	first.Architecture = []evidenceClaim{{Text: "Controller reconciles resources.", Sources: []evidenceRef{firstRef}}}
	first.Artifacts = []artifactEvidence{{PathPattern: "artifacts/manager.log", Purpose: "Shows controller failures.", Sources: []evidenceRef{firstRef}}}
	second := emptyPromptEvidence()
	second.Architecture = []evidenceClaim{{Text: " controller reconciles resources. ", Sources: []evidenceRef{secondRef, firstRef}}}
	second.Artifacts = []artifactEvidence{
		{PathPattern: "artifacts/manager.log", Purpose: "Conflicting later purpose.", Sources: []evidenceRef{secondRef}},
		{PathPattern: "ARTIFACTS/MANAGER.LOG", Purpose: "Case-distinct artifact.", Sources: []evidenceRef{secondRef}},
	}

	merged := mergePromptEvidence([]promptEvidence{first, second})
	if len(merged.Architecture) != 1 || len(merged.Architecture[0].Sources) != 2 {
		t.Fatalf("architecture = %+v", merged.Architecture)
	}
	if len(merged.Artifacts) != 2 || merged.Artifacts[0].Purpose != first.Artifacts[0].Purpose || len(merged.Artifacts[0].Sources) != 2 || merged.Artifacts[1].PathPattern != "ARTIFACTS/MANAGER.LOG" {
		t.Fatalf("artifacts = %+v", merged.Artifacts)
	}
}

func TestMergePromptEvidencePrioritizesEngineMetadataNearByteCap(t *testing.T) {
	metadata := emptyPromptEvidence()
	metadata.Repositories = []evidenceClaim{{Text: "example/project", Sources: []evidenceRef{{Path: "engine://source-repository", StartLine: 1, EndLine: 1}}}}
	metadata.TestFlavors = []evidenceClaim{{Text: "Name: periodic-project-main; Type: periodic", Sources: []evidenceRef{{Path: "engine://prow-jobs", StartLine: 1, EndLine: 1}}}}

	model := emptyPromptEvidence()
	model.Repositories = []evidenceClaim{{Text: "EXAMPLE/PROJECT", Sources: []evidenceRef{{Path: "docs/repo.md", StartLine: 1, EndLine: 1}}}}
	for i := 0; i < maxPromptEvidenceItems; i++ {
		model.Architecture = append(model.Architecture, evidenceClaim{
			Text:    fmt.Sprintf("Model architecture %02d %s", i, strings.Repeat("detail ", 190)),
			Sources: []evidenceRef{{Path: fmt.Sprintf("docs/%02d.md", i), StartLine: 1, EndLine: 1}},
		})
	}

	merged := mergePromptEvidencePrioritized(metadata, model)
	if size := promptEvidenceEncodedSize(merged); size > maxPromptEvidenceText {
		t.Fatalf("merged evidence bytes = %d, limit %d", size, maxPromptEvidenceText)
	}
	if len(merged.Repositories) == 0 || merged.Repositories[0].Text != "example/project" {
		t.Fatalf("authoritative repository was not preserved: %+v", merged.Repositories)
	}
	if len(merged.TestFlavors) == 0 || !strings.Contains(merged.TestFlavors[0].Text, "periodic-project-main") {
		t.Fatalf("representative job was not preserved: %+v", merged.TestFlavors)
	}
}

func TestMergePromptEvidenceEnforcesCaps(t *testing.T) {
	chunk := emptyPromptEvidence()
	for i := 0; i < maxPromptEvidenceItems+20; i++ {
		refs := make([]evidenceRef, 0, maxPromptEvidenceItems+20)
		for j := 0; j < maxPromptEvidenceItems+20; j++ {
			refs = append(refs, evidenceRef{Path: fmt.Sprintf("docs/%03d-%03d.md", i, j), StartLine: 1, EndLine: 1})
		}
		chunk.Architecture = append(chunk.Architecture, evidenceClaim{Text: fmt.Sprintf("Claim %03d %s", i, strings.Repeat("detail ", 200)), Sources: refs})
		chunk.Unresolved = append(chunk.Unresolved, fmt.Sprintf("Gap %03d %s", i, strings.Repeat("detail ", 200)))
	}
	merged := mergePromptEvidence([]promptEvidence{chunk})
	if len(merged.Architecture) > maxPromptEvidenceItems || len(merged.Unresolved) > maxPromptEvidenceItems {
		t.Fatalf("item caps exceeded: architecture=%d unresolved=%d", len(merged.Architecture), len(merged.Unresolved))
	}
	for _, claim := range merged.Architecture {
		if len(claim.Sources) > maxPromptEvidenceItems {
			t.Fatalf("reference cap exceeded: %d", len(claim.Sources))
		}
	}
	if size := promptEvidenceEncodedSize(merged); size > maxPromptEvidenceText {
		t.Fatalf("merged evidence bytes = %d, limit %d", size, maxPromptEvidenceText)
	}
}

func TestPromptDebugReportsChunkProgressWithoutCredentials(t *testing.T) {
	const token = "chunk-progress-secret"
	var out bytes.Buffer
	debug := newPromptDebugger(true, &out, token)
	debug.extractionChunks(4, 2, 5)
	text := out.String()
	if !strings.Contains(text, "extraction_chunks_total=4 extraction_chunks_completed=2 extraction_attempts=5") {
		t.Fatalf("debug output = %s", text)
	}
	if strings.Contains(text, token) {
		t.Fatalf("debug output exposed credential: %s", text)
	}
}

func twoChunkPromptInput() (promptDraftInput, promptEvidence, promptEvidence) {
	firstSource := promptSource{
		Path: "docs/a.md", Kind: "markdown", StartLine: 1, EndLine: 1,
		Text: "Controller A reconciles Project resources. " + strings.Repeat("architecture ", 700),
	}
	secondSource := promptSource{
		Path: "docs/b.md", Kind: "markdown", StartLine: 1, EndLine: 1,
		Text: "Initialization B precedes readiness checks. " + strings.Repeat("lifecycle ", 800),
	}
	first := emptyPromptEvidence()
	first.Architecture = []evidenceClaim{{
		Text:    "Controller A reconciles Project resources.",
		Sources: []evidenceRef{{Path: firstSource.Path, StartLine: 1, EndLine: 1}},
	}}
	second := emptyPromptEvidence()
	second.DiagnosticLifecycle = []evidenceClaim{{
		Text:    "Initialization B precedes readiness checks.",
		Sources: []evidenceRef{{Path: secondSource.Path, StartLine: 1, EndLine: 1}},
	}}
	return promptTestInput("Project", []promptSource{secondSource, firstSource}), first, second
}
