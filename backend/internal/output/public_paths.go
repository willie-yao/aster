package output

import (
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"io/fs"
	"slices"
	"strconv"
	"strings"

	"github.com/willie-yao/aster/backend/internal/models"
)

//go:embed public_paths.json
var publicRootFilesJSON []byte

var publicRootFiles = func() []string {
	var files []string
	if err := json.Unmarshal(publicRootFilesJSON, &files); err != nil {
		panic("decode public data paths: " + err.Error())
	}
	return files
}()

// IsPublicDataPath reports whether name is a canonical public data file path.
// Names are relative to the data directory and use forward slashes.
func IsPublicDataPath(name string) bool {
	if !fs.ValidPath(name) {
		return false
	}
	dir, base, nested := strings.Cut(name, "/")
	if !nested {
		return slices.Contains(publicRootFiles, name)
	}
	if strings.Contains(base, "/") || !strings.HasSuffix(base, ".json") {
		return false
	}
	stem := strings.TrimSuffix(base, ".json")
	switch dir {
	case "jobs":
		id, err := base64.RawURLEncoding.Strict().DecodeString(stem)
		return err == nil && len(id) > 0 && models.JobDataFilename(string(id)) == base
	case "pull-requests":
		number, err := strconv.Atoi(stem)
		return err == nil && number > 0 && models.PullRequestDataFilename(number) == base
	default:
		return false
	}
}
