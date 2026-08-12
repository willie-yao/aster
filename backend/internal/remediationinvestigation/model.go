package remediationinvestigation

import "github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"

func resultFormat() ai.ResponseFormat {
	candidate := map[string]any{"anyOf": []any{
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
		map[string]any{"type": "null"},
	}}
	return ai.ResponseFormat{
		Name:        "submit_remediation_investigation",
		Description: "Submit one minimal candidate target or one typed non-actionable reason using only engine-issued evidence IDs.",
		Schema: map[string]any{
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{
				"version":          map[string]any{"type": "integer", "enum": []int{ResultVersion}},
				"cause_assessment": map[string]any{"type": "string", "enum": []string{string(CauseSupports), string(CauseRefines), string(CauseContradicts), string(CauseInconclusive)}},
				"reason":           map[string]any{"type": "string"},
				"candidate":        candidate,
				"evidence_ids":     map[string]any{"type": "array", "minItems": 1, "maxItems": 128, "items": map[string]any{"type": "string"}},
				"non_actionable_reason": map[string]any{"anyOf": []any{
					map[string]any{"type": "string", "enum": []string{
						string(NonActionableEnvironmentOrInfrastructure), string(NonActionableMitigationOnly),
						string(NonActionableInsufficientEvidence), string(NonActionableDependencyOwnershipUnverified),
					}},
					map[string]any{"type": "null"},
				}},
			},
			"required": []string{"version", "cause_assessment", "reason", "candidate", "evidence_ids", "non_actionable_reason"},
		},
	}
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
