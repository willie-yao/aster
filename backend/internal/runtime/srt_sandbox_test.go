package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSRTSandboxCommandWritesDenyByDefaultSettings(t *testing.T) {
	fake := newFakeSRTPackage(t, SRTVersion)
	work, home, temp := t.TempDir(), t.TempDir(), t.TempDir()
	command := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(command, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	sandbox := &SRTSandbox{Bin: fake, goos: "darwin", lookPath: fakeSRTLookPath(fake), nodeCheck: successfulNodeCheck}
	cmd, err := sandbox.Command(context.Background(), SandboxSpec{
		Command: []string{command, "--flag"}, WorkDir: work, HomeDir: home, TempDir: temp,
		Environment: []string{"PATH=/usr/bin:/bin", "HOME=" + home},
		ReadPaths:   []string{work, home, temp, command}, WritePaths: []string{work, home, temp},
		NetworkDomains: []string{"API.Example.Test:443", "*.registry.example.test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cmd.Args) < 5 || cmd.Args[1] != "--settings" || cmd.Args[3] != "--" {
		t.Fatalf("srt args = %v", cmd.Args)
	}
	settingsPath := cmd.Args[2]
	if filepath.Dir(settingsPath) != filepath.Join(filepath.Dir(temp), ".srt-control") {
		t.Fatalf("settings path = %s", settingsPath)
	}
	if !slices.Equal(cmd.Args[4:], []string{command, "--flag"}) {
		t.Fatalf("wrapped command = %v", cmd.Args[4:])
	}
	if got := environmentMap(cmd.Env); got["HOME"] != home || got["CLAUDE_CODE_TMPDIR"] != temp || got["BUN_TMPDIR"] != temp {
		t.Fatalf("command environment = %v", got)
	}
	settings := readSRTSettings(t, settingsPath)
	if !slices.Equal(settings.Filesystem.DenyRead, []string{"/"}) {
		t.Fatalf("denyRead = %v", settings.Filesystem.DenyRead)
	}
	for _, path := range []string{work, home, temp, command} {
		if !slices.Contains(settings.Filesystem.AllowRead, path) {
			t.Errorf("allowRead does not contain %q: %v", path, settings.Filesystem.AllowRead)
		}
	}
	for _, path := range []string{work, home, temp} {
		if !slices.Contains(settings.Filesystem.AllowWrite, path) {
			t.Errorf("allowWrite does not contain %q: %v", path, settings.Filesystem.AllowWrite)
		}
	}
	for _, path := range []string{filepath.Join(work, ".git"), "/tmp/claude", "/private/tmp/claude"} {
		if !slices.Contains(settings.Filesystem.DenyWrite, path) {
			t.Fatalf("denyWrite does not contain %q: %v", path, settings.Filesystem.DenyWrite)
		}
	}
	if want := []string{"api.example.test:443", "*.registry.example.test"}; !slices.Equal(settings.Network.AllowedDomains, want) {
		t.Fatalf("allowed domains = %v, want %v", settings.Network.AllowedDomains, want)
	}
	if settings.Network.AllowLocalBinding || settings.Network.AllowAllUnixSockets || settings.AllowAppleEvents || settings.EnableWeakerNestedSandbox || settings.EnableWeakerNetworkIsolation {
		t.Fatalf("weaker sandbox option enabled: %+v", settings)
	}
}

func TestSRTSandboxUsesFreshProtectedSettings(t *testing.T) {
	fake := newFakeSRTPackage(t, SRTVersion)
	root := t.TempDir()
	work := filepath.Join(root, "work")
	home := filepath.Join(root, "home")
	temp := filepath.Join(root, "tmp")
	for _, path := range []string{work, home, temp} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	command := filepath.Join(work, "agent")
	if err := os.WriteFile(command, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	sandbox := &SRTSandbox{Bin: fake, goos: "darwin", lookPath: fakeSRTLookPath(fake), nodeCheck: successfulNodeCheck}
	spec := SandboxSpec{
		Command: []string{command}, WorkDir: work, HomeDir: home, TempDir: temp,
		ReadPaths: []string{work, home, temp, command}, WritePaths: []string{work, home, temp},
	}
	first, err := sandbox.Command(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	firstSettings := first.Args[2]
	target := filepath.Join(work, ".git", "config")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(firstSettings); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, firstSettings); err != nil {
		t.Fatal(err)
	}
	second, err := sandbox.Command(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if second.Args[2] == firstSettings {
		t.Fatal("settings path was reused")
	}
	if raw, err := os.ReadFile(target); err != nil || string(raw) != "safe" {
		t.Fatalf("protected target = %q, %v", raw, err)
	}
}

func TestSetSandboxEnvironmentOverridesManagedValues(t *testing.T) {
	got := environmentMap(setSandboxEnvironment(
		[]string{"HOME=/sandbox", "BUN_TMPDIR=/host/tmp", "CLAUDE_CODE_TMPDIR=/host/claude", "HOST_SECRET=value"},
		"BUN_TMPDIR=/sandbox/tmp", "CLAUDE_CODE_TMPDIR=/sandbox/tmp",
	))
	if got["BUN_TMPDIR"] != "/sandbox/tmp" || got["CLAUDE_CODE_TMPDIR"] != "/sandbox/tmp" || got["HOME"] != "/sandbox" {
		t.Fatalf("environment = %v", got)
	}
}

func TestSRTSandboxUnavailable(t *testing.T) {
	command := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(command, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	spec := SandboxSpec{Command: []string{command}, WorkDir: t.TempDir(), HomeDir: t.TempDir(), TempDir: t.TempDir()}
	tests := map[string]*SRTSandbox{
		"missing executable":   {Bin: "missing", goos: "darwin", lookPath: func(string) (string, error) { return "", exec.ErrNotFound }},
		"unsupported platform": {Bin: "srt", goos: "plan9", lookPath: fakeSRTLookPath("unused")},
	}
	for name, sandbox := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := sandbox.Command(context.Background(), spec)
			if !errors.Is(err, ErrSandboxUnavailable) {
				t.Fatalf("error = %v, want ErrSandboxUnavailable", err)
			}
		})
	}
}

func TestCheckSRTNodeVersion(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version string
		wantErr bool
	}{
		{name: "minimum", version: "v20.11.0"},
		{name: "newer", version: "v25.6.0"},
		{name: "too old", version: "v20.10.9", wantErr: true},
		{name: "invalid", version: "unknown", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := filepath.Join(t.TempDir(), "node")
			if err := os.WriteFile(node, []byte("#!/bin/sh\nprintf '%s\n' '"+tc.version+"'\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			err := checkSRTNodeVersion(context.Background(), func(string) (string, error) { return node, nil })
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestCheckSRTNodeVersionHonorsContext(t *testing.T) {
	node := filepath.Join(t.TempDir(), "node")
	if err := os.WriteFile(node, []byte("#!/bin/sh\nsleep 10\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := checkSRTNodeVersion(ctx, func(string) (string, error) { return node, nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context deadline", err)
	}
}

func TestSRTSandboxRejectsLinuxLocalBindingRequest(t *testing.T) {
	command := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(command, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := (&SRTSandbox{goos: "linux"}).Command(context.Background(), SandboxSpec{
		Command: []string{command}, WorkDir: t.TempDir(), HomeDir: t.TempDir(), TempDir: t.TempDir(),
		AllowLocalBind: true,
	})
	if err == nil || !strings.Contains(err.Error(), "cannot expose local bindings") {
		t.Fatalf("error = %v", err)
	}
}

func TestSRTSandboxRunFailsClosedOnPreflightError(t *testing.T) {
	fake := newFakeSRTPackage(t, SRTVersion)
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 7\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	work, home, temp := t.TempDir(), t.TempDir(), t.TempDir()
	command := filepath.Join(work, "agent")
	if err := os.WriteFile(command, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := (&SRTSandbox{Bin: fake, goos: "darwin", lookPath: fakeSRTLookPath(fake), nodeCheck: successfulNodeCheck}).Run(context.Background(), SandboxSpec{
		Command: []string{command}, WorkDir: work, HomeDir: home, TempDir: temp,
		Environment: []string{"PATH=/usr/bin:/bin"},
		ReadPaths:   []string{work, home, temp, command}, WritePaths: []string{work, home, temp},
	})
	if !errors.Is(err, ErrSandboxUnavailable) || !strings.Contains(err.Error(), "preflight failed") {
		t.Fatalf("error = %v", err)
	}
}

func TestSRTSandboxRejectsWrongVersion(t *testing.T) {
	fake := newFakeSRTPackage(t, "0.0.69")
	command := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(command, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := (&SRTSandbox{Bin: fake, goos: "darwin", lookPath: fakeSRTLookPath(fake), nodeCheck: successfulNodeCheck}).Command(context.Background(), SandboxSpec{
		Command: []string{command}, WorkDir: t.TempDir(), HomeDir: t.TempDir(), TempDir: t.TempDir(),
	})
	if !errors.Is(err, ErrSandboxUnavailable) || !strings.Contains(err.Error(), SRTVersion) {
		t.Fatalf("error = %v", err)
	}
}

func TestSRTSandboxRejectsInvalidDirectories(t *testing.T) {
	command := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(command, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := (&SRTSandbox{}).Command(context.Background(), SandboxSpec{
		Command: []string{command}, WorkDir: "relative", HomeDir: t.TempDir(), TempDir: t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "work directory must be absolute") {
		t.Fatalf("error = %v", err)
	}
}

func TestSRTSandboxRejectsLinuxSocketAllowlist(t *testing.T) {
	command := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(command, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := (&SRTSandbox{goos: "linux"}).Command(context.Background(), SandboxSpec{
		Command: []string{command}, WorkDir: t.TempDir(), HomeDir: t.TempDir(), TempDir: t.TempDir(),
		UnixSockets: []string{"/tmp/socket"},
	})
	if err == nil || !strings.Contains(err.Error(), "cannot allow individual Unix sockets") {
		t.Fatalf("error = %v", err)
	}
}

func TestNormalizeSRTDomain(t *testing.T) {
	valid := map[string]string{
		"API.Example.COM":           "api.example.com",
		"*.example.com:8443":        "*.example.com:8443",
		"127.0.0.1:443":             "127.0.0.1:443",
		"localhost:3000":            "localhost:3000",
		"registry.example.internal": "registry.example.internal",
	}
	for input, want := range valid {
		if got, err := normalizeSRTDomain(input); err != nil || got != want {
			t.Errorf("normalizeSRTDomain(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"", "https://example.com", "user:secret@example.com", "example.com/path", "bad..example", "-bad.example", "example.com:99999", "example.com:0443", "internal", "2001:db8::1", "*.com"} {
		if _, err := normalizeSRTDomain(input); err == nil {
			t.Errorf("expected %q to fail", input)
		} else if strings.Contains(err.Error(), "secret") {
			t.Errorf("credential leaked in error: %v", err)
		}
	}
}

func TestNormalizeSandboxPathsRejectsRelative(t *testing.T) {
	if _, err := normalizeSandboxPaths([]string{"relative/path"}, false); err == nil {
		t.Fatal("expected relative path to fail")
	}
}

func newFakeSRTPackage(t *testing.T, version string) string {
	t.Helper()
	root := t.TempDir()
	dist := filepath.Join(root, "dist")
	if err := os.MkdirAll(dist, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dist, "cli.js")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	metadata, err := json.Marshal(map[string]string{"name": srtPackage, "version": version})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "package.json"), metadata, 0o600); err != nil {
		t.Fatal(err)
	}
	return bin
}

func fakeSRTLookPath(bin string) func(string) (string, error) {
	return func(name string) (string, error) {
		if name == bin || name == "srt" {
			return bin, nil
		}
		return "/usr/bin/true", nil
	}
}

func readSRTSettings(t *testing.T, path string) srtSettings {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var settings srtSettings
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatal(err)
	}
	return settings
}

func successfulNodeCheck(context.Context, func(string) (string, error)) error { return nil }

func TestSRTSandboxValidatesNodeFromSandboxPATH(t *testing.T) {
	fake := newFakeSRTPackage(t, SRTVersion)
	binDir := t.TempDir()
	for name, body := range map[string]string{
		"node":         "#!/bin/sh\nprintf 'v20.10.0\\n'\n",
		"bash":         "#!/bin/sh\nexit 0\n",
		"rg":           "#!/bin/sh\nexit 0\n",
		"sandbox-exec": "#!/bin/sh\nexit 0\n",
	} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	work, home, temp := t.TempDir(), t.TempDir(), t.TempDir()
	command := filepath.Join(work, "agent")
	if err := os.WriteFile(command, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := (&SRTSandbox{Bin: fake, goos: "darwin"}).Command(context.Background(), SandboxSpec{
		Command: []string{command}, WorkDir: work, HomeDir: home, TempDir: temp,
		Environment: []string{"PATH=" + binDir},
		ReadPaths:   []string{work, home, temp, command}, WritePaths: []string{work, home, temp},
	})
	if !errors.Is(err, ErrSandboxUnavailable) || !strings.Contains(err.Error(), "older than 20.11") {
		t.Fatalf("error = %v", err)
	}
}

func TestLookPathInRejectsRelativePATH(t *testing.T) {
	if _, err := lookPathIn("node", "relative:/usr/bin"); err == nil {
		t.Fatal("expected relative PATH entry to fail")
	}
}

func TestSRTSandboxResolvesToolsFromSandboxPATH(t *testing.T) {
	fake := newFakeSRTPackage(t, SRTVersion)
	binDir := t.TempDir()
	if err := os.Symlink(fake, filepath.Join(binDir, "srt")); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"node":         "#!/bin/sh\nprintf 'v25.6.0\\n'\n",
		"bash":         "#!/bin/sh\nexit 0\n",
		"rg":           "#!/bin/sh\nexit 0\n",
		"sandbox-exec": "#!/bin/sh\nexit 0\n",
	} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	work, home, temp := t.TempDir(), t.TempDir(), t.TempDir()
	command := filepath.Join(work, "agent")
	if err := os.WriteFile(command, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd, err := (&SRTSandbox{Bin: "srt", goos: "darwin"}).Command(context.Background(), SandboxSpec{
		Command: []string{command}, WorkDir: work, HomeDir: home, TempDir: temp,
		Environment: []string{"PATH=" + binDir},
		ReadPaths:   []string{work, home, temp, command}, WritePaths: []string{work, home, temp},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Path != filepath.Join(binDir, "srt") {
		t.Fatalf("srt path = %q", cmd.Path)
	}
}
