// Package k8s implements tier-2 agent tools that encode Kubernetes-shaped
// navigation primitives on top of the universal artifact Browser. These tools
// expose cluster discovery, per-machine logs, and controller logs as lazy calls
// so the agent only pays for what it needs.
//
// The tools live behind the same Browser interface as the filesystem tools,
// so they remain GCS-implementation-agnostic and can be tested against
// fakes. The discovery helpers are stateless pure functions over a Browser. The
// cluster-to-test matcher is provider-agnostic with a CAPZ-flavored fallback so
// existing dashboards keep working without per-project rule tables.
package k8s

import (
	"context"
	"regexp"
	"strings"

	"github.com/willie-yao/aster/backend/internal/artifacts"
)

// Group is the alias used in config to enable all k8s tools at once.
const Group = "k8s"

// clustersRoot is the relative path under a build root where CAPI-shaped
// projects emit per-cluster artifact dumps. The empty + missing case is
// handled by tools returning {"clusters": []} rather than an error so
// non-K8s builds degrade cleanly.
const clustersRoot = "artifacts/clusters/"

// bootstrapDir is the management cluster's artifact dir. It holds controller
// manager logs and is not a workload cluster. discover_clusters filters it out;
// discover_controllers surfaces it.
const bootstrapDir = "bootstrap"

// knownMachineLogs lists the per-machine log files we look for inside each
// machine directory, in priority order. Ported verbatim from
// collectors/capi/discover.go.
var knownMachineLogs = []string{
	"boot.log",
	"cloud-init-output.log",
	"cloud-init.log",
	"kubelet.log",
	"kube-apiserver.log",
	"journal.log",
	"kern.log",
	"containerd.log",
}

// Cluster is a discovered workload-cluster artifact dump. Path is relative
// to the build root and trailing-slashed so callers can compose deeper
// paths directly.
type Cluster struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// Machine is a per-VM artifact dump under a cluster.
type Machine struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// Controller represents one controller deployment under a namespace under
// the bootstrap management cluster's log tree.
type Controller struct {
	Namespace  string `json:"namespace"`
	Deployment string `json:"deployment"`
	Path       string `json:"path"` // bootstrap/logs/<ns>/<deployment>/ trailing-slashed
}

// LogFile is a single file resolved under a machine or controller dir.
type LogFile struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Size int64  `json:"size,omitempty"`
}

// DiscoverClusters lists workload-cluster subdirectories under
// artifacts/clusters/. The "bootstrap" dir is excluded because it holds
// management controller logs, not workload state. Returns an empty slice when
// the artifacts/clusters/ tree is missing, so callers can treat absence as "not
// a CAPI-shaped build" without special-casing 404.
func DiscoverClusters(ctx context.Context, b artifacts.Browser) ([]Cluster, error) {
	listing, err := b.List(ctx, clustersRoot)
	if err != nil {
		// Missing tree is the dominant "error" here; treat it as empty. Real
		// failures will recur on the next tool call and surface there.
		return nil, nil
	}
	var out []Cluster
	for _, d := range listing.Dirs {
		name := trimSlash(d)
		if name == "" || strings.EqualFold(name, bootstrapDir) {
			continue
		}
		out = append(out, Cluster{
			Name: name,
			Path: clustersRoot + name + "/",
		})
	}
	return out, nil
}

// ListClusterMachines lists machine subdirectories under
// artifacts/clusters/<cluster>/machines/. Returns an empty slice when the
// dir is missing.
func ListClusterMachines(ctx context.Context, b artifacts.Browser, cluster string) ([]Machine, error) {
	path := clustersRoot + cluster + "/machines/"
	listing, err := b.List(ctx, path)
	if err != nil {
		return nil, nil
	}
	var out []Machine
	for _, d := range listing.Dirs {
		name := trimSlash(d)
		if name == "" {
			continue
		}
		out = append(out, Machine{
			Name: name,
			Path: path + name + "/",
		})
	}
	return out, nil
}

// ListMachineLogs lists actual log files present in a machine directory,
// filtered to the knownMachineLogs set and ordered by knownMachineLogs
// priority. Returns the set of files actually present so callers don't
// invite the agent to fetch 404s.
func ListMachineLogs(ctx context.Context, b artifacts.Browser, cluster, machine string) ([]LogFile, error) {
	dir := clustersRoot + cluster + "/machines/" + machine + "/"
	listing, err := b.List(ctx, dir)
	if err != nil {
		return nil, nil
	}
	present := make(map[string]artifacts.FileInfo, len(listing.Files))
	for _, f := range listing.Files {
		present[f.Name] = f
	}
	var out []LogFile
	for _, name := range knownMachineLogs {
		info, ok := present[name]
		if !ok {
			continue
		}
		out = append(out, LogFile{
			Name: name,
			Path: dir + name,
			Size: info.Size,
		})
	}
	return out, nil
}

// DiscoverControllers walks artifacts/clusters/bootstrap/logs/ and returns
// every <namespace>/<deployment> tuple present. If namespace is non-empty,
// only that namespace is walked. Selectors that don't match anything return
// an empty slice, not an error.
func DiscoverControllers(ctx context.Context, b artifacts.Browser, namespace string) ([]Controller, error) {
	base := clustersRoot + bootstrapDir + "/logs/"
	var namespaces []string
	if namespace != "" {
		namespaces = []string{namespace}
	} else {
		listing, err := b.List(ctx, base)
		if err != nil {
			return nil, nil
		}
		for _, d := range listing.Dirs {
			if n := trimSlash(d); n != "" {
				namespaces = append(namespaces, n)
			}
		}
	}
	var out []Controller
	for _, ns := range namespaces {
		nsPath := base + ns + "/"
		listing, err := b.List(ctx, nsPath)
		if err != nil {
			continue
		}
		for _, d := range listing.Dirs {
			dep := trimSlash(d)
			if dep == "" {
				continue
			}
			out = append(out, Controller{
				Namespace:  ns,
				Deployment: dep,
				Path:       nsPath + dep + "/",
			})
		}
	}
	return out, nil
}

// ResolveControllerLog drills into a controller dir to find a concrete
// container-log file path. Walks
// artifacts/clusters/bootstrap/logs/<ns>/<deployment>/<pod>/ for the first
// pod whose name matches podNameRe and returns the path to the named container
// log plus the pod name. nil podNameRe matches any pod; empty containerLog
// defaults to "manager.log".
// Returns nil when nothing matches.
func ResolveControllerLog(ctx context.Context, b artifacts.Browser, namespace, deployment string, podNameRe *regexp.Regexp, containerLog string) (*LogFile, string, error) {
	if containerLog == "" {
		containerLog = "manager.log"
	}
	depPath := clustersRoot + bootstrapDir + "/logs/" + namespace + "/" + deployment + "/"
	listing, err := b.List(ctx, depPath)
	if err != nil {
		return nil, "", nil
	}
	for _, d := range listing.Dirs {
		pod := trimSlash(d)
		if pod == "" {
			continue
		}
		if podNameRe != nil && !podNameRe.MatchString(pod) {
			continue
		}
		podPath := depPath + pod + "/"
		podListing, err := b.List(ctx, podPath)
		if err != nil {
			continue
		}
		for _, f := range podListing.Files {
			if f.Name == containerLog {
				return &LogFile{
					Name: containerLog,
					Path: podPath + containerLog,
					Size: f.Size,
				}, pod, nil
			}
		}
	}
	return nil, "", nil
}

// trimSlash strips a single trailing slash if present. Browser.List dirs are
// trailing-slashed by convention.
func trimSlash(s string) string {
	return strings.TrimSuffix(s, "/")
}
