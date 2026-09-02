package runtime

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// materialize shallow-fetches repo.Ref into dir. Fetch by SHA works on GitHub,
// so a branch, tag, or commit all resolve the same way. A token authenticates
// the fetch via a one-shot http.extraheader so the credential is never written
// to the remote URL or .git/config, keeping it out of any later command output.
func materialize(ctx context.Context, dir string, repo RepoRef) error {
	url := fmt.Sprintf("https://github.com/%s/%s.git", repo.Owner, repo.Name)
	if repo.CloneURL != "" {
		url = repo.CloneURL
	}
	fetch := []string{"fetch", "-q", "--depth", "1", "origin", repo.Ref}
	if repo.Token != "" && repo.CloneURL == "" {
		auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + repo.Token))
		fetch = append([]string{"-c", "http.extraheader=AUTHORIZATION: Basic " + auth}, fetch...)
	}
	steps := [][]string{
		{"init", "-q"},
		{"remote", "add", "origin", url},
		fetch,
		{"-c", "advice.detachedHead=false", "checkout", "-q", "FETCH_HEAD"},
	}
	for _, args := range steps {
		gitArgs := append([]string{"-c", "core.hooksPath=/dev/null"}, args...)
		cmd := exec.CommandContext(ctx, "git", gitArgs...)
		cmd.Dir = dir
		cmd.Env = gitSafeEnvironment()
		var buf bytes.Buffer
		cmd.Stdout, cmd.Stderr = &buf, &buf
		if err := cmd.Run(); err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				return ctx.Err()
			}
			return fmt.Errorf("runtime: git %s: %w: %s", args[0], err, redactToken(tail(buf.String(), 2048), repo.Token))
		}
	}
	return nil
}

func gitSafeEnvironment() []string {
	env := []string{
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_OPTIONAL_LOCKS=0",
	}
	for _, name := range []string{
		"PATH", "HOME", "LANG", "LC_ALL", "LC_CTYPE", "SSL_CERT_FILE", "SSL_CERT_DIR",
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY",
		"http_proxy", "https_proxy", "no_proxy", "all_proxy",
	} {
		if value, ok := os.LookupEnv(name); ok && value != "" {
			env = append(env, name+"="+value)
		}
	}
	return env
}

func tail(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return "...(truncated)...\n" + s[len(s)-max:]
}

func redactToken(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "REDACTED")
}
