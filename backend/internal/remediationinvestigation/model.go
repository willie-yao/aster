package remediationinvestigation

import "github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"

func resultFormat() ai.ResponseFormat {
	stringArray := func(max int) map[string]any {
		return map[string]any{"type": "array", "maxItems": max, "items": map[string]any{"type": "string"}}
	}
	repository := map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"owner": map[string]any{"type": "string"}, "name": map[string]any{"type": "string"}, "revision": map[string]any{"type": "string"},
		},
		"required": []string{"owner", "name", "revision"},
	}
	target := map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"intent": map[string]any{"type": "string"}, "symbol": map[string]any{"type": "string"},
			"required_call": map[string]any{"type": "string"}, "path": map[string]any{"type": "string"},
			"value": map[string]any{"type": "string"}, "repository": map[string]any{"type": "string"},
			"revision": map[string]any{"type": "string"}, "job": map[string]any{"type": "string"},
			"container": map[string]any{"type": "string"}, "name": map[string]any{"type": "string"},
		},
		"required": []string{"intent", "symbol", "required_call", "path", "value", "repository", "revision", "job", "container", "name"},
	}
	proposal := map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"target_kind": map[string]any{"type": "string", "enum": []string{
				string(TargetAddSymbol), string(TargetModifySymbol), string(TargetAddRequiredCall),
				string(TargetSetConfiguration), string(TargetRemoveConfiguration), string(TargetSetJobEnvironment),
			}},
			"repository": repository, "target": target,
			"expected_behavior":         map[string]any{"type": "string"},
			"relationship_proof":        map[string]any{"type": "string"},
			"current_source":            map[string]any{"type": "string", "enum": []string{string(CurrentSourcePresent), string(CurrentSourceAbsent), string(CurrentSourceUnknown)}},
			"verification_requirements": stringArray(20), "allowed_changed_paths": stringArray(20),
			"allowed_validation_commands": stringArray(20),
		},
		"required": []string{"target_kind", "repository", "target", "expected_behavior", "relationship_proof", "current_source", "verification_requirements", "allowed_changed_paths", "allowed_validation_commands"},
	}
	evidence := map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"kind":     map[string]any{"type": "string", "enum": []string{string(EvidenceSource), string(EvidenceAnalysis), string(EvidenceArtifact)}},
			"build_id": map[string]any{"type": "string"}, "path": map[string]any{"type": "string"},
			"line_start": map[string]any{"type": "integer"}, "line_end": map[string]any{"type": "integer"},
			"quote": map[string]any{"type": "string"}, "analysis_generated_at": map[string]any{"type": "string"},
		},
		"required": []string{"kind", "build_id", "path", "line_start", "line_end", "quote", "analysis_generated_at"},
	}
	return ai.ResponseFormat{
		Name:        "submit_remediation_investigation",
		Description: "Submit one typed, evidence-backed remediation classification for the frozen causal group.",
		Schema: map[string]any{
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{
				"version": map[string]any{"type": "integer", "enum": []int{ResultVersion}},
				"classification": map[string]any{"type": "string", "enum": []string{
					string(ClassificationActionable), string(ClassificationAlreadyFixed), string(ClassificationExternalDependency),
					string(ClassificationEnvironmentOrInfrastructure), string(ClassificationMitigationOnly), string(ClassificationInsufficientEvidence),
				}},
				"reason":                  map[string]any{"type": "string"},
				"cause_assessment":        map[string]any{"type": "string", "enum": []string{string(CauseSupports), string(CauseRefines), string(CauseContradicts), string(CauseInconclusive)}},
				"cause_assessment_reason": map[string]any{"type": "string"},
				"proposal":                map[string]any{"anyOf": []any{proposal, map[string]any{"type": "null"}}},
				"evidence":                map[string]any{"type": "array", "minItems": 1, "maxItems": 16, "items": evidence},
			},
			"required": []string{"version", "classification", "reason", "cause_assessment", "cause_assessment_reason", "proposal", "evidence"},
		},
	}
}
