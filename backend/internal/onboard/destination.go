package onboard

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"unicode"
)

var knownGeneratedFiles = []string{
	".github/workflows/deploy.yml",
	"CHECKLIST.md",
	"deploy/values.yaml",
	"deploy/README.md",
	"PROMPT_HANDOFF.md",
	".opencode/skills/system-prompt-generation/SKILL.md",
}

type destinationConflictError struct {
	paths []string
}

func (e *destinationConflictError) Error() string {
	return fmt.Sprintf("refusing to replace existing scaffold files without --update-existing: %s", strings.Join(e.paths, ", "))
}

func inspectFileDestination(outDir string, files map[string]string, replaceConsumerOwned bool) ([]DestinationFilePlan, []string, error) {
	var err error
	outDir, err = normalizeDashboardConsumerDir(outDir)
	if err != nil {
		return nil, nil, err
	}
	if err := inspectDestinationRoot(outDir); err != nil {
		return nil, nil, err
	}

	actions := make([]DestinationFilePlan, 0, len(files))
	for _, rel := range sortedFilePaths(files) {
		if err := validateDestinationFilePath(rel); err != nil {
			return nil, nil, err
		}
		if err := inspectDestinationParents(outDir, rel); err != nil {
			return nil, nil, err
		}
		ownership := destinationFileOwnership(rel)
		full := filepath.Join(outDir, filepath.FromSlash(rel))
		info, err := os.Lstat(full)
		switch {
		case err == nil:
			if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				return nil, nil, fmt.Errorf("generated file path %s conflicts with a non-regular filesystem entry", full)
			}
			digest, err := digestDestinationFile(full)
			if err != nil {
				return nil, nil, fmt.Errorf("hashing %s: %w", full, err)
			}
			action := destinationActionReplace
			if ownership == destinationOwnershipConsumer && !replaceConsumerOwned {
				action = destinationActionPreserve
			}
			actions = append(actions, DestinationFilePlan{Path: rel, Action: action, Ownership: ownership, ReviewedDigest: digest})
		case os.IsNotExist(err):
			actions = append(actions, DestinationFilePlan{Path: rel, Action: destinationActionCreate, Ownership: ownership})
		default:
			return nil, nil, fmt.Errorf("checking %s: %w", full, err)
		}
	}

	preservedSkills, err := inspectConsumerSkills(outDir)
	if err != nil {
		return nil, nil, err
	}
	actions = append(actions, preservedSkills...)
	sort.Slice(actions, func(i, j int) bool { return actions[i].Path < actions[j].Path })

	planned := make(map[string]struct{}, len(files))
	for rel := range files {
		planned[rel] = struct{}{}
	}
	var stale []string
	for _, rel := range knownGeneratedFiles {
		if _, ok := planned[rel]; ok {
			continue
		}
		if _, err := os.Lstat(filepath.Join(outDir, filepath.FromSlash(rel))); err == nil {
			stale = append(stale, rel)
		} else if !os.IsNotExist(err) {
			return nil, nil, fmt.Errorf("checking stale scaffold file %s: %w", rel, err)
		}
	}
	return actions, stale, nil
}

func destinationFileOwnership(rel string) string {
	if rel == "prompts/system.md" || isConsumerSkillPath(rel) {
		return destinationOwnershipConsumer
	}
	return destinationOwnershipGenerated
}

func isConsumerSkillPath(rel string) bool {
	if path.Dir(rel) != "skills" {
		return false
	}
	ext := strings.ToLower(path.Ext(rel))
	return ext == ".yaml" || ext == ".yml"
}

func inspectConsumerSkills(outDir string) ([]DestinationFilePlan, error) {
	dir := filepath.Join(outDir, "skills")
	info, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("checking consumer skills directory %s: %w", dir, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("consumer skills directory %s conflicts with a non-directory filesystem entry", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading consumer skills directory %s: %w", dir, err)
	}
	var actions []DestinationFilePlan
	for _, entry := range entries {
		rel := path.Join("skills", entry.Name())
		if !isConsumerSkillPath(rel) {
			continue
		}
		entryInfo, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("checking consumer skill %s: %w", rel, err)
		}
		if entryInfo.Mode()&os.ModeSymlink != 0 || !entryInfo.Mode().IsRegular() {
			return nil, fmt.Errorf("consumer skill path %s conflicts with a non-regular filesystem entry", rel)
		}
		digest, err := digestDestinationFile(filepath.Join(outDir, filepath.FromSlash(rel)))
		if err != nil {
			return nil, fmt.Errorf("hashing %s: %w", rel, err)
		}
		actions = append(actions, DestinationFilePlan{
			Path: rel, Action: destinationActionPreserve, Ownership: destinationOwnershipConsumer, ReviewedDigest: digest,
		})
	}
	return actions, nil
}

func normalizeDashboardConsumerDir(outDir string) (string, error) {
	outDir = strings.TrimSpace(outDir)
	if outDir == "" {
		return "", fmt.Errorf("dashboard consumer directory is required")
	}
	if strings.IndexFunc(outDir, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("dashboard consumer directory must not contain control characters")
	}
	return filepath.Clean(outDir), nil
}

func inspectDestinationRoot(outDir string) error {
	info, err := os.Lstat(outDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking dashboard consumer directory %s: %w", outDir, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("dashboard consumer directory %s conflicts with a non-directory filesystem entry", outDir)
	}
	return nil
}

func inspectDestinationParents(outDir, rel string) error {
	current := filepath.Clean(outDir)
	parent := path.Dir(rel)
	if parent == "." {
		return nil
	}
	for _, part := range strings.Split(parent, "/") {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("checking generated file parent %s: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("generated file parent %s conflicts with a non-directory filesystem entry", current)
		}
	}
	return nil
}

func validateDestinationFilePath(rel string) error {
	if rel == "" || rel == ".." || strings.HasPrefix(rel, "../") || path.IsAbs(rel) || path.Clean(rel) != rel || strings.Contains(rel, "\\") || strings.IndexFunc(rel, unicode.IsControl) >= 0 {
		return fmt.Errorf("generated file path %q is not a safe repo-relative path", rel)
	}
	return nil
}

func hasDestinationReplacements(files []DestinationFilePlan) bool {
	for _, file := range files {
		if file.Action == destinationActionReplace {
			return true
		}
	}
	return false
}

func destinationReplacementPaths(files []DestinationFilePlan) []string {
	var paths []string
	for _, file := range files {
		if file.Action == destinationActionReplace {
			paths = append(paths, file.Path)
		}
	}
	return paths
}

func writeFiles(outDir string, files map[string]string, updateExisting, replaceConsumerOwned bool, expected []DestinationFilePlan) error {
	var err error
	outDir, err = normalizeDashboardConsumerDir(outDir)
	if err != nil {
		return err
	}
	actions, _, err := inspectFileDestination(outDir, files, replaceConsumerOwned)
	if err != nil {
		return err
	}
	if len(expected) > 0 && !reflect.DeepEqual(actions, expected) {
		return fmt.Errorf("dashboard consumer directory changed after review; rerun onboarding before writing")
	}
	if replacements := destinationReplacementPaths(actions); len(replacements) > 0 && !updateExisting {
		return &destinationConflictError{paths: replacements}
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("creating dashboard consumer directory %s: %w", outDir, err)
	}
	if err := inspectDestinationRoot(outDir); err != nil {
		return err
	}
	root, err := os.OpenRoot(outDir)
	if err != nil {
		return fmt.Errorf("opening dashboard consumer directory %s: %w", outDir, err)
	}
	defer root.Close()
	for _, action := range actions {
		content, ok := files[action.Path]
		if action.Action != destinationActionPreserve && !ok {
			return fmt.Errorf("reviewed destination file %s has no generated content", action.Path)
		}
		if err := writeReviewedFileAtRoot(root, action, []byte(content)); err != nil {
			return err
		}
	}
	return nil
}

func writeReviewedFile(outDir string, reviewed DestinationFilePlan, content []byte) error {
	outDir, err := normalizeDashboardConsumerDir(outDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("creating dashboard consumer directory %s: %w", outDir, err)
	}
	if err := inspectDestinationRoot(outDir); err != nil {
		return err
	}
	root, err := os.OpenRoot(outDir)
	if err != nil {
		return fmt.Errorf("opening dashboard consumer directory %s: %w", outDir, err)
	}
	defer root.Close()
	return writeReviewedFileAtRoot(root, reviewed, content)
}

func writeReviewedFileAtRoot(root *os.Root, reviewed DestinationFilePlan, content []byte) error {
	if reviewed.Action != destinationActionPreserve {
		if err := ensureDestinationParents(root, reviewed.Path); err != nil {
			return err
		}
	}
	actual, err := currentDestinationAction(root, reviewed.Path)
	if err != nil {
		return err
	}
	expectedActual := reviewed.Action
	if reviewed.Action == destinationActionPreserve {
		expectedActual = destinationActionReplace
	}
	if actual != expectedActual {
		return fmt.Errorf("dashboard consumer directory changed after review; %s is now %s instead of %s", reviewed.Path, actual, reviewed.Action)
	}
	if reviewed.Action == destinationActionReplace || reviewed.Action == destinationActionPreserve {
		actualDigest, err := digestDestinationFileAtRoot(root, reviewed.Path)
		if err != nil {
			return fmt.Errorf("hashing %s before %s: %w", reviewed.Path, reviewed.Action, err)
		}
		if actualDigest != reviewed.ReviewedDigest {
			return fmt.Errorf("dashboard consumer directory changed after review; %s content no longer matches the reviewed digest", reviewed.Path)
		}
		if reviewed.Action == destinationActionPreserve {
			return nil
		}
		if err := replaceFileAtomic(root, reviewed.Path, content); err != nil {
			return fmt.Errorf("replacing %s: %w", reviewed.Path, err)
		}
		return nil
	}
	if err := createFileExclusive(root, reviewed.Path, content); err != nil {
		return fmt.Errorf("creating %s: %w", reviewed.Path, err)
	}
	return nil
}

func digestDestinationFile(filename string) (string, error) {
	file, err := os.Open(filename)
	if err != nil {
		return "", err
	}
	defer file.Close()
	return digestDestinationReader(file)
}

func digestDestinationFileAtRoot(root *os.Root, filename string) (string, error) {
	file, err := root.Open(filename)
	if err != nil {
		return "", err
	}
	defer file.Close()
	return digestDestinationReader(file)
}

func digestDestinationReader(reader io.Reader) (string, error) {
	hash := sha256.New()
	if _, err := io.Copy(hash, reader); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func ensureDestinationParents(root *os.Root, rel string) error {
	parent := path.Dir(rel)
	if parent == "." {
		return nil
	}
	current := ""
	for _, part := range strings.Split(parent, "/") {
		if current == "" {
			current = part
		} else {
			current = path.Join(current, part)
		}
		info, err := root.Lstat(current)
		if os.IsNotExist(err) {
			if err := root.Mkdir(current, 0o755); err != nil && !os.IsExist(err) {
				return fmt.Errorf("creating generated file parent %s: %w", current, err)
			}
			info, err = root.Lstat(current)
		}
		if err != nil {
			return fmt.Errorf("checking generated file parent %s: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("generated file parent %s conflicts with a non-directory filesystem entry", current)
		}
	}
	return nil
}

func currentDestinationAction(root *os.Root, rel string) (string, error) {
	info, err := root.Lstat(rel)
	switch {
	case err == nil:
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return "", fmt.Errorf("generated file path %s conflicts with a non-regular filesystem entry", rel)
		}
		return destinationActionReplace, nil
	case os.IsNotExist(err):
		return destinationActionCreate, nil
	default:
		return "", fmt.Errorf("checking %s: %w", rel, err)
	}
}

func createFileExclusive(root *os.Root, filename string, content []byte) error {
	file, err := root.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	created, _ := file.Stat()
	complete := false
	defer func() {
		_ = file.Close()
		if complete || created == nil {
			return
		}
		current, err := root.Lstat(filename)
		if err == nil && os.SameFile(created, current) {
			_ = root.Remove(filename)
		}
	}()
	if _, err := file.Write(content); err != nil {
		return err
	}
	if err := file.Chmod(0o644); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	complete = true
	return nil
}

func replaceFileAtomic(root *os.Root, filename string, content []byte) error {
	parent := path.Dir(filename)
	for attempt := 0; attempt < 10; attempt++ {
		var suffix [8]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return err
		}
		tempName := fmt.Sprintf(".prow-ai-dashboard-%x", suffix[:])
		if parent != "." {
			tempName = path.Join(parent, tempName)
		}
		temp, err := root.OpenFile(tempName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		cleanup := true
		defer func() {
			_ = temp.Close()
			if cleanup {
				_ = root.Remove(tempName)
			}
		}()
		if _, err := temp.Write(content); err != nil {
			return err
		}
		if err := temp.Chmod(0o644); err != nil {
			return err
		}
		if err := temp.Close(); err != nil {
			return err
		}
		actual, err := currentDestinationAction(root, filename)
		if err != nil {
			return err
		}
		if actual != destinationActionReplace {
			return fmt.Errorf("dashboard consumer directory changed after review; %s is now %s instead of replace", filename, actual)
		}
		if err := root.Rename(tempName, filename); err != nil {
			return err
		}
		cleanup = false
		return nil
	}
	return fmt.Errorf("could not allocate a temporary scaffold file")
}
