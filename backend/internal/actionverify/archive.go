package actionverify

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

const (
	maxTargetArchiveFiles = 200
	maxTargetArchiveBytes = 8 << 20
)

// BoundedSource lists and reads files from one immutable source revision.
type BoundedSource interface {
	ListFiles(context.Context) ([]string, error)
	ReadFile(context.Context, string) (string, bool, error)
}

// BuildTargetArchive loads only files needed to verify structured targets.
func BuildTargetArchive(ctx context.Context, source BoundedSource, targets []models.RemediationTarget) (Archive, error) {
	if source == nil {
		return Archive{}, fmt.Errorf("bounded source reader is unavailable")
	}
	archive := Archive{Paths: map[string]bool{}, GoFiles: map[string]string{}, Files: map[string]string{}}
	filesRead, bytesRead := 0, 0
	read := func(file string) error {
		if _, loaded := archive.GoFiles[file]; loaded {
			return nil
		}
		if _, loaded := archive.Files[file]; loaded {
			return nil
		}
		content, found, err := source.ReadFile(ctx, file)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("pinned source file is unavailable")
		}
		filesRead++
		bytesRead += len(content)
		if filesRead > maxTargetArchiveFiles || bytesRead > maxTargetArchiveBytes {
			return fmt.Errorf("bounded target archive exceeds verification limits")
		}
		archive.Paths[file] = true
		if strings.HasSuffix(file, ".go") {
			archive.GoFiles[file] = content
		} else {
			archive.Files[file] = content
		}
		return nil
	}

	needsTree := false
	for _, target := range targets {
		if target.Intent == models.RemediationIntentInvestigate || target.Intent == models.RemediationIntentSetJobEnvironment {
			continue
		}
		if err := read(target.Path); err != nil {
			return Archive{}, err
		}
		needsTree = needsTree || target.Intent == models.RemediationIntentModifySymbol && target.RequiredCall != ""
	}
	if !needsTree {
		return archive, nil
	}

	paths, err := source.ListFiles(ctx)
	if err != nil {
		return Archive{}, err
	}
	sort.Strings(paths)
	for _, file := range paths {
		archive.Paths[file] = true
	}
	packageDirs := map[string]bool{}
	for _, target := range targets {
		if target.Intent != models.RemediationIntentModifySymbol || target.RequiredCall == "" {
			continue
		}
		importPath, _, ok := RequiredCallParts(target.RequiredCall)
		if !ok {
			continue
		}
		if importPath == "" {
			packageDir := path.Dir(target.Path)
			packageDirs[packageDir] = packageDirs[packageDir] || strings.HasSuffix(target.Path, "_test.go")
			continue
		}
		goModPath := nearestModuleFile(archive.Paths, target.Path)
		if goModPath == "" {
			continue
		}
		if err := read(goModPath); err != nil {
			return Archive{}, err
		}
		modulePath := modulePathFromGoMod(archive.Files[goModPath])
		packageDir, ok := repositoryPackageDir(goModPath, modulePath, importPath)
		if ok && !crossesNestedModule(archive.Paths, goModPath, packageDir) {
			packageDirs[packageDir] = false
		}
	}
	for _, file := range paths {
		includeTests, selected := packageDirs[path.Dir(file)]
		if !selected || !strings.HasSuffix(file, ".go") || !includeTests && strings.HasSuffix(file, "_test.go") {
			continue
		}
		if err := read(file); err != nil {
			return Archive{}, err
		}
	}
	return archive, nil
}

func nearestModuleFile(paths map[string]bool, targetPath string) string {
	dir := path.Dir(targetPath)
	for {
		candidate := "go.mod"
		if dir != "." {
			candidate = path.Join(dir, "go.mod")
		}
		if paths[candidate] {
			return candidate
		}
		if dir == "." {
			return ""
		}
		dir = path.Dir(dir)
	}
}

func repositoryPackageDir(goModPath, modulePath, importPath string) (string, bool) {
	if modulePath == "" || importPath != modulePath && !strings.HasPrefix(importPath, modulePath+"/") {
		return "", false
	}
	moduleDir := path.Dir(goModPath)
	if importPath == modulePath {
		return moduleDir, true
	}
	relative := strings.TrimPrefix(importPath, modulePath+"/")
	if moduleDir == "." {
		return relative, true
	}
	return path.Join(moduleDir, relative), true
}

func crossesNestedModule(paths map[string]bool, goModPath, packageDir string) bool {
	moduleDir := path.Dir(goModPath)
	dir := packageDir
	for dir != moduleDir {
		if dir == "." || dir == "" {
			return true
		}
		if paths[path.Join(dir, "go.mod")] {
			return true
		}
		dir = path.Dir(dir)
	}
	return false
}
