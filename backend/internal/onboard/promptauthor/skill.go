// Package promptauthor holds the engine-owned prompt-generation skill and the
// deterministic output contract for prompts/system.md. Generation itself is
// delegated to the operator's own coding agent through the handoff bundle.
package promptauthor

import _ "embed"

const (
	// OutputPath is the only file a prompt-authoring agent may write.
	OutputPath = "prompts/system.md"
	// SkillName is the engine-owned skill the handoff bundle installs.
	SkillName = "system-prompt-generation"
	// maxBytes bounds one generated project prompt.
	maxBytes = 64 << 10
)

//go:embed skill/system-prompt-generation.md
var systemPromptSkill string

// SkillContent returns the engine-owned prompt-generation skill.
func SkillContent() string { return systemPromptSkill }
