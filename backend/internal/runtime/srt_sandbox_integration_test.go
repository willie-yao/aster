package runtime

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	srtAttackHelperEnv       = "SRT_ATTACK_HELPER"
	srtAttackChildEnv        = "SRT_ATTACK_CHILD"
	srtCancellationHelperEnv = "SRT_CANCELLATION_HELPER"
	srtCancellationChildEnv  = "SRT_CANCELLATION_CHILD"
)

type srtAttackReport struct {
	AllowedRead        bool `json:"allowed_read"`
	AllowedWrite       bool `json:"allowed_write"`
	HostReadDenied     bool `json:"host_read_denied"`
	HostEnvAbsent      bool `json:"host_env_absent"`
	OutsideWriteDenied bool `json:"outside_write_denied"`
	GitWriteDenied     bool `json:"git_write_denied"`
	NetworkDenied      bool `json:"network_denied"`
	UnixSocketDenied   bool `json:"unix_socket_denied"`
	LocalBindDenied    bool `json:"local_bind_denied"`
	SymlinkWriteDenied bool `json:"symlink_write_denied"`
}

func TestSRTSandboxHostileIntegration(t *testing.T) {
	bin := strings.TrimSpace(os.Getenv("SRT_TEST_BIN"))
	if bin == "" {
		t.Skip("set SRT_TEST_BIN to the pinned srt executable")
	}
	root := newSRTIntegrationRoot(t)
	work := filepath.Join(root, "work")
	home := filepath.Join(root, "home")
	temp := filepath.Join(root, "tmp")
	outside := filepath.Join(root, "outside")
	for _, path := range []string{work, home, temp, outside, filepath.Join(work, ".git")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(work, "allowed.txt"), []byte("allowed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hostSecret := filepath.Join(outside, "host-secret.txt")
	if err := os.WriteFile(hostSecret, []byte("disposable-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(work, "escape")); err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(outside, "host.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	executable := copyTestExecutable(t, work)
	reportPath := filepath.Join(work, "report.json")
	childReportPath := filepath.Join(work, "child-report.json")
	t.Setenv("HOST_SECRET_ENV", "host-environment-secret")
	env := srtIntegrationEnv(home, temp,
		srtAttackHelperEnv+"=1",
		"SRT_ATTACK_WORK="+work,
		"SRT_ATTACK_OUTSIDE="+outside,
		"SRT_ATTACK_HOST_FILE="+hostSecret,
		"SRT_ATTACK_SOCKET="+socketPath,
		"SRT_ATTACK_REPORT="+reportPath,
		"SRT_ATTACK_CHILD_REPORT="+childReportPath,
	)
	output, err := NewSRTSandbox(bin).Run(context.Background(), SandboxSpec{
		Command: []string{executable, "-test.run=^TestSRTSandboxAttackHelper$"},
		WorkDir: work, HomeDir: home, TempDir: temp, Environment: env,
		ReadPaths: []string{work, home, temp, executable}, WritePaths: []string{work, home, temp},
	})
	if err != nil {
		t.Fatalf("sandbox helper: %v: %s", err, tail(string(output), 4096))
	}
	for _, path := range []string{reportPath, childReportPath} {
		report := readSRTAttackReport(t, path)
		if !report.AllowedRead || !report.AllowedWrite || !report.HostReadDenied || !report.HostEnvAbsent ||
			!report.OutsideWriteDenied || !report.GitWriteDenied || !report.NetworkDenied ||
			!report.UnixSocketDenied || !report.LocalBindDenied || !report.SymlinkWriteDenied {
			t.Fatalf("attack report %s = %+v", filepath.Base(path), report)
		}
	}
	for _, path := range []string{filepath.Join(outside, "outside-write"), filepath.Join(outside, "symlink-write")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("sandbox escape created %s", path)
		}
	}
}

func TestSRTSandboxAttackHelper(t *testing.T) {
	if os.Getenv(srtAttackHelperEnv) != "1" {
		return
	}
	report := runSRTAttacks()
	writeSRTAttackReport(t, os.Getenv("SRT_ATTACK_REPORT"), report)
	if os.Getenv(srtAttackChildEnv) == "1" {
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestSRTSandboxAttackHelper$")
	cmd.Env = append(os.Environ(),
		srtAttackChildEnv+"=1",
		"SRT_ATTACK_REPORT="+os.Getenv("SRT_ATTACK_CHILD_REPORT"),
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child helper: %v: %s", err, tail(string(output), 2048))
	}
}

func runSRTAttacks() srtAttackReport {
	work := os.Getenv("SRT_ATTACK_WORK")
	outside := os.Getenv("SRT_ATTACK_OUTSIDE")
	allowed, allowedReadErr := os.ReadFile(filepath.Join(work, "allowed.txt"))
	allowedWriteErr := os.WriteFile(filepath.Join(work, "allowed-write"), []byte("ok"), 0o600)
	_, hostReadErr := os.ReadFile(os.Getenv("SRT_ATTACK_HOST_FILE"))
	outsideWriteErr := os.WriteFile(filepath.Join(outside, "outside-write"), []byte("bad"), 0o600)
	gitWriteErr := os.WriteFile(filepath.Join(work, ".git", "config"), []byte("bad"), 0o600)
	network, networkErr := net.DialTimeout("tcp", "example.com:80", 750*time.Millisecond)
	if network != nil {
		_ = network.Close()
	}
	unixSocket, unixSocketErr := net.DialTimeout("unix", os.Getenv("SRT_ATTACK_SOCKET"), 750*time.Millisecond)
	if unixSocket != nil {
		_ = unixSocket.Close()
	}
	local, localErr := net.Listen("tcp", "127.0.0.1:0")
	if local != nil {
		_ = local.Close()
	}
	symlinkWriteErr := os.WriteFile(filepath.Join(work, "escape", "symlink-write"), []byte("bad"), 0o600)
	return srtAttackReport{
		AllowedRead:        allowedReadErr == nil && string(allowed) == "allowed\n",
		AllowedWrite:       allowedWriteErr == nil,
		HostReadDenied:     hostReadErr != nil,
		HostEnvAbsent:      os.Getenv("HOST_SECRET_ENV") == "",
		OutsideWriteDenied: outsideWriteErr != nil,
		GitWriteDenied:     gitWriteErr != nil,
		NetworkDenied:      networkErr != nil,
		UnixSocketDenied:   unixSocketErr != nil,
		LocalBindDenied:    localErr != nil,
		SymlinkWriteDenied: symlinkWriteErr != nil,
	}
}

func TestSRTSandboxCancellationIntegration(t *testing.T) {
	bin := strings.TrimSpace(os.Getenv("SRT_TEST_BIN"))
	if bin == "" {
		t.Skip("set SRT_TEST_BIN to the pinned srt executable")
	}
	root := newSRTIntegrationRoot(t)
	work := filepath.Join(root, "work")
	home := filepath.Join(root, "home")
	temp := filepath.Join(root, "tmp")
	for _, path := range []string{work, home, temp} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	executable := copyTestExecutable(t, work)
	ready := filepath.Join(work, "ready")
	sessionReady := filepath.Join(work, "session-ready")
	marker := filepath.Join(work, "child-survived")
	ctx, cancel := context.WithCancel(context.Background())
	env := srtIntegrationEnv(home, temp,
		srtCancellationHelperEnv+"=1",
		"SRT_CANCEL_READY="+ready,
		"SRT_CANCEL_SESSION_READY="+sessionReady,
		"SRT_CANCEL_MARKER="+marker,
	)
	type runResult struct {
		output []byte
		err    error
	}
	done := make(chan runResult, 1)
	go func() {
		output, err := NewSRTSandbox(bin).Run(ctx, SandboxSpec{
			Command: []string{executable, "-test.run=^TestSRTSandboxCancellationHelper$"},
			WorkDir: work, HomeDir: home, TempDir: temp, Environment: env,
			ReadPaths: []string{work, home, temp, executable}, WritePaths: []string{work, home, temp},
		})
		done <- runResult{output: output, err: err}
	}()
	waitForPath(t, ready, 5*time.Second)
	cancel()
	result := <-done
	if result.err == nil {
		t.Fatal("cancelled sandbox run returned no error")
	}
	time.Sleep(1200 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("sandboxed child survived cancellation")
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestSRTSandboxCancellationHelper(t *testing.T) {
	if os.Getenv(srtCancellationHelperEnv) != "1" {
		return
	}
	if os.Getenv(srtCancellationChildEnv) == "1" {
		if _, err := syscall.Setsid(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(os.Getenv("SRT_CANCEL_SESSION_READY"), []byte("ready"), 0o600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(800 * time.Millisecond)
		if err := os.WriteFile(os.Getenv("SRT_CANCEL_MARKER"), []byte("survived"), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestSRTSandboxCancellationHelper$")
	cmd.Env = append(os.Environ(), srtCancellationChildEnv+"=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForPath(t, os.Getenv("SRT_CANCEL_SESSION_READY"), 5*time.Second)
	if err := os.WriteFile(os.Getenv("SRT_CANCEL_READY"), []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
}

func copyTestExecutable(t *testing.T, dir string) string {
	t.Helper()
	source, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "runtime-test-helper")
	if err := os.WriteFile(target, raw, 0o700); err != nil {
		t.Fatal(err)
	}
	return target
}

func newSRTIntegrationRoot(t *testing.T) string {
	t.Helper()
	base, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	base = filepath.Join(base, "prow-ai-dashboard", "srt-tests")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(base, "run-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	return root
}

func srtIntegrationEnv(home, temp string, extra ...string) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"TMPDIR=" + temp,
		"TMP=" + temp,
		"TEMP=" + temp,
	}
	for _, name := range []string{"LANG", "LC_ALL", "LC_CTYPE", "SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS"} {
		if value := os.Getenv(name); value != "" {
			env = append(env, name+"="+value)
		}
	}
	return append(env, extra...)
}

func readSRTAttackReport(t *testing.T, path string) srtAttackReport {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var report srtAttackReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	return report
}

func writeSRTAttackReport(t *testing.T, path string, report srtAttackReport) {
	t.Helper()
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func waitForPath(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSRTSandboxRealOpenCode(t *testing.T) {
	if os.Getenv("SRT_REAL_OPENCODE") != "1" {
		t.Skip("set SRT_REAL_OPENCODE=1 for the disposable Copilot smoke test")
	}
	srtBin := strings.TrimSpace(os.Getenv("SRT_TEST_BIN"))
	if srtBin == "" {
		t.Fatal("SRT_TEST_BIN is required")
	}
	opencodeBin, err := exec.LookPath("opencode")
	if err != nil {
		t.Fatal("opencode is required")
	}
	model := strings.TrimSpace(os.Getenv("SRT_TEST_OPENCODE_MODEL"))
	if model == "" {
		model = "github-copilot/claude-sonnet-4.6"
	}
	domains := splitCommaList(os.Getenv("SRT_TEST_NETWORK_DOMAINS"))
	runtime := &LocalAgentRuntime{Bin: opencodeBin, Sandbox: &SRTSandbox{Bin: srtBin, debug: true}}

	t.Run("no bash one-file generation", func(t *testing.T) {
		repo := initRepo(t)
		result, err := runtime.Generate(context.Background(), GenerateSpec{
			Repo:        RepoRef{Owner: "sandbox", Name: "prompt", Ref: "main", CloneURL: repo},
			Instruction: "Create generated.txt containing exactly sandboxed followed by a newline. Do not change any other file.",
			NativeModel: model, UseAmbientAuth: true, AllowBash: false,
			NetworkDomains: domains, Timeout: 3 * time.Minute,
		})
		if err != nil {
			t.Fatalf("Generate: %v; sandbox domains: %v", err, extractSandboxDomains(result.Output))
		}
		if len(result.Files) != 1 || result.Files["generated.txt"] != "sandboxed\n" {
			t.Fatalf("unexpected result files %v; sandbox domains: %v; signals: %v; paths: %v; denied: %v; events: %v", mapKeys(result.Files), extractSandboxDomains(result.Output), sandboxOutputSignals(result.Output), extractSandboxPaths(result.Output), sandboxDeniedLines(result.Output), opencodeEventSummary(result.Output))
		}
	})

	t.Run("bash-enabled fix and test", func(t *testing.T) {
		repo := initRepo(t)
		if err := os.WriteFile(filepath.Join(repo, "check.sh"), []byte("#!/bin/sh\ntest \"$(cat orig.txt)\" = fixed\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		gitRunForTest(t, repo, "add", "check.sh")
		gitRunForTest(t, repo, "commit", "-m", "add check")
		result, err := runtime.Generate(context.Background(), GenerateSpec{
			Repo:        RepoRef{Owner: "sandbox", Name: "fix", Ref: "main", CloneURL: repo},
			Instruction: "Change orig.txt to contain exactly fixed followed by a newline. Use Bash to run ./check.sh after editing. Do not change any other file.",
			NativeModel: model, UseAmbientAuth: true, AllowBash: true,
			NetworkDomains: domains, Timeout: 3 * time.Minute,
		})
		if err != nil {
			t.Fatalf("Generate: %v; sandbox domains: %v", err, extractSandboxDomains(result.Output))
		}
		if len(result.Files) != 1 || result.Files["orig.txt"] != "fixed\n" {
			t.Fatalf("unexpected result files %v; sandbox domains: %v; signals: %v; paths: %v; denied: %v; events: %v", mapKeys(result.Files), extractSandboxDomains(result.Output), sandboxOutputSignals(result.Output), extractSandboxPaths(result.Output), sandboxDeniedLines(result.Output), opencodeEventSummary(result.Output))
		}
	})
}

func splitCommaList(value string) []string {
	var out []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func extractSandboxDomains(output string) []string {
	var domains []string
	for _, line := range strings.Split(output, "\n") {
		lower := strings.ToLower(line)
		if !strings.Contains(lower, "denying:") && !strings.Contains(lower, "blocked to") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		candidate := strings.Trim(fields[len(fields)-1], "[](),;\"")
		if normalized, err := normalizeSRTDomain(candidate); err == nil {
			domains = append(domains, normalized)
		}
	}
	return uniqueStrings(domains)
}

func opencodeEventSummary(output string) []string {
	var summary []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(line), &event) == nil {
			typeName, _ := event["type"].(string)
			item := typeName
			if failure, ok := event["error"].(map[string]any); ok {
				if name, ok := failure["name"].(string); ok {
					item += ":" + name
				}
			}
			if item != "" {
				summary = append(summary, item)
			}
			continue
		}
		lower := strings.ToLower(line)
		for _, signal := range []string{"migration", "operation not permitted", "permission denied", "connection blocked", "no matching config rule"} {
			if strings.Contains(lower, signal) {
				summary = append(summary, signal)
			}
		}
	}
	return uniqueStrings(summary)
}

func sandboxDeniedLines(output string) []string {
	var lines []string
	for _, line := range strings.Split(output, "\n") {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "operation not permitted") || strings.Contains(lower, "permission denied") {
			if len(line) > 500 {
				line = line[:500]
			}
			lines = append(lines, line)
		}
	}
	return lines
}

func sandboxOutputSignals(output string) []string {
	lower := strings.ToLower(output)
	var signals []string
	for _, signal := range []string{
		"operation not permitted", "permission denied", "sandbox", "network", "connect",
		"fetch", "install", "provider", "authentication", "unauthorized", "forbidden",
		"enoent", "eacces", "eperm", "error",
	} {
		if strings.Contains(lower, signal) {
			signals = append(signals, signal)
		}
	}
	return signals
}

func extractSandboxPaths(output string) []string {
	fields := strings.FieldsFunc(output, func(r rune) bool {
		return r != '/' && r != '.' && r != '-' && r != '_' &&
			(r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9')
	})
	var paths []string
	for _, field := range fields {
		if strings.HasPrefix(field, "/") && len(field) > 1 {
			paths = append(paths, field)
		}
	}
	return uniqueStrings(paths)
}

func mapKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func gitRunForTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", args[0], err, output)
	}
}
