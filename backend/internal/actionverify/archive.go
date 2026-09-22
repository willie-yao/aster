package actionverify

import (
	"path"
	"strings"
)

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
