package fetcher

import (
	"log"
	"os"

	"github.com/willie-yao/aster/backend/internal/textutil"
)

// credentialEnv reads a secret-bearing variable and strips surrounding
// whitespace, naming the variable so a malformed Secret is visible here rather
// than as an opaque 401 from the model endpoint or the GitHub API.
func credentialEnv(name string) string {
	value, trimmed := textutil.TrimCredential(os.Getenv(name))
	if trimmed {
		log.Printf("⚠️  %s has leading or trailing whitespace; using the trimmed value. Recreate the Secret with `printf %%s` or `echo -n` to drop the stray newline.", name)
	}
	return value
}
