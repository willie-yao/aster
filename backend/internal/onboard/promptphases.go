package onboard

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"unicode/utf8"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
)

type promptExtractionPhase struct {
	Name        string
	Fields      []string
	Instruction string
}

var promptExtractionPhases = []promptExtractionPhase{
	{
		Name:        "context",
		Fields:      []string{"architecture", "diagnostic_lifecycle", "repositories"},
		Instruction: "Return only architecture relationships, diagnostic lifecycle claims, and grounded source repositories for this chunk.",
	},
	{
		Name:        "operations",
		Fields:      []string{"test_flavors", "artifacts", "triage_order"},
		Instruction: "Return only test or job flavors, exact artifact evidence, and project-specific triage steps for this chunk.",
	},
	{
		Name:        "patterns",
		Fields:      []string{"failure_patterns", "transient_rules", "unresolved"},
		Instruction: "Return only grounded failure patterns, transient boundaries, and important unresolved details for this chunk.",
	},
}

func promptEvidencePhaseResponseFormat(phase promptExtractionPhase, sectionMaxItems, nestedMaxItems int) ai.ResponseFormat {
	full := promptEvidenceResponseFormat(sectionMaxItems, nestedMaxItems)
	limitPromptSchemaStrings(full.Schema, maxPromptChunkStringLength)
	allProperties := full.Schema["properties"].(map[string]any)
	properties := make(map[string]any, len(phase.Fields))
	for _, field := range phase.Fields {
		properties[field] = allProperties[field]
	}
	return ai.ResponseFormat{
		Name:        "return_prompt_evidence_" + phase.Name,
		Description: "Return one grounded evidence phase for the project diagnostic runbook.",
		Schema:      objectSchema(properties, phase.Fields...),
	}
}

func limitPromptSchemaStrings(value any, maxLength int) {
	switch typed := value.(type) {
	case map[string]any:
		if typed["type"] == "string" {
			typed["maxLength"] = maxLength
		}
		for _, child := range typed {
			limitPromptSchemaStrings(child, maxLength)
		}
	case []any:
		for _, child := range typed {
			limitPromptSchemaStrings(child, maxLength)
		}
	}
}

func validatePromptEvidenceStringLimit(e promptEvidence, maxLength int) error {
	for _, value := range promptEvidenceStrings(e) {
		if length := utf8.RuneCountInString(value); length > maxLength {
			return fmt.Errorf("prompt evidence string has %d characters, limit %d", length, maxLength)
		}
	}
	for _, ref := range allPromptEvidenceRefs(e) {
		if length := utf8.RuneCountInString(ref.Path); length > maxLength {
			return fmt.Errorf("prompt evidence source path has %d characters, limit %d", length, maxLength)
		}
	}
	return nil
}

func decodeAndValidatePromptEvidencePhase(raw json.RawMessage, phase promptExtractionPhase, input promptDraftInput, credentials []string, sectionMaxItems, nestedMaxItems int, target *promptEvidence) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var fields map[string]json.RawMessage
	if err := decoder.Decode(&fields); err != nil {
		return &promptEvidenceValidationError{stage: promptStageEvidenceExtraction, code: "decode", field: phase.Name, cause: err}
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return &promptEvidenceValidationError{stage: promptStageEvidenceExtraction, code: "decode", field: phase.Name, cause: err}
	}
	allowedFields := make(map[string]struct{}, len(phase.Fields))
	for _, field := range phase.Fields {
		allowedFields[field] = struct{}{}
	}
	for field := range fields {
		if _, ok := allowedFields[field]; !ok {
			return &promptEvidenceValidationError{stage: promptStageEvidenceExtraction, code: "decode", field: field, cause: fmt.Errorf("field is not allowed in %s phase", phase.Name)}
		}
	}

	evidence := emptyPromptEvidence()
	for _, field := range phase.Fields {
		value, ok := fields[field]
		if !ok {
			return &promptEvidenceValidationError{stage: promptStageEvidenceExtraction, code: "decode", field: field, cause: fmt.Errorf("missing prompt evidence field")}
		}
		if err := decodePromptEvidenceField(field, value, &evidence); err != nil {
			return &promptEvidenceValidationError{stage: promptStageEvidenceExtraction, code: "decode", field: field, cause: err}
		}
	}
	normalizePromptEvidence(&evidence)
	if err := validatePromptEvidenceStringLimit(evidence, maxPromptChunkStringLength); err != nil {
		return &promptEvidenceValidationError{stage: promptStageEvidenceExtraction, code: "string-limit", field: phase.Name, cause: err}
	}
	if err := validatePromptEvidenceItemLimit(evidence, sectionMaxItems, nestedMaxItems); err != nil {
		return &promptEvidenceValidationError{stage: promptStageEvidenceExtraction, code: "item-limit", field: phase.Name, cause: err}
	}
	if err := validatePromptEvidenceReferences(evidence, input.Sources); err != nil {
		return &promptEvidenceValidationError{stage: promptStageEvidenceGrounding, code: "source-reference", field: phase.Name, cause: err}
	}
	groundPromptEvidence(&evidence, input.Sources)
	limitPromptUnresolved(&evidence, sectionMaxItems)
	if err := validatePromptEvidence(evidence, input, credentials); err != nil {
		return &promptEvidenceValidationError{stage: promptStageEvidenceGrounding, code: "content-grounding", field: phase.Name, cause: err}
	}
	*target = evidence
	return nil
}

func decodePromptEvidenceField(field string, raw json.RawMessage, evidence *promptEvidence) error {
	var target any
	switch field {
	case "architecture":
		target = &evidence.Architecture
	case "diagnostic_lifecycle":
		target = &evidence.DiagnosticLifecycle
	case "test_flavors":
		target = &evidence.TestFlavors
	case "artifacts":
		target = &evidence.Artifacts
	case "failure_patterns":
		target = &evidence.FailurePatterns
	case "transient_rules":
		target = &evidence.TransientRules
	case "triage_order":
		target = &evidence.TriageOrder
	case "repositories":
		target = &evidence.Repositories
	case "unresolved":
		target = &evidence.Unresolved
	default:
		return fmt.Errorf("unknown prompt evidence field %q", field)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	return ensureJSONEOF(decoder)
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}
