package remediationinvestigation

import "github.com/willie-yao/aster/backend/internal/ai"

func targetExtractionFormat() ai.ResponseFormat {
	hypothesis := map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"target":              targetCandidateSchema(),
			"evidence_ids":        evidenceIDsSchema(),
			"relationship_reason": map[string]any{"type": "string"},
		},
		"required": []string{"target", "evidence_ids", "relationship_reason"},
	}
	return ai.ResponseFormat{
		Name:        "submit_remediation_target_hypotheses",
		Description: "Submit zero to three typed target verification subjects using only engine-issued evidence IDs.",
		Schema: map[string]any{
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{
				"version":    map[string]any{"type": "integer", "enum": []int{TargetExtractionVersion}},
				"hypotheses": map[string]any{"type": "array", "minItems": 0, "maxItems": 3, "items": hypothesis},
			},
			"required": []string{"version", "hypotheses"},
		},
	}
}

func nonActionableAssessmentFormat() ai.ResponseFormat {
	return ai.ResponseFormat{
		Name:        "submit_remediation_non_actionable_assessment",
		Description: "Classify a cause only after no target hypothesis passed deterministic verification.",
		Schema: map[string]any{
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{
				"version":          map[string]any{"type": "integer", "enum": []int{NonActionableAssessmentVersion}},
				"cause_assessment": map[string]any{"type": "string", "enum": []string{string(CauseSupports), string(CauseRefines), string(CauseContradicts), string(CauseInconclusive)}},
				"reason":           map[string]any{"type": "string"},
				"evidence_ids":     evidenceIDsSchema(),
				"non_actionable_reason": map[string]any{"type": "string", "enum": []string{
					string(NonActionableEnvironmentOrInfrastructure), string(NonActionableMitigationOnly),
					string(NonActionableInsufficientEvidence), string(NonActionableDependencyOwnershipUnverified),
				}},
			},
			"required": []string{"version", "cause_assessment", "reason", "evidence_ids", "non_actionable_reason"},
		},
	}
}

func targetCandidateSchema() map[string]any {
	return map[string]any{"anyOf": []any{
		candidateSchema(string(CandidateRequiredCall), map[string]any{
			"path":              map[string]any{"type": "string"},
			"containing_symbol": map[string]any{"type": "string"},
			"required_call":     map[string]any{"type": "string"},
		}, []string{"kind", "path", "containing_symbol", "required_call"}),
		candidateSchema(string(CandidateSymbolAddition), map[string]any{
			"path":   map[string]any{"type": "string"},
			"symbol": map[string]any{"type": "string"},
		}, []string{"kind", "path", "symbol"}),
		candidateSchema(string(CandidateProwEnvironmentEntry), map[string]any{
			"config_path": map[string]any{"type": "string"},
			"job":         map[string]any{"type": "string"},
			"container":   map[string]any{"type": "string"},
			"name":        map[string]any{"type": "string"},
			"value":       map[string]any{"type": "string"},
		}, []string{"kind", "config_path", "job", "container", "name", "value"}),
		candidateSchema(string(CandidateConfigurationField), map[string]any{
			"path":       map[string]any{"type": "string"},
			"field_path": map[string]any{"type": "array", "minItems": 1, "maxItems": 16, "items": map[string]any{"type": "string"}},
			"value":      map[string]any{"type": "string"},
		}, []string{"kind", "path", "field_path", "value"}),
	}}
}

func evidenceIDsSchema() map[string]any {
	return map[string]any{"type": "array", "minItems": 1, "maxItems": 128, "items": map[string]any{"type": "string"}}
}

func candidateSchema(kind string, fields map[string]any, required []string) map[string]any {
	properties := map[string]any{"kind": map[string]any{"type": "string", "enum": []string{kind}}}
	for name, schema := range fields {
		properties[name] = schema
	}
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": properties, "required": required,
	}
}
