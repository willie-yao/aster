package k8s

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
)

// fakeBrowser is an in-memory artifacts.Browser shaped like a CAPI-style build
// tree. dirs maps parent paths to child subdirs; files maps full paths to bytes.
type fakeBrowser struct {
	dirs  map[string][]string
	files map[string][]byte
}

func (b *fakeBrowser) BuildRoot() string { return "fake/build/1" }

func (b *fakeBrowser) ListTree(_ context.Context, maxPaths int) ([]string, bool, error) {
	var out []string
	for name := range b.files {
		if len(out) >= maxPaths {
			return out, true, nil
		}
		out = append(out, name)
	}
	return out, false, nil
}

func (b *fakeBrowser) List(_ context.Context, dir string) (*artifacts.Listing, error) {
	prefix := dir
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	subdirs, hasDir := b.dirs[prefix]
	var files []artifacts.FileInfo
	for name, data := range b.files {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		rest := strings.TrimPrefix(name, prefix)
		if strings.Contains(rest, "/") {
			continue
		}
		files = append(files, artifacts.FileInfo{Name: rest, Size: int64(len(data))})
	}
	if !hasDir && len(files) == 0 {
		return nil, fmt.Errorf("not found: %s", dir)
	}
	return &artifacts.Listing{Dir: prefix, Dirs: subdirs, Files: files}, nil
}

func (b *fakeBrowser) Read(_ context.Context, p string, _, _ int) ([]byte, int64, error) {
	data, ok := b.files[p]
	if !ok {
		return nil, 0, fmt.Errorf("not found: %s", p)
	}
	return data, int64(len(data)), nil
}

func (b *fakeBrowser) Tail(_ context.Context, p string, _, _ int) (*artifacts.TailResult, error) {
	data, ok := b.files[p]
	if !ok {
		return nil, fmt.Errorf("not found: %s", p)
	}
	return &artifacts.TailResult{FileSize: int64(len(data)), Content: data}, nil
}

func (b *fakeBrowser) Grep(_ context.Context, p string, _ *regexp.Regexp, _, _, _ int) (*artifacts.GrepResult, error) {
	data, ok := b.files[p]
	if !ok {
		return nil, fmt.Errorf("not found: %s", p)
	}
	return &artifacts.GrepResult{FileSize: int64(len(data))}, nil
}

// capiShapedBrowser models the same artifact layout as
// collectors/capi/testdata: two workload clusters (ha, ipv6) under
// artifacts/clusters/ plus a bootstrap dir with capz-system controller
// logs. Used for both discovery and parity tests.
func capiShapedBrowser() *fakeBrowser {
	return &fakeBrowser{
		dirs: map[string][]string{
			"artifacts/clusters/":                                                    {"bootstrap/", "capz-e2e-abc123-ha/", "capz-e2e-abc123-ipv6/"},
			"artifacts/clusters/capz-e2e-abc123-ha/":                                 {"azure-activity-logs/", "machines/", "kube-system/", "calico-system/"},
			"artifacts/clusters/capz-e2e-abc123-ha/machines/":                        {"capz-e2e-abc123-ha-control-plane-jkl42/", "capz-e2e-abc123-ha-md-0-xyz98/"},
			"artifacts/clusters/capz-e2e-abc123-ipv6/":                               {"machines/"},
			"artifacts/clusters/capz-e2e-abc123-ipv6/machines/":                      {"capz-e2e-abc123-ipv6-control-plane-aa1/"},
			"artifacts/clusters/bootstrap/":                                          {"logs/"},
			"artifacts/clusters/bootstrap/logs/":                                     {"capz-system/", "capi-system/"},
			"artifacts/clusters/bootstrap/logs/capz-system/":                         {"capz-controller-manager/", "azureserviceoperator-controller-manager/"},
			"artifacts/clusters/bootstrap/logs/capz-system/capz-controller-manager/": {"capz-controller-manager-7d4f5b9c4-xyz12/"},
			"artifacts/clusters/bootstrap/logs/capi-system/":                         {"capi-controller-manager/"},
			"artifacts/clusters/bootstrap/logs/capi-system/capi-controller-manager/": {"capi-controller-manager-5f6c8d9b7-abc34/"},
		},
		files: map[string][]byte{
			"artifacts/clusters/capz-e2e-abc123-ha/machines/capz-e2e-abc123-ha-control-plane-jkl42/boot.log":    []byte("boot output"),
			"artifacts/clusters/capz-e2e-abc123-ha/machines/capz-e2e-abc123-ha-control-plane-jkl42/kubelet.log": []byte("kubelet output"),
			"artifacts/clusters/capz-e2e-abc123-ha/machines/capz-e2e-abc123-ha-control-plane-jkl42/journal.log": []byte("journal output"),
			// cloud-init.log is "present but empty" to verify discovery doesn't filter on size.
			"artifacts/clusters/capz-e2e-abc123-ha/machines/capz-e2e-abc123-ha-control-plane-jkl42/cloud-init.log":                      []byte{},
			"artifacts/clusters/capz-e2e-abc123-ha/machines/capz-e2e-abc123-ha-md-0-xyz98/boot.log":                                     []byte("worker boot"),
			"artifacts/clusters/bootstrap/logs/capz-system/capz-controller-manager/capz-controller-manager-7d4f5b9c4-xyz12/manager.log": []byte("capz manager log"),
			"artifacts/clusters/bootstrap/logs/capi-system/capi-controller-manager/capi-controller-manager-5f6c8d9b7-abc34/manager.log": []byte("capi manager log"),
		},
	}
}

func TestDiscoverClustersExcludesBootstrap(t *testing.T) {
	b := capiShapedBrowser()
	clusters, err := DiscoverClusters(context.Background(), b)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(clusters) != 2 {
		t.Fatalf("expected 2 clusters (bootstrap excluded), got %d: %v", len(clusters), clusters)
	}
	want := map[string]string{
		"capz-e2e-abc123-ha":   "artifacts/clusters/capz-e2e-abc123-ha/",
		"capz-e2e-abc123-ipv6": "artifacts/clusters/capz-e2e-abc123-ipv6/",
	}
	for _, c := range clusters {
		if c.Name == bootstrapDir {
			t.Errorf("bootstrap leaked into cluster list")
		}
		gotPath, ok := want[c.Name]
		if !ok {
			t.Errorf("unexpected cluster %q", c.Name)
			continue
		}
		if c.Path != gotPath {
			t.Errorf("cluster %q path = %q, want %q", c.Name, c.Path, gotPath)
		}
		delete(want, c.Name)
	}
	if len(want) != 0 {
		t.Errorf("missing clusters: %v", want)
	}
}

func TestDiscoverClustersMissingTreeReturnsEmpty(t *testing.T) {
	b := &fakeBrowser{} // List returns error for everything
	clusters, err := DiscoverClusters(context.Background(), b)
	if err != nil {
		t.Fatalf("missing tree should not error, got %v", err)
	}
	if len(clusters) != 0 {
		t.Errorf("expected empty slice, got %v", clusters)
	}
}

func TestListClusterMachines(t *testing.T) {
	b := capiShapedBrowser()
	machines, err := ListClusterMachines(context.Background(), b, "capz-e2e-abc123-ha")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(machines) != 2 {
		t.Fatalf("expected 2 machines, got %d", len(machines))
	}
	wantPath := "artifacts/clusters/capz-e2e-abc123-ha/machines/capz-e2e-abc123-ha-control-plane-jkl42/"
	if machines[0].Name != "capz-e2e-abc123-ha-control-plane-jkl42" || machines[0].Path != wantPath {
		t.Errorf("unexpected first machine: %+v", machines[0])
	}
}

func TestListClusterMachinesMissingClusterReturnsEmpty(t *testing.T) {
	b := capiShapedBrowser()
	machines, err := ListClusterMachines(context.Background(), b, "no-such-cluster")
	if err != nil {
		t.Fatalf("missing cluster should not error, got %v", err)
	}
	if len(machines) != 0 {
		t.Errorf("expected empty slice, got %v", machines)
	}
}

func TestListMachineLogsFiltersAndOrders(t *testing.T) {
	b := capiShapedBrowser()
	logs, err := ListMachineLogs(context.Background(), b, "capz-e2e-abc123-ha", "capz-e2e-abc123-ha-control-plane-jkl42")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Output is ordered by knownMachineLogs priority and filtered to present files.
	wantNames := []string{"boot.log", "cloud-init.log", "kubelet.log", "journal.log"}
	if len(logs) != len(wantNames) {
		t.Fatalf("expected %d logs, got %d: %+v", len(wantNames), len(logs), logs)
	}
	for i, want := range wantNames {
		if logs[i].Name != want {
			t.Errorf("log[%d] = %q, want %q (full=%v)", i, logs[i].Name, want, logs)
		}
	}
	// Zero-byte cloud-init.log should keep size 0.
	if logs[1].Size != 0 {
		t.Errorf("cloud-init.log size = %d, want 0", logs[1].Size)
	}
	if logs[0].Size != int64(len("boot output")) {
		t.Errorf("boot.log size = %d, want %d", logs[0].Size, len("boot output"))
	}
}

func TestDiscoverControllersAllNamespaces(t *testing.T) {
	b := capiShapedBrowser()
	controllers, err := DiscoverControllers(context.Background(), b, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(controllers) != 3 {
		t.Fatalf("expected 3 controllers, got %d: %+v", len(controllers), controllers)
	}
	got := map[string]bool{}
	for _, c := range controllers {
		got[c.Namespace+"/"+c.Deployment] = true
	}
	for _, want := range []string{
		"capz-system/capz-controller-manager",
		"capz-system/azureserviceoperator-controller-manager",
		"capi-system/capi-controller-manager",
	} {
		if !got[want] {
			t.Errorf("missing controller: %s", want)
		}
	}
}

func TestDiscoverControllersNamespaceFilter(t *testing.T) {
	b := capiShapedBrowser()
	controllers, err := DiscoverControllers(context.Background(), b, "capz-system")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(controllers) != 2 {
		t.Fatalf("expected 2 controllers in capz-system, got %d: %+v", len(controllers), controllers)
	}
	for _, c := range controllers {
		if c.Namespace != "capz-system" {
			t.Errorf("controller %s in namespace %q leaked through filter", c.Deployment, c.Namespace)
		}
	}
}

func TestDiscoverControllersMissingBootstrapReturnsEmpty(t *testing.T) {
	b := &fakeBrowser{}
	got, err := DiscoverControllers(context.Background(), b, "")
	if err != nil {
		t.Fatalf("missing bootstrap should not error, got %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty slice, got %v", got)
	}
}

func TestResolveControllerLogDefaultContainerLog(t *testing.T) {
	b := capiShapedBrowser()
	log, pod, err := ResolveControllerLog(context.Background(), b, "capz-system", "capz-controller-manager", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if log == nil {
		t.Fatal("expected a log match, got nil")
	}
	if pod != "capz-controller-manager-7d4f5b9c4-xyz12" {
		t.Errorf("pod = %q, want capz-controller-manager-7d4f5b9c4-xyz12", pod)
	}
	wantPath := "artifacts/clusters/bootstrap/logs/capz-system/capz-controller-manager/capz-controller-manager-7d4f5b9c4-xyz12/manager.log"
	if log.Path != wantPath {
		t.Errorf("log.Path = %q, want %q", log.Path, wantPath)
	}
	if log.Name != "manager.log" {
		t.Errorf("log.Name = %q, want manager.log (default)", log.Name)
	}
}

func TestResolveControllerLogPodNameRegexNoMatch(t *testing.T) {
	b := capiShapedBrowser()
	re := regexp.MustCompile("^no-match-")
	log, pod, err := ResolveControllerLog(context.Background(), b, "capz-system", "capz-controller-manager", re, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if log != nil || pod != "" {
		t.Errorf("expected no match, got log=%v pod=%q", log, pod)
	}
}

func TestResolveControllerLogMissingDeploymentReturnsNil(t *testing.T) {
	b := &fakeBrowser{}
	log, _, err := ResolveControllerLog(context.Background(), b, "capz-system", "no-such", nil, "")
	if err != nil {
		t.Fatalf("missing deployment should not error, got %v", err)
	}
	if log != nil {
		t.Errorf("expected nil log, got %+v", log)
	}
}
