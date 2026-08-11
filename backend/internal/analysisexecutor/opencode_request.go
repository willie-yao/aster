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

func newOpenCodeRequestShape(spec OpenCodeSpec, version string) agentanalysis.WorkspaceOpenCodeRequestShape {
	shape := agentanalysis.WorkspaceOpenCodeRequestShape{
		Available:            true,
		StreamingMode:        "streaming",
		ModelID:              spec.Gateway.Model,
		UserPromptBytes:      len(spec.Prompt),
		ResponseSchemaSHA256: agentanalysis.WorkspaceResultSchemaHash(),
		ToolChoiceMode:       "required",
		ContextLimit:         spec.ModelContextTokens,
		OutputTokenLimit:     spec.ModelOutputTokens,
		OpenCodeVersion:      version,
	}
	if count, err := openCodeSystemPromptBytes(spec, time.Now()); err == nil {
		shape.SystemPromptBytesAvailable = true
		shape.SystemPromptBytes = count
	}
	return shape
}

func openCodeSystemPromptBytes(spec OpenCodeSpec, now time.Time) (int, error) {
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
	if output, gitErr := cmd.Output(); gitErr == nil {
		worktree = strings.TrimSpace(string(output))
		if resolved, resolveErr := filepath.EvalSymlinks(worktree); resolveErr == nil {
			worktree = resolved
		}
		isGit = "yes"
	}
	environment := strings.Join([]string{
		"You are powered by the model named " + spec.Gateway.Model + ". The exact model ID is engine/" + spec.Gateway.Model,
		"Here is some useful information about the environment you are running in:",
		"<env>",
		"  Working directory: " + directory,
		"  Workspace root folder: " + worktree,
		"  Is directory a git repo: " + isGit,
		"  Platform: " + runtime.GOOS,
		"  Today's date: " + now.Format("Mon Jan 2 2006"),
		"</env>",
	}, "\n")
	system := strings.Join([]string{agentanalysis.WorkspaceAgentPrompt(), environment, openCodeStructuredOutputSystemPrompt}, "\n")
	return len([]byte(system)), nil
}

func fetchOpenCodeToolSchemaDigest(ctx context.Context, client *http.Client, baseURL string, spec OpenCodeSpec) (int, string, error) {
	query := url.Values{
		"directory": {spec.WorkDir},
		"provider":  {"engine"},
		"model":     {spec.Gateway.Model},
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
	tools = append(tools, digestToolSchema{Name: "StructuredOutput", Schema: structured})
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	data, err := json.Marshal(tools)
	if err != nil {
		return 0, "", err
	}
	digest := sha256.Sum256(data)
	return len(tools), hex.EncodeToString(digest[:]), nil
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
