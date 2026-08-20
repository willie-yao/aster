package agentanalysis

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
)

var publicGitHubRepositoryPart = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

const (
	WorkspaceSourceMaxFiles         = 100_000
	WorkspaceSourceMaxFileBytes     = int64(64 << 20)
	WorkspaceSourceMaxGitFileBytes  = int64(512 << 20)
	WorkspaceSourceMaxBytes         = int64(1 << 30)
	WorkspaceSourceMaxSnapshotBytes = int64(1536 << 20)
	workspaceSourceTreeMaxJSONBytes = int64(64 << 20)
)

// ValidatePublicGitHubSourceTree bounds one immutable public GitHub tree before cloning it.
func ValidatePublicGitHubSourceTree(ctx context.Context, client *http.Client, apiBaseURL string, source sourceinvestigation.Repository) error {
	if err := sourceinvestigation.ValidateRepository(source); err != nil {
		return err
	}
	if !publicGitHubRepositoryPart.MatchString(source.Owner) || !publicGitHubRepositoryPart.MatchString(source.Name) {
		return fmt.Errorf("source repository is not a public GitHub repository")
	}
	base, err := url.Parse(strings.TrimRight(strings.TrimSpace(apiBaseURL), "/"))
	if err != nil || base.Scheme != "https" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return fmt.Errorf("source tree API base URL is invalid")
	}
	base.Path = path.Join(base.Path, "repos", source.Owner, source.Name, "git", "trees", source.Revision)
	query := base.Query()
	query.Set("recursive", "1")
	base.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base.String(), nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if client == nil {
		client = &http.Client{
			Timeout:       2 * time.Minute,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("inspect public source tree: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("inspect public source tree: HTTP %d", response.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, workspaceSourceTreeMaxJSONBytes+1))
	var document struct {
		Truncated bool `json:"truncated"`
		Tree      []struct {
			Mode string `json:"mode"`
			Type string `json:"type"`
			Size int64  `json:"size"`
		} `json:"tree"`
	}
	if err := decoder.Decode(&document); err != nil {
		return fmt.Errorf("inspect public source tree response: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("inspect public source tree response has trailing data")
	}
	if document.Truncated {
		return fmt.Errorf("public source tree response is truncated")
	}
	files := 0
	var total int64
	for _, entry := range document.Tree {
		switch entry.Type {
		case "tree":
			continue
		case "blob":
			if entry.Mode != "100644" && entry.Mode != "100755" && entry.Mode != "120000" {
				return fmt.Errorf("public source tree contains an unsupported file mode")
			}
		default:
			return fmt.Errorf("public source tree contains an unsupported entry type")
		}
		if entry.Size < 0 || entry.Size > WorkspaceSourceMaxFileBytes {
			return fmt.Errorf("public source tree contains an oversized file")
		}
		files++
		total += entry.Size
		if files > WorkspaceSourceMaxFiles || total > WorkspaceSourceMaxBytes {
			return fmt.Errorf("public source tree exceeds file or byte bounds")
		}
	}
	if files == 0 {
		return fmt.Errorf("public source tree contains no files")
	}
	return nil
}

// ValidateWorkspaceSourceSnapshot bounds one prepared source tree including Git metadata.
func ValidateWorkspaceSourceSnapshot(ctx context.Context, root string) error {
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("source snapshot root is not a safe directory")
	}
	files := 0
	var total int64
	err = filepath.WalkDir(root, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if current == root || entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}
		inGitMetadata := relative == ".git" || strings.HasPrefix(relative, ".git"+string(filepath.Separator))
		var size int64
		if entry.Type()&os.ModeSymlink != 0 {
			if inGitMetadata {
				return fmt.Errorf("source Git metadata contains a symlink")
			}
			target, err := os.Readlink(current)
			if err != nil {
				return err
			}
			size = int64(len(target))
		} else {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("source snapshot contains an unsupported file")
			}
			size = info.Size()
		}
		fileLimit := WorkspaceSourceMaxFileBytes
		if inGitMetadata {
			fileLimit = WorkspaceSourceMaxGitFileBytes
		}
		if size > fileLimit {
			return fmt.Errorf("source snapshot contains an oversized file")
		}
		files++
		total += size
		if files > WorkspaceSourceMaxFiles || total > WorkspaceSourceMaxSnapshotBytes {
			return fmt.Errorf("source snapshot exceeds file or byte bounds")
		}
		return nil
	})
	if err != nil {
		return err
	}
	if files == 0 {
		return fmt.Errorf("source snapshot contains no files")
	}
	return nil
}
