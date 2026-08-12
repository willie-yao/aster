package analysisexecutor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/agentanalysis"
)

const (
	maxOpenCodeToolSchemas               = 128
	openCodeStructuredOutputSystemPrompt = "IMPORTANT: The user has requested structured output. You MUST use the StructuredOutput tool to provide your final response. Do NOT respond with plain text - you MUST call the StructuredOutput tool with your answer formatted according to the schema."
)

var analysisNativeToolIDs = []string{"bash", "glob", "grep", "read"}

type openCodeToolSchema struct {
	ID         string          `json:"id"`
	Parameters json.RawMessage `json:"parameters"`
}

type digestToolSchema struct {
	Name   string          `json:"name"`
	Schema json.RawMessage `json:"schema"`
}

func newOpenCodeRequestShape(spec OpenCodeSpec, version, finalizationInstruction string) agentanalysis.WorkspaceOpenCodeRequestShape {
	shape := baseOpenCodeRequestShape(spec, version, spec.Prompt+finalizationInstruction, "required")
	shape.ResponseSchemaSHA256 = agentanalysis.WorkspaceResultSchemaHash()
	if count, digest, err := structuredOutputToolSchemaDigest(); err == nil {
		shape.ToolSchemaAvailable = true
		shape.ToolCount = count
		shape.ToolSchemaSHA256 = digest
	}
	if count, err := openCodeSystemPromptBytes(spec, time.Now()); err == nil {
		shape.SystemPromptBytesAvailable = true
		shape.SystemPromptBytes = count
	}
	return shape
}

func newOpenCodeEvidenceRequestShape(spec OpenCodeSpec, version string) agentanalysis.WorkspaceOpenCodeRequestShape {
	shape := baseOpenCodeRequestShape(spec, version, spec.Prompt, "auto")
	if count, err := openCodeEvidenceSystemPromptBytes(spec, time.Now()); err == nil {
		shape.SystemPromptBytesAvailable = true
		shape.SystemPromptBytes = count
	}
	return shape
}

func baseOpenCodeRequestShape(spec OpenCodeSpec, version, prompt, toolChoice string) agentanalysis.WorkspaceOpenCodeRequestShape {
	return agentanalysis.WorkspaceOpenCodeRequestShape{
		Available:        true,
		StreamingMode:    "streaming",
		ModelID:          spec.Provider.Model,
		UserPromptBytes:  len(prompt),
		ToolChoiceMode:   toolChoice,
		ContextLimit:     spec.ModelContextTokens,
		OutputTokenLimit: spec.ModelOutputTokens,
		OpenCodeVersion:  version,
	}
}

func openCodeSystemPromptBytes(spec OpenCodeSpec, now time.Time) (int, error) {
	return openCodePhaseSystemPromptBytes(spec, now, agentanalysis.WorkspaceFinalizerPrompt(), true)
}

func openCodeEvidenceSystemPromptBytes(spec OpenCodeSpec, now time.Time) (int, error) {
	return openCodePhaseSystemPromptBytes(spec, now, agentanalysis.WorkspaceAgentPrompt(), false)
}

func openCodePhaseSystemPromptBytes(spec OpenCodeSpec, now time.Time, agentPrompt string, structured bool) (int, error) {
	directory, err := filepath.Abs(spec.WorkDir)
	if err != nil {
		return 0, err
	}
	if resolved, resolveErr := filepath.EvalSymlinks(directory); resolveErr == nil {
		directory = resolved
	}
	worktree := string(filepath.Separator)
	isGit := "no"
	cmd := exec.Command("git", "-C", directory, "rev-parse", "--show-toplevel")
	cmd.Env = append(nonCredentialSubprocessEnvironment(), "GIT_OPTIONAL_LOCKS=0", "GIT_CONFIG_NOSYSTEM=1")
	if output, gitErr := cmd.Output(); gitErr == nil {
		worktree = strings.TrimSpace(string(output))
		if resolved, resolveErr := filepath.EvalSymlinks(worktree); resolveErr == nil {
			worktree = resolved
		}
		isGit = "yes"
	}
	environment := strings.Join([]string{
		"You are powered by the model named " + spec.Provider.Model + ". The exact model ID is engine/" + spec.Provider.Model,
		"Here is some useful information about the environment you are running in:",
		"<env>",
		"  Working directory: " + directory,
		"  Workspace root folder: " + worktree,
		"  Is directory a git repo: " + isGit,
		"  Platform: " + runtime.GOOS,
		"  Today's date: " + now.Format("Mon Jan 2 2006"),
		"</env>",
	}, "\n")
	parts := []string{agentPrompt, environment}
	if structured {
		parts = append(parts, openCodeStructuredOutputSystemPrompt)
	}
	return len([]byte(strings.Join(parts, "\n"))), nil
}

func fetchOpenCodeNativeToolSchemaDigest(ctx context.Context, client *http.Client, baseURL string, spec OpenCodeSpec) (int, string, error) {
	query := url.Values{
		"directory": {spec.WorkDir},
		"provider":  {"engine"},
		"model":     {spec.Provider.Model},
	}
	var response []openCodeToolSchema
	if err := openCodeJSON(ctx, client, http.MethodGet, baseURL+"/experimental/tool?"+query.Encode(), nil, &response); err != nil {
		return 0, "", err
	}
	if len(response) == 0 || len(response) > maxOpenCodeToolSchemas {
		return 0, "", fmt.Errorf("OpenCode tool schema count is invalid")
	}
	wanted := map[string]bool{}
	for _, name := range analysisNativeToolIDs {
		wanted[name] = true
	}
	tools := make([]digestToolSchema, 0, len(wanted)+1)
	seen := map[string]bool{}
	for _, item := range response {
		if !wanted[item.ID] {
			continue
		}
		if seen[item.ID] || len(item.Parameters) == 0 {
			return 0, "", fmt.Errorf("OpenCode tool schema is invalid")
		}
		canonical, err := canonicalJSON(item.Parameters)
		if err != nil {
			return 0, "", fmt.Errorf("canonicalize OpenCode tool schema")
		}
		seen[item.ID] = true
		tools = append(tools, digestToolSchema{Name: item.ID, Schema: canonical})
	}
	if len(seen) != len(wanted) {
		return 0, "", fmt.Errorf("OpenCode tool schema set is incomplete")
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	data, err := json.Marshal(tools)
	if err != nil {
		return 0, "", err
	}
	digest := sha256.Sum256(data)
	return len(tools), hex.EncodeToString(digest[:]), nil
}

func structuredOutputToolSchemaDigest() (int, string, error) {
	structuredSchema := agentanalysis.WorkspaceResultSchema()
	delete(structuredSchema, "$schema")
	structured, err := json.Marshal(structuredSchema)
	if err != nil {
		return 0, "", err
	}
	structured, err = canonicalJSON(structured)
	if err != nil {
		return 0, "", err
	}
	data, err := json.Marshal([]digestToolSchema{{Name: "StructuredOutput", Schema: structured}})
	if err != nil {
		return 0, "", err
	}
	digest := sha256.Sum256(data)
	return 1, hex.EncodeToString(digest[:]), nil
}

func canonicalJSON(raw []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("JSON contains trailing data")
	}
	return json.Marshal(value)
}
