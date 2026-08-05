package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const (
	// SRTVersion is the tested @anthropic-ai/sandbox-runtime release.
	SRTVersion = "0.0.70"
	// SRTBinEnv selects the pinned srt executable for local runtimes.
	SRTBinEnv  = "SRT_BIN"
	srtPackage = "@anthropic-ai/sandbox-runtime"
)

// SRTSandbox applies Anthropic Sandbox Runtime to one local process tree.
type SRTSandbox struct {
	// Bin points to the srt executable from the pinned npm package.
	Bin string

	goos      string
	lookPath  func(string) (string, error)
	nodeCheck func(func(string) (string, error)) error
	debug     bool
}

// NewSRTSandbox returns an enforcing local process sandbox. Bin defaults to
// "srt" and must resolve to the pinned npm package.
func NewSRTSandbox(bin string) *SRTSandbox {
	return &SRTSandbox{Bin: strings.TrimSpace(bin)}
}

// NewSRTSandboxFromEnv uses SRT_BIN or resolves srt from PATH.
func NewSRTSandboxFromEnv() *SRTSandbox {
	return NewSRTSandbox(os.Getenv(SRTBinEnv))
}

func (s *SRTSandbox) Command(ctx context.Context, spec SandboxSpec) (*exec.Cmd, error) {
	if len(spec.Command) == 0 || strings.TrimSpace(spec.Command[0]) == "" {
		return nil, fmt.Errorf("runtime: sandbox command is required")
	}
	if err := validateSRTSpec(spec); err != nil {
		return nil, err
	}
	goos := s.goos
	if goos == "" {
		goos = runtime.GOOS
	}
	if goos != "darwin" && goos != "linux" {
		return nil, fmt.Errorf("%w: srt does not support %s", ErrSandboxUnavailable, goos)
	}
	if goos == "linux" && len(spec.UnixSockets) > 0 {
		return nil, fmt.Errorf("runtime: srt cannot allow individual Unix sockets on linux")
	}
	lookPath := s.lookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	bin := s.Bin
	if bin == "" {
		bin = "srt"
	}
	resolvedBin, err := lookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("%w: srt executable not found", ErrSandboxUnavailable)
	}
	if err := verifySRTVersion(resolvedBin); err != nil {
		return nil, err
	}
	for _, dependency := range srtDependencies(goos) {
		if _, err := lookPath(dependency); err != nil {
			return nil, fmt.Errorf("%w: srt dependency %s not found", ErrSandboxUnavailable, dependency)
		}
	}
	nodeCheck := s.nodeCheck
	if nodeCheck == nil {
		nodeCheck = checkSRTNodeVersion
	}
	if err := nodeCheck(lookPath); err != nil {
		return nil, err
	}
	settings, err := buildSRTSettings(goos, spec)
	if err != nil {
		return nil, err
	}
	settingsPath := filepath.Join(spec.TempDir, "srt-settings.json")
	settingsJSON, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("runtime: encode srt settings: %w", err)
	}
	if err := os.WriteFile(settingsPath, settingsJSON, 0o600); err != nil {
		return nil, fmt.Errorf("runtime: write srt settings: %w", err)
	}
	args := []string{"--settings", settingsPath}
	if s.debug {
		args = append(args, "--debug")
	}
	args = append(args, "--")
	args = append(args, spec.Command...)
	cmd := exec.CommandContext(ctx, resolvedBin, args...)
	cmd.Dir = spec.WorkDir
	forcedEnvironment := []string{
		"CLAUDE_CODE_TMPDIR=" + spec.TempDir,
		"BUN_TMPDIR=" + spec.TempDir,
	}
	if goos == "darwin" {
		if developerDir, err := filepath.EvalSymlinks("/var/select/developer_dir"); err == nil {
			forcedEnvironment = append(forcedEnvironment, "DEVELOPER_DIR="+developerDir)
		}
	}
	cmd.Env = setSandboxEnvironment(spec.Environment, forcedEnvironment...)
	configureProcessTreeCancellation(cmd)
	return cmd, nil
}

func validateSRTSpec(spec SandboxSpec) error {
	if !filepath.IsAbs(spec.Command[0]) {
		return fmt.Errorf("runtime: sandbox command must be absolute")
	}
	info, err := os.Stat(spec.Command[0])
	if err != nil {
		return fmt.Errorf("runtime: sandbox command is unavailable: %w", err)
	}
	if info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("runtime: sandbox command is not executable")
	}
	for name, path := range map[string]string{
		"work directory": spec.WorkDir,
		"home directory": spec.HomeDir,
		"temp directory": spec.TempDir,
	} {
		if !filepath.IsAbs(path) {
			return fmt.Errorf("runtime: sandbox %s must be absolute", name)
		}
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("runtime: sandbox %s is unavailable: %w", name, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("runtime: sandbox %s is not a directory", name)
		}
	}
	return nil
}

func setSandboxEnvironment(environment []string, forced ...string) []string {
	forcedNames := make(map[string]struct{}, len(forced))
	for _, entry := range forced {
		name, _, _ := strings.Cut(entry, "=")
		forcedNames[name] = struct{}{}
	}
	out := make([]string, 0, len(environment)+len(forced))
	for _, entry := range environment {
		name, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if _, forced := forcedNames[name]; forced {
			continue
		}
		out = append(out, entry)
	}
	return append(out, forced...)
}

func verifySRTVersion(bin string) error {
	resolved, err := filepath.EvalSymlinks(bin)
	if err != nil {
		return fmt.Errorf("%w: resolve srt executable: %v", ErrSandboxUnavailable, err)
	}
	if filepath.Base(resolved) != "cli.js" || filepath.Base(filepath.Dir(resolved)) != "dist" {
		return fmt.Errorf("%w: srt executable is not from the pinned npm package", ErrSandboxUnavailable)
	}
	packageJSON := filepath.Join(filepath.Dir(filepath.Dir(resolved)), "package.json")
	raw, err := os.ReadFile(packageJSON)
	if err != nil {
		return fmt.Errorf("%w: read srt package metadata: %v", ErrSandboxUnavailable, err)
	}
	var metadata struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return fmt.Errorf("%w: decode srt package metadata: %v", ErrSandboxUnavailable, err)
	}
	if metadata.Name != srtPackage || metadata.Version != SRTVersion {
		return fmt.Errorf("%w: srt package is %s@%s, want %s@%s", ErrSandboxUnavailable, metadata.Name, metadata.Version, srtPackage, SRTVersion)
	}
	return nil
}

func srtDependencies(goos string) []string {
	if goos == "linux" {
		return []string{"bwrap", "socat", "rg"}
	}
	return []string{"sandbox-exec", "rg"}
}

func checkSRTNodeVersion(lookPath func(string) (string, error)) error {
	node, err := lookPath("node")
	if err != nil {
		return fmt.Errorf("%w: node executable not found", ErrSandboxUnavailable)
	}
	output, err := exec.Command(node, "--version").Output()
	if err != nil {
		return fmt.Errorf("%w: read node version", ErrSandboxUnavailable)
	}
	version := strings.TrimPrefix(strings.TrimSpace(string(output)), "v")
	parts := strings.Split(version, ".")
	if len(parts) < 2 {
		return fmt.Errorf("%w: unrecognized node version", ErrSandboxUnavailable)
	}
	major, majorErr := strconv.Atoi(parts[0])
	minor, minorErr := strconv.Atoi(parts[1])
	if majorErr != nil || minorErr != nil || major < 20 || major == 20 && minor < 11 {
		return fmt.Errorf("%w: node %s is older than 20.11", ErrSandboxUnavailable, version)
	}
	return nil
}

type srtSettings struct {
	Network                      srtNetworkSettings    `json:"network"`
	Filesystem                   srtFilesystemSettings `json:"filesystem"`
	EnableWeakerNestedSandbox    bool                  `json:"enableWeakerNestedSandbox"`
	EnableWeakerNetworkIsolation bool                  `json:"enableWeakerNetworkIsolation"`
	AllowAppleEvents             bool                  `json:"allowAppleEvents"`
}

type srtNetworkSettings struct {
	AllowedDomains      []string `json:"allowedDomains"`
	DeniedDomains       []string `json:"deniedDomains"`
	AllowUnixSockets    []string `json:"allowUnixSockets"`
	AllowAllUnixSockets bool     `json:"allowAllUnixSockets"`
	AllowLocalBinding   bool     `json:"allowLocalBinding"`
}

type srtFilesystemSettings struct {
	DenyRead   []string `json:"denyRead"`
	AllowRead  []string `json:"allowRead"`
	AllowWrite []string `json:"allowWrite"`
	DenyWrite  []string `json:"denyWrite"`
}

func buildSRTSettings(goos string, spec SandboxSpec) (srtSettings, error) {
	readPaths := append([]string{}, srtSystemReadPaths(goos)...)
	readPaths = append(readPaths, spec.Command[0])
	readPaths = append(readPaths, spec.ReadPaths...)
	readPaths = append(readPaths, spec.WritePaths...)
	allowRead, err := normalizeSandboxPaths(readPaths, true)
	if err != nil {
		return srtSettings{}, fmt.Errorf("runtime: srt read paths: %w", err)
	}
	allowWrite, err := normalizeSandboxPaths(spec.WritePaths, true)
	if err != nil {
		return srtSettings{}, fmt.Errorf("runtime: srt write paths: %w", err)
	}
	denyWrite, err := normalizeSandboxPaths([]string{
		filepath.Join(spec.WorkDir, ".git"),
		"/tmp/claude",
		"/private/tmp/claude",
	}, false)
	if err != nil {
		return srtSettings{}, fmt.Errorf("runtime: srt deny-write paths: %w", err)
	}
	domains := make([]string, 0, len(spec.NetworkDomains))
	for _, domain := range spec.NetworkDomains {
		normalized, err := normalizeSRTDomain(domain)
		if err != nil {
			return srtSettings{}, err
		}
		domains = append(domains, normalized)
	}
	sockets, err := normalizeSandboxPaths(spec.UnixSockets, false)
	if err != nil {
		return srtSettings{}, fmt.Errorf("runtime: srt Unix sockets: %w", err)
	}
	return srtSettings{
		Network: srtNetworkSettings{
			AllowedDomains:      uniqueStrings(domains),
			DeniedDomains:       []string{},
			AllowUnixSockets:    sockets,
			AllowAllUnixSockets: false,
			AllowLocalBinding:   spec.AllowLocalBind,
		},
		Filesystem: srtFilesystemSettings{
			DenyRead:   []string{"/"},
			AllowRead:  allowRead,
			AllowWrite: allowWrite,
			DenyWrite:  denyWrite,
		},
		EnableWeakerNestedSandbox:    false,
		EnableWeakerNetworkIsolation: false,
		AllowAppleEvents:             false,
	}, nil
}

func srtSystemReadPaths(goos string) []string {
	paths := []string{"/bin", "/usr", "/lib", "/lib64", "/dev"}
	if goos == "darwin" {
		paths = append(paths,
			"/System",
			"/opt/homebrew",
			"/usr/local",
			"/etc", "/private/etc",
			"/private/var/db/timezone",
			"/var/select", "/private/var/select",
		)
	} else {
		paths = append(paths,
			"/etc/ld.so.cache", "/etc/ssl", "/etc/ca-certificates",
			"/etc/resolv.conf", "/etc/hosts", "/etc/nsswitch.conf",
			"/etc/passwd", "/etc/group", "/etc/localtime",
		)
	}
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		if _, err := os.Stat(path); err == nil {
			out = append(out, path)
		}
	}
	return out
}

func normalizeSandboxPaths(paths []string, requireExisting bool) ([]string, error) {
	out := make([]string, 0, len(paths)*2)
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		if !filepath.IsAbs(path) {
			return nil, fmt.Errorf("path %q is not absolute", path)
		}
		path = filepath.Clean(path)
		if requireExisting {
			if _, err := os.Stat(path); err != nil {
				return nil, fmt.Errorf("path %q is unavailable: %w", path, err)
			}
		}
		out = append(out, path)
		if resolved, err := filepath.EvalSymlinks(path); err == nil && resolved != path {
			out = append(out, resolved)
		}
	}
	return uniqueStrings(out), nil
}

func normalizeSRTDomain(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" || strings.ContainsAny(value, "/@ \\\t\r\n") || strings.Contains(value, "://") {
		return "", errors.New("runtime: invalid srt network domain")
	}
	host, port, err := splitDomainPort(value)
	if err != nil {
		return "", errors.New("runtime: invalid srt network domain")
	}
	wildcard := strings.HasPrefix(host, "*.")
	if wildcard {
		host = strings.TrimPrefix(host, "*.")
	}
	if net.ParseIP(strings.Trim(host, "[]")) == nil && !validDNSName(host) {
		return "", errors.New("runtime: invalid srt network domain")
	}
	if wildcard {
		host = "*." + host
	}
	if port != "" {
		return host + ":" + port, nil
	}
	return host, nil
}

func splitDomainPort(value string) (string, string, error) {
	if strings.HasPrefix(value, "[") {
		host, port, err := net.SplitHostPort(value)
		if err != nil {
			return "", "", err
		}
		if err := validPort(port); err != nil {
			return "", "", err
		}
		return host, port, nil
	}
	if strings.Count(value, ":") == 0 {
		return value, "", nil
	}
	if strings.Count(value, ":") > 1 {
		if net.ParseIP(value) == nil {
			return "", "", errors.New("invalid host")
		}
		return value, "", nil
	}
	host, port, ok := strings.Cut(value, ":")
	if !ok || host == "" || validPort(port) != nil {
		return "", "", errors.New("invalid port")
	}
	return host, port, nil
}

func validPort(value string) error {
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return errors.New("invalid port")
	}
	return nil
}

func validDNSName(value string) bool {
	if len(value) == 0 || len(value) > 253 {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if r < 'a' || r > 'z' {
				if r < '0' || r > '9' {
					if r != '-' {
						return false
					}
				}
			}
		}
	}
	return true
}

func (s *SRTSandbox) agentTempBase() (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("%w: resolve user cache directory: %v", ErrSandboxUnavailable, err)
	}
	base := filepath.Join(cache, "prow-ai-dashboard", "srt")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", fmt.Errorf("%w: create srt temp base: %v", ErrSandboxUnavailable, err)
	}
	if err := os.Chmod(base, 0o700); err != nil {
		return "", fmt.Errorf("%w: protect srt temp base: %v", ErrSandboxUnavailable, err)
	}
	resolved, err := filepath.EvalSymlinks(base)
	if err != nil {
		return "", fmt.Errorf("%w: resolve srt temp base: %v", ErrSandboxUnavailable, err)
	}
	return resolved, nil
}
