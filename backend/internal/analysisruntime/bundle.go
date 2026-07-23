package analysisruntime

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"gopkg.in/yaml.v3"
)

const (
	// ProjectBundleEnv carries one immutable project bundle from a ConfigMap.
	ProjectBundleEnv = "PROW_AI_ANALYSIS_BUNDLE"
	// ProjectBundleDigestEnv carries the verified bundle SHA-256.
	ProjectBundleDigestEnv = "PROW_AI_ANALYSIS_BUNDLE_SHA256"
	// ProjectBundleConfigMapKey is the immutable ConfigMap data key.
	ProjectBundleConfigMapKey = "bundle.json"
	// ProjectBundleSchemaVersion identifies the JSON bundle schema.
	ProjectBundleSchemaVersion = 1
	// ContainerAnalyzerContractVersion is implemented by the analyzer binary.
	ContainerAnalyzerContractVersion = "dashboard-failure-analyzer-v3"
	// MaxProjectBundleBytes stays below the Linux per-environment-value limit.
	MaxProjectBundleBytes = 96 << 10
)

// ProjectBundleFile is one consumer-owned analysis file.
type ProjectBundleFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// ProjectBundle is the complete immutable input for one analysis Task.
type ProjectBundle struct {
	SchemaVersion   int                       `json:"schema_version"`
	ContractVersion string                    `json:"contract_version"`
	Digest          string                    `json:"digest"`
	Request         ai.FailureAnalysisRequest `json:"request"`
	Files           []ProjectBundleFile       `json:"files"`
}

// BuildProjectBundle loads, sanitizes, and encodes one consumer project bundle.
func BuildProjectBundle(projectDir, contractVersion string, request ai.FailureAnalysisRequest) ([]byte, string, error) {
	if strings.TrimSpace(contractVersion) == "" {
		return nil, "", fmt.Errorf("project bundle contract version is required")
	}
	if err := validateRequest(request); err != nil {
		return nil, "", err
	}
	cfg, err := project.Load(filepath.Join(projectDir, "project.yaml"))
	if err != nil {
		return nil, "", fmt.Errorf("load project bundle config: %w", err)
	}
	if cfg.AI != nil && len(cfg.AI.Headers) > 0 {
		return nil, "", fmt.Errorf("project bundle does not support ai.headers; provider credentials must remain Secret-backed")
	}
	projectYAML, err := readBundleFile(filepath.Join(projectDir, "project.yaml"))
	if err != nil {
		return nil, "", err
	}
	projectYAML, err = sanitizeProjectYAML(projectYAML)
	if err != nil {
		return nil, "", err
	}
	prompt, err := readBundleFile(filepath.Join(projectDir, "prompts", "system.md"))
	if err != nil {
		return nil, "", err
	}
	if strings.TrimSpace(string(prompt)) == "" {
		return nil, "", fmt.Errorf("project bundle prompt is empty")
	}
	files := []ProjectBundleFile{
		{Path: "project.yaml", Content: string(projectYAML)},
		{Path: "prompts/system.md", Content: string(prompt)},
	}
	skillFiles, err := loadBundleSkills(projectDir)
	if err != nil {
		return nil, "", err
	}
	files = append(files, skillFiles...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	bundle := ProjectBundle{
		SchemaVersion: ProjectBundleSchemaVersion, ContractVersion: contractVersion,
		Request: request, Files: files,
	}
	digest, err := projectBundleDigest(bundle)
	if err != nil {
		return nil, "", err
	}
	bundle.Digest = digest
	if err := validateProjectBundle(bundle); err != nil {
		return nil, "", err
	}
	data, err := json.Marshal(bundle)
	if err != nil {
		return nil, "", fmt.Errorf("marshal project bundle: %w", err)
	}
	if len(data) > MaxProjectBundleBytes {
		return nil, "", fmt.Errorf("project bundle is %d bytes, exceeds %d-byte ConfigMap environment limit", len(data), MaxProjectBundleBytes)
	}
	return data, digest, nil
}

// DecodeProjectBundle validates one strict content-addressed bundle.
func DecodeProjectBundle(data []byte) (ProjectBundle, error) {
	var bundle ProjectBundle
	if len(data) == 0 {
		return bundle, fmt.Errorf("project bundle is empty")
	}
	if len(data) > MaxProjectBundleBytes {
		return bundle, fmt.Errorf("project bundle is %d bytes, exceeds %d-byte ConfigMap environment limit", len(data), MaxProjectBundleBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&bundle); err != nil {
		return bundle, fmt.Errorf("decode project bundle: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return bundle, fmt.Errorf("project bundle contains multiple JSON values")
	} else if err != io.EOF {
		return bundle, fmt.Errorf("decode trailing project bundle data: %w", err)
	}
	if err := validateProjectBundle(bundle); err != nil {
		return bundle, err
	}
	return bundle, nil
}

// VerifyProjectBundleDigest compares the external content address to the bundle.
func VerifyProjectBundleDigest(bundle ProjectBundle, expected string) error {
	expected = strings.ToLower(strings.TrimSpace(expected))
	if expected == "" {
		return fmt.Errorf("%s is required", ProjectBundleDigestEnv)
	}
	if len(expected) != len(bundle.Digest) || subtle.ConstantTimeCompare([]byte(expected), []byte(bundle.Digest)) != 1 {
		return fmt.Errorf("project bundle digest mismatch")
	}
	return nil
}

// VerifyProjectBundleContract rejects bundles for another analyzer version.
func VerifyProjectBundleContract(bundle ProjectBundle) error {
	if bundle.ContractVersion != ContainerAnalyzerContractVersion {
		return fmt.Errorf("unsupported container analyzer contract %q, want %q", bundle.ContractVersion, ContainerAnalyzerContractVersion)
	}
	return nil
}

// MaterializeProjectBundle writes the verified project files to a private directory.
func MaterializeProjectBundle(bundle ProjectBundle) (string, func(), error) {
	if err := validateProjectBundle(bundle); err != nil {
		return "", nil, err
	}
	dir, err := os.MkdirTemp("", "prow-ai-project-bundle-")
	if err != nil {
		return "", nil, fmt.Errorf("create project bundle directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	for _, file := range bundle.Files {
		path := filepath.Join(dir, filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			cleanup()
			return "", nil, fmt.Errorf("create project bundle directory for %s: %w", file.Path, err)
		}
		if err := os.WriteFile(path, []byte(file.Content), 0o600); err != nil {
			cleanup()
			return "", nil, fmt.Errorf("write project bundle file %s: %w", file.Path, err)
		}
	}
	return dir, cleanup, nil
}

func loadBundleSkills(projectDir string) ([]ProjectBundleFile, error) {
	dir := filepath.Join(projectDir, "skills")
	info, err := os.Lstat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat project bundle skills: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("project bundle skills path must be a directory, not a symlink")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read project bundle skills: %w", err)
	}
	files := make([]ProjectBundleFile, 0, len(entries))
	for _, entry := range entries {
		ext := strings.ToLower(filepath.Ext(entry.Name()))
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		data, err := readBundleFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		files = append(files, ProjectBundleFile{Path: "skills/" + entry.Name(), Content: string(data)})
	}
	return files, nil
}

func readBundleFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("read project bundle file %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("project bundle file %s must be a regular file, not a symlink", path)
	}
	if info.Size() > MaxProjectBundleBytes {
		return nil, fmt.Errorf("project bundle file %s exceeds %d-byte environment limit", path, MaxProjectBundleBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read project bundle file %s: %w", path, err)
	}
	return data, nil
}

func sanitizeProjectYAML(data []byte) ([]byte, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("parse project bundle config: %w", err)
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("project bundle config must be a YAML mapping")
	}
	if hasYAMLMergeKey(&document) {
		return nil, fmt.Errorf("project bundle config does not support YAML merge keys")
	}
	if hasYAMLAnchorOrAlias(&document) {
		return nil, fmt.Errorf("project bundle config does not support YAML anchors or aliases")
	}
	root := document.Content[0]
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "ai" {
			continue
		}
		aiNode := root.Content[i+1]
		if aiNode.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("project bundle ai config must be a YAML mapping")
		}
		filtered := aiNode.Content[:0]
		for j := 0; j+1 < len(aiNode.Content); j += 2 {
			switch aiNode.Content[j].Value {
			case "api", "endpoint", "model", "headers":
				continue
			default:
				filtered = append(filtered, aiNode.Content[j], aiNode.Content[j+1])
			}
		}
		aiNode.Content = filtered
		break
	}
	stripYAMLComments(&document)
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if err := encoder.Encode(&document); err != nil {
		return nil, fmt.Errorf("encode project bundle config: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("close project bundle config encoder: %w", err)
	}
	return out.Bytes(), nil
}

func hasYAMLMergeKey(node *yaml.Node) bool {
	if node == nil {
		return false
	}
	if node.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(node.Content); i += 2 {
			if node.Content[i].Value == "<<" || node.Content[i].Tag == "!!merge" {
				return true
			}
		}
	}
	for _, child := range node.Content {
		if hasYAMLMergeKey(child) {
			return true
		}
	}
	return false
}

func hasYAMLAnchorOrAlias(node *yaml.Node) bool {
	if node == nil {
		return false
	}
	if node.Anchor != "" || node.Kind == yaml.AliasNode || node.Alias != nil {
		return true
	}
	for _, child := range node.Content {
		if hasYAMLAnchorOrAlias(child) {
			return true
		}
	}
	return false
}

func stripYAMLComments(node *yaml.Node) {
	if node == nil {
		return
	}
	node.HeadComment = ""
	node.LineComment = ""
	node.FootComment = ""
	for _, child := range node.Content {
		stripYAMLComments(child)
	}
}

func validateProjectBundle(bundle ProjectBundle) error {
	if bundle.SchemaVersion != ProjectBundleSchemaVersion {
		return fmt.Errorf("unsupported project bundle schema version %d", bundle.SchemaVersion)
	}
	if strings.TrimSpace(bundle.ContractVersion) == "" {
		return fmt.Errorf("project bundle contract version is required")
	}
	if err := validateRequest(bundle.Request); err != nil {
		return err
	}
	if len(bundle.Files) < 2 {
		return fmt.Errorf("project bundle is missing required project files")
	}
	seen := make(map[string]bool, len(bundle.Files))
	previous := ""
	for _, file := range bundle.Files {
		if !allowedProjectBundlePath(file.Path) {
			return fmt.Errorf("project bundle file path %q is not allowed", file.Path)
		}
		if seen[file.Path] {
			return fmt.Errorf("project bundle contains duplicate file %q", file.Path)
		}
		if previous != "" && file.Path < previous {
			return fmt.Errorf("project bundle files are not sorted")
		}
		seen[file.Path] = true
		previous = file.Path
	}
	if !seen["project.yaml"] || !seen["prompts/system.md"] {
		return fmt.Errorf("project bundle is missing project.yaml or prompts/system.md")
	}
	if strings.TrimSpace(bundleFileContent(bundle.Files, "prompts/system.md")) == "" {
		return fmt.Errorf("project bundle prompt is empty")
	}
	projectYAML := bundleFileContent(bundle.Files, "project.yaml")
	sanitized, err := sanitizeProjectYAML([]byte(projectYAML))
	if err != nil {
		return fmt.Errorf("validate bundled project config: %w", err)
	}
	if !bytes.Equal(sanitized, []byte(projectYAML)) {
		return fmt.Errorf("project bundle config is not in the sanitized v3 form")
	}
	if len(bundle.Digest) != sha256.Size*2 {
		return fmt.Errorf("project bundle digest is invalid")
	}
	if _, err := hex.DecodeString(bundle.Digest); err != nil || bundle.Digest != strings.ToLower(bundle.Digest) {
		return fmt.Errorf("project bundle digest is invalid")
	}
	digest, err := projectBundleDigest(bundle)
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare([]byte(digest), []byte(bundle.Digest)) != 1 {
		return fmt.Errorf("project bundle content digest mismatch")
	}
	return nil
}

func allowedProjectBundlePath(path string) bool {
	if strings.Contains(path, `\`) {
		return false
	}
	if path == "project.yaml" || path == "prompts/system.md" {
		return true
	}
	if !strings.HasPrefix(path, "skills/") || strings.Contains(strings.TrimPrefix(path, "skills/"), "/") {
		return false
	}
	name := strings.TrimPrefix(path, "skills/")
	return name != "" && (strings.HasSuffix(strings.ToLower(name), ".yaml") || strings.HasSuffix(strings.ToLower(name), ".yml"))
}

func bundleFileContent(files []ProjectBundleFile, path string) string {
	for _, file := range files {
		if file.Path == path {
			return file.Content
		}
	}
	return ""
}

func projectBundleDigest(bundle ProjectBundle) (string, error) {
	payload := struct {
		SchemaVersion   int                       `json:"schema_version"`
		ContractVersion string                    `json:"contract_version"`
		Request         ai.FailureAnalysisRequest `json:"request"`
		Files           []ProjectBundleFile       `json:"files"`
	}{bundle.SchemaVersion, bundle.ContractVersion, bundle.Request, bundle.Files}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal project bundle digest payload: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func validateRequest(request ai.FailureAnalysisRequest) error {
	switch {
	case strings.TrimSpace(request.JobID) == "":
		return fmt.Errorf("failure analysis request job_id is required")
	case strings.TrimSpace(request.BuildPrefix) == "":
		return fmt.Errorf("failure analysis request build_prefix is required")
	case strings.TrimSpace(request.Build.BuildID) == "":
		return fmt.Errorf("failure analysis request build.build_id is required")
	case strings.TrimSpace(request.TestCase.Name) == "":
		return fmt.Errorf("failure analysis request test_case.name is required")
	case request.TestCase.Status != "failed":
		return fmt.Errorf("failure analysis request test_case.status must be failed")
	case request.ConsecutiveFailures < 0:
		return fmt.Errorf("failure analysis request consecutive_failures must not be negative")
	default:
		return nil
	}
}
