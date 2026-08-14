// Package kubernetesdeploy validates and installs Kubernetes consumer bundles.
package kubernetesdeploy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/modelprovider"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/onboard"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"gopkg.in/yaml.v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
)

const (
	supportedAgentSandboxVersion          = "v0.5.3"
	supportedAgentSandboxControllerImage  = "registry.k8s.io/agent-sandbox/agent-sandbox-controller:v0.5.3"
	supportedAgentSandboxControllerDigest = "sha256:ba381b4e0c86cca597d5c5a31860e38d30ec1c45e0a7a8328bb2799c87d059c0"
	supportedAgentSandboxAMD64Digest      = "sha256:f7fccf31493b7568270916c76ab8960de17ede644feb670b93e3702b7056e0fd"
	supportedAgentSandboxARM64Digest      = "sha256:ab1e3cdb72e5c03626502abe79aed475dbef49df95357f7217a36d10a39211fc"
	agentSandboxSystemNamespace           = "agent-sandbox-system"
	agentSandboxControllerName            = "agent-sandbox-controller"
	agentSandboxWebhookServiceName        = "agent-sandbox-webhook-service"
	modelGatewayTLSSecretAnnotation       = "prow-ai-dashboard/model-gateway-tls-secret"
)

var (
	sha256DigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	shaTagPattern       = regexp.MustCompile(`^sha-[0-9a-f]{7,64}$`)
	semverTagPattern    = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$`)
	publicHostPattern   = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*\.[a-z](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
)

// KubernetesDoctorOptions selects the configured release to inspect.
type KubernetesDoctorOptions struct {
	Action       string
	ProjectDir   string
	ValuesFile   string
	Release      string
	Namespace    string
	KubeContext  string
	Chart        string
	ChartVersion string
}

// KubernetesDoctorStatus is the outcome of one live deployment check.
type KubernetesDoctorStatus string

const (
	KubernetesDoctorPass       KubernetesDoctorStatus = "pass"
	KubernetesDoctorWarn       KubernetesDoctorStatus = "warn"
	KubernetesDoctorFail       KubernetesDoctorStatus = "fail"
	KubernetesDoctorUnverified KubernetesDoctorStatus = "unverified"
)

// KubernetesDoctorCheck is one bounded operator-facing result.
type KubernetesDoctorCheck struct {
	Name   string
	Status KubernetesDoctorStatus
	Detail string
	Action string
}

// KubernetesDoctorReport contains the local and live checks.
type KubernetesDoctorReport struct {
	Checks []KubernetesDoctorCheck
}

// HasFailures reports whether any deterministic blocker was found.
func (r KubernetesDoctorReport) HasFailures() bool {
	for _, check := range r.Checks {
		if check.Status == KubernetesDoctorFail {
			return true
		}
	}
	return false
}

func (r KubernetesDoctorReport) counts() (failures, warnings, unverified int) {
	for _, check := range r.Checks {
		switch check.Status {
		case KubernetesDoctorFail:
			failures++
		case KubernetesDoctorWarn:
			warnings++
		case KubernetesDoctorUnverified:
			unverified++
		}
	}
	return failures, warnings, unverified
}

type kubernetesDoctorDependencies struct {
	runner         commandRunner
	clusterFactory func(string) (clusterReader, error)
	consumerDoctor func(context.Context, onboard.DoctorOptions) onboard.DoctorReport
	readFile       func(string) ([]byte, error)
}

// KubernetesDoctor performs only local reads, Helm list/template, and Kubernetes GET/LIST requests.
func KubernetesDoctor(ctx context.Context, opts KubernetesDoctorOptions) KubernetesDoctorReport {
	return runKubernetesDoctor(ctx, opts, kubernetesDoctorDependencies{
		runner:         execRunner{},
		clusterFactory: newReadOnlyCluster,
		consumerDoctor: onboard.Doctor,
		readFile:       os.ReadFile,
	})
}

func runKubernetesDoctor(ctx context.Context, opts KubernetesDoctorOptions, deps kubernetesDoctorDependencies) KubernetesDoctorReport {
	report := KubernetesDoctorReport{}
	add := func(name string, status KubernetesDoctorStatus, detail, action string) {
		report.Checks = append(report.Checks, KubernetesDoctorCheck{Name: name, Status: status, Detail: detail, Action: action})
	}

	action := strings.TrimSpace(opts.Action)
	if action == "" {
		action = "auto"
	}
	if action != "auto" && action != "install" && action != "upgrade" {
		add("command", KubernetesDoctorFail, "--action must be auto, install, or upgrade", "Choose the operation that will follow this doctor run.")
		return report
	}
	if strings.TrimSpace(opts.KubeContext) == "" {
		add("Kubernetes context", KubernetesDoctorFail, "--kube-context is required; the current default context is never used implicitly", "Pass the reviewed context name explicitly.")
		return report
	}

	consumer := deps.consumerDoctor(ctx, onboard.DoctorOptions{ProjectDir: opts.ProjectDir})
	for _, check := range consumer.Checks {
		status := KubernetesDoctorPass
		switch check.Status {
		case onboard.DoctorWarn:
			status = KubernetesDoctorWarn
		case onboard.DoctorFail:
			status = KubernetesDoctorFail
		}
		add("consumer "+check.Name, status, check.Detail, check.Action)
	}

	deployAction := action
	if deployAction == "auto" {
		deployAction = "install"
	}
	deployOpts := Options{
		Action: deployAction, ProjectDir: opts.ProjectDir, ValuesFile: opts.ValuesFile,
		Release: opts.Release, Namespace: opts.Namespace, KubeContext: opts.KubeContext,
		Chart: opts.Chart, ChartVersion: opts.ChartVersion, DryRun: true,
	}
	resolved, skillPaths, err := validateBundle(deployOpts)
	if err != nil {
		add("deployment bundle", KubernetesDoctorFail, err.Error(), "Fix the consumer bundle and command flags, then rerun doctor.")
		return report
	}
	add("deployment bundle", KubernetesDoctorPass, fmt.Sprintf("validated project, prompt, values, and %d consumer skill files", len(skillPaths)), "")

	valuesYAML, err := deps.readFile(resolved.ValuesFile)
	if err != nil {
		add("deployment values", KubernetesDoctorFail, fmt.Sprintf("read values file: %v", err), "Restore the reviewed values file and rerun doctor.")
		return report
	}
	var values kubernetesDoctorValues
	if err := yaml.Unmarshal(valuesYAML, &values); err != nil {
		add("deployment values", KubernetesDoctorFail, fmt.Sprintf("parse values file: %v", err), "Fix deploy/values.yaml and rerun doctor.")
		return report
	}
	cfg, _, err := project.LoadDir(resolved.ProjectDir)
	if err != nil {
		add("project configuration", KubernetesDoctorFail, err.Error(), "Fix the strict project bundle and rerun doctor.")
		return report
	}
	checkLocalSecurity(add, values, cfg)

	manifest, err := renderDoctorManifest(ctx, resolved, skillPaths, deps.runner)
	if err != nil {
		add("chart render", KubernetesDoctorFail, err.Error(), "Use the schema and chart version that match the selected release, then rerun doctor.")
	} else {
		add("chart render", KubernetesDoctorPass, "Helm schema validation and complete local render passed", "")
		checkRenderedImages(add, manifest)
	}

	cluster, err := deps.clusterFactory(resolved.KubeContext)
	if err != nil {
		add("Kubernetes API", KubernetesDoctorFail, err.Error(), "Fix the explicit kube context or its credentials without disabling TLS verification.")
		return report
	}
	version, err := cluster.ServerVersion(ctx)
	if err != nil {
		add("Kubernetes API", KubernetesDoctorFail, fmt.Sprintf("server version is unavailable: %v", err), "Restore read access to the reviewed Kubernetes API context.")
		return report
	}
	add("Kubernetes API", KubernetesDoctorPass, "read-only API access succeeded; server version "+version, "")
	checkRequiredAPIs(ctx, add, cluster, values.AgentSandbox.FixRuntime.Enabled)
	checkNamespace(ctx, add, cluster, resolved.Namespace)

	exists, releaseStatus, revision, err := helmReleaseState(ctx, cluster, resolved.Namespace, resolved.Release)
	if err != nil {
		add("Helm release", KubernetesDoctorFail, "Helm release metadata is unavailable: "+err.Error(), "Restore metadata-only LIST access to Helm release Secrets.")
	} else {
		checkReleaseState(add, action, exists, releaseStatus, revision, resolved.Release, resolved.Namespace)
	}

	checkStorage(ctx, add, cluster, resolved, values)
	checkReleaseWorkloads(ctx, add, cluster, resolved, values, manifest, exists)
	checkSecretReferences(ctx, add, cluster, resolved, values)
	checkPublicOrigin(ctx, add, cluster, resolved, values, cfg, manifest, exists)
	if values.AgentSandbox.FixRuntime.Enabled {
		checkAgentSandbox(ctx, add, cluster, resolved, values, exists)
	}
	return report
}

func renderDoctorManifest(ctx context.Context, opts Options, skillPaths []string, runner commandRunner) ([]*unstructured.Unstructured, error) {
	args := helmArgs(opts, skillPaths)
	var stdout, stderr bytes.Buffer
	if err := runner.Run(ctx, "helm", args, &stdout, &stderr); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return nil, fmt.Errorf("helm template: %s", boundedDoctorText(message, 2048))
	}
	decoder := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(stdout.Bytes()), 4096)
	var objects []*unstructured.Unstructured
	for {
		var raw map[string]any
		err := decoder.Decode(&raw)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parse rendered chart: %w", err)
		}
		if len(raw) == 0 {
			continue
		}
		objects = append(objects, &unstructured.Unstructured{Object: raw})
	}
	return objects, nil
}

func checkRequiredAPIs(ctx context.Context, add func(string, KubernetesDoctorStatus, string, string), cluster clusterReader, agentSandbox bool) {
	required := []schema.GroupVersionResource{deploymentsGVR, cronJobsGVR, endpointSlicesGVR, networkPoliciesGVR, storageClassesGVR}
	if agentSandbox {
		required = append(required, runtimeClassesGVR, sandboxesGVR)
	}
	var missing []string
	for _, gvr := range required {
		exists, err := cluster.HasResource(ctx, gvr)
		if err != nil {
			add("required API "+gvr.String(), KubernetesDoctorWarn, "API discovery was unavailable: "+err.Error(), "Confirm RBAC allows discovery of this API.")
			continue
		}
		if !exists {
			missing = append(missing, gvr.String())
		}
	}
	if len(missing) > 0 {
		add("required Kubernetes APIs", KubernetesDoctorFail, "missing APIs: "+strings.Join(missing, ", "), "Install the required platform APIs before deploying the application.")
		return
	}
	add("required Kubernetes APIs", KubernetesDoctorPass, "all configured deployment APIs are served", "")
}

func checkNamespace(ctx context.Context, add func(string, KubernetesDoctorStatus, string, string), cluster clusterReader, namespace string) {
	if _, err := cluster.Get(ctx, namespacesGVR, "", namespace); err != nil {
		if apierrors.IsNotFound(err) {
			add("application namespace", KubernetesDoctorFail, "namespace "+namespace+" does not exist", "Have the platform administrator create and label the namespace before install or upgrade.")
		} else {
			add("application namespace", KubernetesDoctorFail, "namespace metadata is unavailable: "+err.Error(), "Restore GET access to the configured namespace.")
		}
		return
	}
	add("application namespace", KubernetesDoctorPass, "namespace "+namespace+" exists", "")
}

func helmReleaseState(ctx context.Context, cluster clusterReader, namespace, release string) (bool, string, int, error) {
	items, err := cluster.ListSecretMetadata(ctx, namespace, "owner=helm,name="+release)
	if err != nil {
		return false, "", 0, err
	}
	latestRevision := 0
	latestStatus := ""
	for _, item := range items.Items {
		revision, _ := strconv.Atoi(item.Labels["version"])
		if revision < latestRevision {
			continue
		}
		latestRevision = revision
		latestStatus = item.Labels["status"]
	}
	if latestRevision == 0 || latestStatus == "uninstalled" {
		return false, latestStatus, latestRevision, nil
	}
	return true, latestStatus, latestRevision, nil
}

func checkReleaseState(add func(string, KubernetesDoctorStatus, string, string), action string, exists bool, status string, revision int, release, namespace string) {
	if exists && strings.HasPrefix(status, "pending-") {
		add("Helm release", KubernetesDoctorFail, fmt.Sprintf("release %q revision %d is %s", release, revision, status), "Resolve or roll back the pending Helm operation before continuing.")
		return
	}
	switch {
	case action == "install" && exists:
		add("Helm release", KubernetesDoctorFail, fmt.Sprintf("release %q already exists in namespace %q with status %s", release, namespace, status), "Run doctor with -action upgrade, then use kubernetes upgrade.")
	case action == "upgrade" && !exists:
		add("Helm release", KubernetesDoctorFail, fmt.Sprintf("release %q does not exist in namespace %q", release, namespace), "Run doctor with -action install, then use kubernetes install.")
	case exists && status == "failed":
		add("Helm release", KubernetesDoctorWarn, fmt.Sprintf("release %q revision %d is failed; a guarded upgrade may repair it", release, revision), "Review Helm history and keep a rollback target before upgrade.")
	case exists:
		add("Helm release", KubernetesDoctorPass, fmt.Sprintf("release %q revision %d is %s and the next operation is upgrade", release, revision, status), "")
	default:
		add("Helm release", KubernetesDoctorPass, fmt.Sprintf("release %q is absent and the next operation is install", release), "")
	}
}

func checkLocalSecurity(add func(string, KubernetesDoctorStatus, string, string), values kubernetesDoctorValues, cfg *project.Config) {
	var inline []string
	if strings.TrimSpace(values.AI.Token) != "" {
		inline = append(inline, "ai.token")
	}
	if strings.TrimSpace(values.AI.GitHubReadToken) != "" {
		inline = append(inline, "ai.githubReadToken")
	}
	if strings.TrimSpace(values.Server.Actions.OAuth.ClientSecret) != "" {
		inline = append(inline, "server.actions.oauth.clientSecret")
	}
	if strings.TrimSpace(values.Server.Actions.OAuth.SessionKey) != "" {
		inline = append(inline, "server.actions.oauth.sessionKey")
	}
	if strings.TrimSpace(values.Server.Actions.OAuth.BotToken) != "" {
		inline = append(inline, "server.actions.oauth.botToken")
	}
	if strings.TrimSpace(values.Server.Actions.Proxy.BotToken) != "" || strings.TrimSpace(values.Server.Actions.Proxy.Secret) != "" {
		inline = append(inline, "server.actions.proxy credential")
	}
	for _, env := range values.Server.ExtraEnv {
		name := strings.ToUpper(strings.TrimSpace(env.Name))
		if strings.TrimSpace(env.Value) != "" && (strings.Contains(name, "TOKEN") || strings.Contains(name, "SECRET") || strings.Contains(name, "PASSWORD") || strings.Contains(name, "KEY")) {
			inline = append(inline, "server.extraEnv."+env.Name)
		}
	}
	if len(inline) > 0 {
		sort.Strings(inline)
		add("credential references", KubernetesDoctorFail, "inline credential fields are set: "+strings.Join(inline, ", "), "Move credential values to the organization Secret manager and retain only existing Secret names in consumer files.")
	} else {
		add("credential references", KubernetesDoctorPass, "consumer files contain references rather than credential values", "")
	}

	if values.AgentSandbox.FixRuntime.Enabled {
		checkImageReference(add, "Fix executor image", values.AgentSandbox.FixRuntime.Image.Repository, values.AgentSandbox.FixRuntime.Image.Digest, true)
		provider := values.AgentSandbox.FixRuntime.ModelProvider
		config := modelprovider.Normalize(modelprovider.Config{
			CredentialMode: provider.CredentialMode, API: provider.API, Endpoint: provider.Endpoint, Model: provider.Model,
			Auth: modelprovider.Auth{Type: provider.Auth.Type}, PublicCAPrivateDNS: provider.PublicCAPrivateDNS,
		})
		if err := modelprovider.ValidateDeploymentEndpoint(config); err != nil {
			add("Agent Sandbox provider endpoint", KubernetesDoctorFail, err.Error(), "Use a reviewed absolute HTTPS endpoint without credentials, query parameters, or fragments.")
		} else {
			add("Agent Sandbox provider endpoint", KubernetesDoctorPass, "configured endpoint satisfies the encrypted deployment contract", "")
		}
	}
	if cfg.Branding.SiteURL == "" {
		add("public URL", KubernetesDoctorWarn, "branding.site_url is empty", "Set the reviewed public HTTPS origin before enabling OAuth or authenticated features.")
	}
}

func checkRenderedImages(add func(string, KubernetesDoctorStatus, string, string), manifest []*unstructured.Unstructured) {
	set := map[string]struct{}{}
	for _, object := range manifest {
		podSpec, ok := renderedPodSpec(object)
		if !ok {
			continue
		}
		collectContainerImages(podSpec, set)
	}
	if len(set) == 0 {
		add("rendered images", KubernetesDoctorFail, "the rendered application chart contains no workload images", "Use the complete published application chart.")
		return
	}
	images := make([]string, 0, len(set))
	for image := range set {
		images = append(images, image)
	}
	sort.Strings(images)
	for _, image := range images {
		identity := ""
		if at := strings.LastIndex(image, "@"); at >= 0 {
			identity = image[at+1:]
		} else if colon := strings.LastIndex(image, ":"); colon > strings.LastIndex(image, "/") {
			identity = image[colon+1:]
		}
		lower := strings.ToLower(identity)
		if identity == "" || lower == "latest" || lower == "main" || lower == "dev" || strings.Contains(lower, "snapshot") {
			add("rendered image "+image, KubernetesDoctorFail, "workload image does not use a reviewed immutable identity", "Use a digest, commit tag, or semantic release tag.")
			continue
		}
		if !sha256DigestPattern.MatchString(identity) && !shaTagPattern.MatchString(identity) && !semverTagPattern.MatchString(identity) {
			add("rendered image "+image, KubernetesDoctorWarn, "image tag is not a digest, commit tag, or semantic release tag", "Confirm registry immutability or select a reviewed immutable identity.")
			continue
		}
		add("rendered image "+image, KubernetesDoctorPass, "image identity syntax is immutable or release-scoped", "")
	}
}

func renderedPodSpec(object *unstructured.Unstructured) (map[string]any, bool) {
	var fields []string
	switch object.GetKind() {
	case "Pod":
		fields = []string{"spec"}
	case "Deployment", "StatefulSet", "DaemonSet", "Job", "ReplicaSet", "ReplicationController":
		fields = []string{"spec", "template", "spec"}
	case "CronJob":
		fields = []string{"spec", "jobTemplate", "spec", "template", "spec"}
	default:
		return nil, false
	}
	podSpec, found, err := unstructured.NestedMap(object.Object, fields...)
	return podSpec, found && err == nil
}

func collectContainerImages(podSpec map[string]any, set map[string]struct{}) {
	for _, field := range []string{"initContainers", "containers", "ephemeralContainers"} {
		containers, _, _ := unstructured.NestedSlice(podSpec, field)
		for _, raw := range containers {
			container, _ := raw.(map[string]any)
			image, _, _ := unstructured.NestedString(container, "image")
			if strings.TrimSpace(image) != "" {
				set[strings.TrimSpace(image)] = struct{}{}
			}
		}
	}
}

func checkImageReference(add func(string, KubernetesDoctorStatus, string, string), name, repository, identity string, digestRequired bool) {
	repository = strings.TrimSpace(repository)
	identity = strings.TrimSpace(identity)
	if repository == "" || identity == "" {
		add(name, KubernetesDoctorFail, "repository or immutable identity is missing", "Set the reviewed published image reference.")
		return
	}
	if digestRequired {
		if !sha256DigestPattern.MatchString(identity) {
			add(name, KubernetesDoctorFail, "executor images require a sha256 digest", "Pin the published OCI image by digest.")
			return
		}
		add(name, KubernetesDoctorPass, "image is pinned by sha256 digest", "")
		return
	}
	lower := strings.ToLower(identity)
	if lower == "latest" || lower == "main" || lower == "dev" {
		add(name, KubernetesDoctorFail, "mutable or development image tag "+identity+" is not allowed", "Use a reviewed release tag, commit tag, or digest.")
		return
	}
	if sha256DigestPattern.MatchString(identity) || shaTagPattern.MatchString(identity) || semverTagPattern.MatchString(identity) {
		add(name, KubernetesDoctorPass, "image uses a reviewed immutable identity syntax", "")
		return
	}
	add(name, KubernetesDoctorWarn, "image identity "+identity+" is not a digest, commit tag, or semantic release tag", "Confirm registry immutability or replace it with a reviewed immutable identity.")
}

func checkStorage(ctx context.Context, add func(string, KubernetesDoctorStatus, string, string), cluster clusterReader, opts Options, values kubernetesDoctorValues) {
	claim := strings.TrimSpace(values.Persistence.ExistingClaim)
	if claim != "" {
		object, err := cluster.Get(ctx, persistentClaimsGVR, opts.Namespace, claim)
		if err != nil {
			add("persistent storage", KubernetesDoctorFail, "existing claim "+claim+" is unavailable: "+err.Error(), "Create the reviewed RWX claim or correct persistence.existingClaim.")
			return
		}
		modes, _, _ := unstructured.NestedStringSlice(object.Object, "spec", "accessModes")
		if !containsString(modes, "ReadWriteMany") {
			add("persistent storage", KubernetesDoctorFail, "existing claim "+claim+" does not declare ReadWriteMany", "Use a claim whose metadata declares ReadWriteMany.")
			return
		}
		phase, _, _ := unstructured.NestedString(object.Object, "status", "phase")
		if phase != "Bound" {
			add("persistent storage", KubernetesDoctorFail, "existing claim "+claim+" is not Bound", "Resolve the claim before install or upgrade.")
			return
		}
		add("persistent storage", KubernetesDoctorPass, "existing claim "+claim+" is Bound and declares ReadWriteMany", "")
		add("RWX semantics", KubernetesDoctorUnverified, "claim metadata cannot prove working multi-node RWX semantics", "Validate RWX behavior during target-cluster release acceptance.")
		checkClaimConsumers(ctx, add, cluster, opts, claim)
	} else {
		storageClass := strings.TrimSpace(values.Persistence.StorageClass)
		if storageClass == "" {
			add("persistent storage", KubernetesDoctorFail, "no storage class or existing claim is configured", "Select reviewed RWX storage before deployment.")
			return
		}
		if _, err := cluster.Get(ctx, storageClassesGVR, "", storageClass); err != nil {
			add("persistent storage", KubernetesDoctorFail, "storage class "+storageClass+" is unavailable: "+err.Error(), "Have the platform administrator provide the configured storage class.")
			return
		}
		if values.Persistence.AccessMode != "" && values.Persistence.AccessMode != "ReadWriteMany" {
			add("persistent storage", KubernetesDoctorFail, "configured access mode is "+values.Persistence.AccessMode, "Set persistence.accessMode to ReadWriteMany.")
			return
		}
		add("persistent storage", KubernetesDoctorPass, "storage class "+storageClass+" exists and the desired claim requests ReadWriteMany", "")
		add("RWX semantics", KubernetesDoctorUnverified, "read-only inspection cannot prove the storage driver provides working multi-node RWX semantics", "Validate RWX behavior during target-cluster release acceptance.")
	}
	claims := []string{values.Persistence.ExistingClaim, values.AgentSandbox.Analyzer.Input.ExistingClaim, values.AgentSandbox.CausalCritic.Ledger.ExistingClaim}
	seen := map[string]string{}
	for index, candidate := range claims {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		label := []string{"application data", "analyzer input", "critic ledger"}[index]
		if prior, ok := seen[candidate]; ok {
			add("separate claims", KubernetesDoctorFail, fmt.Sprintf("claim %s is reused for %s and %s", candidate, prior, label), "Use separate claims for public data and private runtime state.")
			return
		}
		seen[candidate] = label
	}
	add("separate claims", KubernetesDoctorPass, "configured application and private runtime claims do not overlap", "")
}

func checkClaimConsumers(ctx context.Context, add func(string, KubernetesDoctorStatus, string, string), cluster clusterReader, opts Options, claim string) {
	pods, err := cluster.List(ctx, podsGVR, opts.Namespace, "")
	if err != nil {
		add("claim ownership", KubernetesDoctorWarn, "pod claim consumers are unavailable: "+err.Error(), "Confirm no other release writes the configured claim.")
		return
	}
	var conflicts []string
	for _, pod := range pods.Items {
		instance := pod.GetLabels()["app.kubernetes.io/instance"]
		if instance == opts.Release {
			continue
		}
		volumes, _, _ := unstructured.NestedSlice(pod.Object, "spec", "volumes")
		for _, raw := range volumes {
			volume, _ := raw.(map[string]any)
			name, _, _ := unstructured.NestedString(volume, "persistentVolumeClaim", "claimName")
			if name == claim {
				conflicts = append(conflicts, pod.GetName())
			}
		}
	}
	if len(conflicts) > 0 {
		sort.Strings(conflicts)
		add("claim ownership", KubernetesDoctorFail, "claim is mounted by pods outside release "+opts.Release+": "+strings.Join(conflicts, ", "), "Stop the conflicting writer or select a separate claim.")
		return
	}
	add("claim ownership", KubernetesDoctorPass, "no pod outside the configured release mounts the existing claim", "")
}

func checkReleaseWorkloads(ctx context.Context, add func(string, KubernetesDoctorStatus, string, string), cluster clusterReader, opts Options, values kubernetesDoctorValues, manifest []*unstructured.Unstructured, releaseExists bool) {
	selector := "app.kubernetes.io/instance=" + opts.Release
	deployments, depErr := cluster.List(ctx, deploymentsGVR, opts.Namespace, selector)
	cronJobs, cronErr := cluster.List(ctx, cronJobsGVR, opts.Namespace, selector)
	if depErr != nil || cronErr != nil {
		add("writer workloads", KubernetesDoctorFail, "release workloads are unavailable", "Restore GET/LIST access to Deployments and CronJobs.")
		return
	}
	writers := 0
	for _, deployment := range deployments.Items {
		if deployment.GetLabels()["app.kubernetes.io/component"] == "worker" {
			writers++
		}
	}
	for _, cronJob := range cronJobs.Items {
		if cronJob.GetLabels()["app.kubernetes.io/component"] == "fetcher" {
			writers++
		}
	}
	if writers > 1 {
		add("writer workloads", KubernetesDoctorFail, "multiple live writer workloads are selected by the release", "Keep exactly one worker Deployment or fetcher CronJob.")
	} else if releaseExists && writers == 0 {
		add("writer workloads", KubernetesDoctorFail, "the existing release has no writer workload", "Restore the workload selected by mode before continuing.")
	} else if !releaseExists {
		add("writer workloads", KubernetesDoctorPass, "no existing writer conflicts with the planned install", "")
	} else {
		add("writer workloads", KubernetesDoctorPass, "the existing release has exactly one writer workload", "")
	}
	if values.Mode == "watch" && releaseExists {
		for _, cronJob := range cronJobs.Items {
			if cronJob.GetLabels()["app.kubernetes.io/component"] == "fetcher" {
				add("writer mode", KubernetesDoctorWarn, "desired watch mode will replace the live fetcher CronJob during the guarded upgrade", "Review the rendered diff and keep rollback-on-failure enabled.")
				break
			}
		}
	}
	if values.Mode == "cron" && releaseExists {
		for _, deployment := range deployments.Items {
			if deployment.GetLabels()["app.kubernetes.io/component"] == "worker" {
				add("writer mode", KubernetesDoctorWarn, "desired cron mode will replace the live worker Deployment during the guarded upgrade", "Review the rendered diff and keep rollback-on-failure enabled.")
				break
			}
		}
	}
	if len(manifest) > 0 {
		add("desired resources", KubernetesDoctorPass, fmt.Sprintf("local chart render contains %d Kubernetes objects", len(manifest)), "")
		checkDesiredServiceAccounts(ctx, add, cluster, manifest, releaseExists)
	}
}

func checkDesiredServiceAccounts(ctx context.Context, add func(string, KubernetesDoctorStatus, string, string), cluster clusterReader, manifest []*unstructured.Unstructured, releaseExists bool) {
	var desired []string
	for _, object := range manifest {
		if object.GetAPIVersion() == "v1" && object.GetKind() == "ServiceAccount" {
			desired = append(desired, object.GetNamespace()+"/"+object.GetName())
		}
	}
	if len(desired) == 0 {
		add("application ServiceAccounts", KubernetesDoctorWarn, "the rendered chart contains no application ServiceAccount", "Confirm the selected chart and existing ServiceAccount contract.")
		return
	}
	sort.Strings(desired)
	if !releaseExists {
		add("application ServiceAccounts", KubernetesDoctorPass, fmt.Sprintf("the chart owns %d desired ServiceAccount(s)", len(desired)), "")
		return
	}
	var missing []string
	for _, identity := range desired {
		parts := strings.SplitN(identity, "/", 2)
		if _, err := cluster.Get(ctx, serviceAccountsGVR, parts[0], parts[1]); err != nil {
			if apierrors.IsForbidden(err) {
				add("application ServiceAccounts", KubernetesDoctorFail, "RBAC forbids reading release-owned ServiceAccount "+identity, "Restore metadata read access before validating the existing release.")
				return
			}
			missing = append(missing, identity)
		}
	}
	if len(missing) > 0 {
		add("application ServiceAccounts", KubernetesDoctorFail, "release-owned ServiceAccounts are missing: "+strings.Join(missing, ", "), "Restore them through the owning application release.")
		return
	}
	add("application ServiceAccounts", KubernetesDoctorPass, "all rendered release-owned ServiceAccounts exist", "")
}

func checkSecretReferences(ctx context.Context, add func(string, KubernetesDoctorStatus, string, string), cluster clusterReader, opts Options, values kubernetesDoctorValues) {
	type secretRef struct{ namespace, name, purpose string }
	var refs []secretRef
	if values.AI.ExistingSecret != "" {
		refs = append(refs, secretRef{opts.Namespace, values.AI.ExistingSecret, "application AI"})
	}
	if values.Server.Actions.OAuth.ExistingSecret != "" {
		refs = append(refs, secretRef{opts.Namespace, values.Server.Actions.OAuth.ExistingSecret, "OAuth"})
	}
	if values.Server.Actions.Proxy.ExistingSecret != "" {
		refs = append(refs, secretRef{opts.Namespace, values.Server.Actions.Proxy.ExistingSecret, "proxy authentication"})
	}
	fix := values.AgentSandbox.FixRuntime
	if fix.Enabled && fix.ModelProvider.Auth.ExistingSecret != "" {
		refs = append(refs, secretRef{fix.Namespace, fix.ModelProvider.Auth.ExistingSecret, "Agent Sandbox provider"})
	}
	if len(refs) == 0 {
		add("Secret references", KubernetesDoctorPass, "no configured existing Secret references require live verification", "")
		return
	}
	for _, ref := range refs {
		if _, err := cluster.SecretMetadata(ctx, ref.namespace, ref.name); err != nil {
			add(ref.purpose+" Secret", KubernetesDoctorFail, fmt.Sprintf("Secret metadata %s/%s is unavailable: %v", ref.namespace, ref.name, err), "Have the Secret manager provision the referenced Secret name.")
			continue
		}
		add(ref.purpose+" Secret", KubernetesDoctorPass, fmt.Sprintf("Secret metadata %s/%s exists; key names and values were not read", ref.namespace, ref.name), "")
	}
	add("Secret key validation", KubernetesDoctorUnverified, "metadata-only Kubernetes reads intentionally do not inspect Secret key names or values", "Verify required keys through the organization Secret-management workflow.")
}

func checkPublicOrigin(ctx context.Context, add func(string, KubernetesDoctorStatus, string, string), cluster clusterReader, opts Options, values kubernetesDoctorValues, cfg *project.Config, manifest []*unstructured.Unstructured, releaseExists bool) {
	if values.Server.Security.HSTS.Enabled != nil && !*values.Server.Security.HSTS.Enabled {
		add("HSTS", KubernetesDoctorFail, "HSTS is disabled in deployment values", "Enable server.security.hsts.enabled for a deployed HTTPS origin.")
	} else {
		add("HSTS", KubernetesDoctorPass, "HSTS remains enabled", "")
	}
	if values.Server.Development.AllowInsecureCookies || values.Server.Development.AllowInsecureHTTP {
		add("secure cookies", KubernetesDoctorFail, "development insecure HTTP or cookie settings are enabled", "Disable development-only insecure settings before deployment.")
	} else {
		add("secure cookies", KubernetesDoctorPass, "development insecure HTTP and cookie settings are disabled", "")
	}
	if values.Server.Actions.Enabled && values.Server.Actions.Mode == "oauth" {
		site := strings.TrimRight(strings.TrimSpace(cfg.Branding.SiteURL), "/")
		want := site + "/api/auth/callback"
		if site == "" || values.Server.Actions.OAuth.RedirectURL != want {
			add("OAuth callback", KubernetesDoctorFail, "OAuth callback does not match branding.site_url plus /api/auth/callback", "Update the reviewed public URL, values, and OAuth application together.")
		} else {
			add("OAuth callback", KubernetesDoctorPass, "OAuth callback matches the configured public origin", "")
		}
	}
	if values.Ingress.Enabled && strings.EqualFold(values.Server.Service.Type, "LoadBalancer") {
		add("public topology", KubernetesDoctorFail, "Ingress and LoadBalancer origin are both enabled", "Choose one documented public topology.")
	}
	serviceType := firstNonempty(values.Server.Service.Type, "ClusterIP")
	if serviceType == "LoadBalancer" && !values.Server.Service.Internal.Enabled && len(values.Server.Service.LoadBalancerSourceRanges) == 0 {
		add("direct origin exposure", KubernetesDoctorFail, "public LoadBalancer has no internal annotation contract or source restrictions", "Restrict the origin to the reviewed edge path or use ClusterIP behind an ingress/proxy.")
	} else {
		add("direct origin exposure", KubernetesDoctorPass, "configured Service topology includes an origin restriction or is not public", "")
	}
	if releaseExists {
		selector := "app.kubernetes.io/instance=" + opts.Release + ",app.kubernetes.io/component=server"
		services, err := cluster.List(ctx, servicesGVR, opts.Namespace, selector)
		if err != nil {
			add("live Service topology", KubernetesDoctorFail, "server Services are unreadable: "+err.Error(), "Restore LIST access before validating public exposure.")
		} else if len(services.Items) != 1 {
			add("live Service topology", KubernetesDoctorFail, fmt.Sprintf("expected one server Service, found %d", len(services.Items)), "Restore the release-owned server Service before upgrade.")
		} else {
			liveType, _, _ := unstructured.NestedString(services.Items[0].Object, "spec", "type")
			if liveType != "" && liveType != serviceType {
				add("live Service topology", KubernetesDoctorFail, "live Service type "+liveType+" differs from desired "+serviceType, "Review the upgrade diff before changing public exposure.")
			} else {
				add("live Service topology", KubernetesDoctorPass, "live Service type matches the desired topology", "")
			}
		}
		ingresses, err := cluster.List(ctx, ingressesGVR, opts.Namespace, "app.kubernetes.io/instance="+opts.Release)
		if err != nil {
			add("live Ingress", KubernetesDoctorFail, "Ingresses are unreadable: "+err.Error(), "Restore LIST access before validating public exposure.")
		} else if len(ingresses.Items) > 0 && !values.Ingress.Enabled {
			add("live Ingress", KubernetesDoctorFail, "an Ingress exists although the desired topology disables it", "Remove the conflicting origin through the owning release or infrastructure workflow.")
		} else {
			add("live Ingress", KubernetesDoctorPass, "live Ingress state matches the desired topology", "")
		}
	}
	if parsed, err := url.Parse(cfg.Branding.SiteURL); err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		add("public URL", KubernetesDoctorFail, "branding.site_url must be a clean absolute HTTPS origin", "Set the externally reviewed public HTTPS URL.")
	} else {
		add("public URL", KubernetesDoctorPass, "public URL uses a clean HTTPS origin", "")
	}
	_ = manifest
	add("external edge and DNS", KubernetesDoctorUnverified, "Kubernetes reads cannot prove externally managed edge routing, DNS ownership, or certificate issuance; private-only deployments should confirm that public exposure is intentionally absent", "Verify public-origin infrastructure through the organization's infrastructure-as-code and target-cluster release acceptance.")
}

func checkAgentSandbox(ctx context.Context, add func(string, KubernetesDoctorStatus, string, string), cluster clusterReader, opts Options, values kubernetesDoctorValues, releaseExists bool) {
	fix := values.AgentSandbox.FixRuntime
	crd, err := cluster.Get(ctx, customResourcesGVR, "", "sandboxes.agents.x-k8s.io")
	if err != nil {
		add("Agent Sandbox CRD", KubernetesDoctorFail, "sandboxes.agents.x-k8s.io is unavailable: "+err.Error(), "Install the checksum-verified upstream Agent Sandbox v0.5.3 core manifest.")
	} else if !crdServesStorageVersion(crd, "v1beta1") {
		add("Agent Sandbox CRD", KubernetesDoctorFail, "v1beta1 is not both served and storage", "Install the supported upstream Agent Sandbox v0.5.3 CRD.")
	} else {
		add("Agent Sandbox CRD", KubernetesDoctorPass, "v1beta1 is served and storage for Sandbox objects", "")
	}
	controller, err := cluster.Get(ctx, deploymentsGVR, agentSandboxSystemNamespace, agentSandboxControllerName)
	if err != nil {
		add("Agent Sandbox controller", KubernetesDoctorFail, "controller Deployment is unavailable: "+err.Error(), "Install the supported upstream Agent Sandbox release.")
	} else {
		image := containerImage(controller, agentSandboxControllerName)
		available, _, _ := unstructured.NestedInt64(controller.Object, "status", "availableReplicas")
		if image != supportedAgentSandboxControllerImage && image != "registry.k8s.io/agent-sandbox/agent-sandbox-controller@"+supportedAgentSandboxControllerDigest {
			add("Agent Sandbox controller", KubernetesDoctorFail, "controller image does not match supported "+supportedAgentSandboxVersion, "Install or restore the pinned supported upstream release.")
		} else if available < 1 {
			add("Agent Sandbox controller", KubernetesDoctorFail, "controller Deployment has no available replicas", "Restore controller availability before deploying Fix runtime support.")
		} else {
			add("Agent Sandbox controller", KubernetesDoctorPass, "supported v0.5.3 controller is available", "")
		}
	}
	checkNamedObject(ctx, add, cluster, serviceAccountsGVR, agentSandboxSystemNamespace, agentSandboxControllerName, "Agent Sandbox controller ServiceAccount")
	checkEndpointService(ctx, add, cluster, agentSandboxSystemNamespace, agentSandboxControllerName, "Agent Sandbox controller endpoints")
	checkEndpointService(ctx, add, cluster, agentSandboxSystemNamespace, agentSandboxWebhookServiceName, "Agent Sandbox webhook endpoints")
	checkControllerPods(ctx, add, cluster)

	executionNamespace, err := cluster.Get(ctx, namespacesGVR, "", fix.Namespace)
	if err != nil {
		add("Agent Sandbox execution namespace", KubernetesDoctorFail, "namespace "+fix.Namespace+" is unavailable: "+err.Error(), "Install the platform bundle before the application release.")
	} else if executionNamespace.GetLabels()["prow-ai-dashboard/release"] != opts.Release {
		add("Agent Sandbox execution namespace", KubernetesDoctorFail, "namespace is not dedicated to release "+opts.Release, "Install the platform bundle with prow-ai-dashboard/release set to the application release name.")
	} else if executionNamespace.GetAnnotations()["prow-ai-dashboard/runtime-class"] != fix.RuntimeClassName {
		add("Agent Sandbox execution namespace", KubernetesDoctorFail, "namespace runtime-class contract does not match "+fix.RuntimeClassName, "Upgrade the platform bundle with the reviewed secure RuntimeClass name.")
	} else if executionNamespace.GetAnnotations()["prow-ai-dashboard/agent-sandbox-version"] != supportedAgentSandboxVersion {
		add("Agent Sandbox execution namespace", KubernetesDoctorFail, "namespace Agent Sandbox contract is not "+supportedAgentSandboxVersion, "Upgrade the platform bundle to the supported controller contract.")
	} else if executionNamespace.GetAnnotations()["prow-ai-dashboard/default-deny-policy-name"] == "" || executionNamespace.GetAnnotations()["prow-ai-dashboard/execution-policy-name"] == "" || executionNamespace.GetAnnotations()["prow-ai-dashboard/execution-policy-sha256"] == "" || executionNamespace.GetAnnotations()["prow-ai-dashboard/platform-release"] == "" {
		add("Agent Sandbox execution namespace", KubernetesDoctorFail, "namespace does not record the complete network-policy and platform-release contract", "Upgrade the platform bundle before enabling Fix runtime.")
	} else if executionNamespace.GetAnnotations()["prow-ai-dashboard/network-policy-mode"] != "cilium" {
		add("Agent Sandbox execution namespace", KubernetesDoctorFail, "namespace selects an unsupported network-policy backend", "Install the platform bundle with the supported FQDN-aware Cilium backend.")
	} else {
		add("Agent Sandbox execution namespace", KubernetesDoctorPass, "namespace records the release, runtime, controller, and FQDN-aware Cilium policy contract", "")
	}
	policyHash := ""
	networkPolicyMode := ""
	if executionNamespace != nil {
		policyHash = executionNamespace.GetAnnotations()["prow-ai-dashboard/execution-policy-sha256"]
		networkPolicyMode = executionNamespace.GetAnnotations()["prow-ai-dashboard/network-policy-mode"]
	}
	platformRelease := ""
	if executionNamespace != nil {
		platformRelease = executionNamespace.GetAnnotations()["prow-ai-dashboard/platform-release"]
	}
	platformBinding := checkPlatformBinding(ctx, add, cluster, opts.Namespace, opts.Release, fix, policyHash, platformRelease)
	runtimeClass, err := cluster.Get(ctx, runtimeClassesGVR, "", fix.RuntimeClassName)
	if err != nil {
		add("Agent Sandbox RuntimeClass", KubernetesDoctorFail, "RuntimeClass "+fix.RuntimeClassName+" is unavailable: "+err.Error(), "Have the infrastructure owner install and configure the secure runtime handler.")
	} else {
		handler, _, _ := unstructured.NestedString(runtimeClass.Object, "handler")
		if strings.TrimSpace(handler) == "" {
			add("Agent Sandbox RuntimeClass", KubernetesDoctorFail, "RuntimeClass handler is empty", "Configure a real secure runtime handler on compatible nodes.")
		} else {
			add("Agent Sandbox RuntimeClass", KubernetesDoctorPass, "RuntimeClass exists with a nonempty handler", "")
			checkRuntimeNodes(ctx, add, cluster, runtimeClass)
		}
	}
	checkExecutionBounds(ctx, add, cluster, fix.Namespace)
	checkWorkloadServiceAccount(ctx, add, cluster, fix, releaseExists)
	checkActiveSandboxes(ctx, add, cluster, fix.Namespace, opts.Release)
	if networkPolicyMode == "cilium" {
		checkGateway(ctx, add, cluster, opts.Namespace, fix, platformBinding)
	} else if fix.ModelProvider.CredentialMode == modelprovider.CredentialModeGateway {
		add("model gateway network-policy backend", KubernetesDoctorFail, "the selected platform contract is not the supported FQDN-aware Cilium backend", "Install or restore the platform chart with execution.networkPolicy.mode=cilium.")
	}
	add("hostile-code isolation", KubernetesDoctorUnverified, "resource presence does not prove that the RuntimeClass enforces hostile-code isolation", "Validate the secure runtime and node image during target-cluster release acceptance.")
}

func checkPlatformBinding(ctx context.Context, add func(string, KubernetesDoctorStatus, string, string), cluster clusterReader, applicationNamespace, release string, fix doctorFixRuntimeValues, policyHash, platformRelease string) map[string]string {
	selector := "app.kubernetes.io/part-of=prow-ai-dashboard-platform,app.kubernetes.io/component=platform-binding"
	bindings, err := cluster.List(ctx, configMapsGVR, applicationNamespace, selector)
	if err != nil {
		add("platform binding", KubernetesDoctorFail, "platform binding ConfigMaps are unreadable: "+err.Error(), "Restore LIST access to the application namespace.")
		return nil
	}
	var matches []map[string]string
	for _, binding := range bindings.Items {
		data, _, _ := unstructured.NestedStringMap(binding.Object, "data")
		if data["applicationReleaseName"] != release || data["executionNamespace"] != fix.Namespace || data["runtimeClassName"] != fix.RuntimeClassName || policyHash == "" || data["executionPolicySHA256"] != policyHash || platformRelease == "" || binding.GetLabels()["app.kubernetes.io/instance"] != platformRelease {
			continue
		}
		gatewayMode := fix.ModelProvider.CredentialMode == modelprovider.CredentialModeGateway
		if gatewayMode != (data["modelGatewayEnabled"] == "true") {
			continue
		}
		if gatewayMode {
			if data["modelGatewayExecutionNamespace"] != fix.Namespace || !validPublicHostname(data["modelGatewayUpstreamHost"]) || data["modelGatewayTargetPort"] == "" || data["modelGatewayPolicySHA256"] != gatewayPolicyHash(fix.Namespace, data["modelGatewayUpstreamHost"], data["modelGatewayTargetPort"]) {
				continue
			}
			if fix.ModelProvider.PublicCAPrivateDNS {
				parsed, _ := url.Parse(fix.ModelProvider.Endpoint)
				if !strings.EqualFold(data["modelGatewayPublicHost"], parsed.Hostname()) {
					continue
				}
			}
		}
		matches = append(matches, data)
	}
	if len(matches) != 1 {
		add("platform binding", KubernetesDoctorFail, fmt.Sprintf("expected one immutable platform binding, found %d", len(matches)), "Install or repair the platform chart in the application namespace.")
		return nil
	}
	add("platform binding", KubernetesDoctorPass, "immutable application, execution namespace, RuntimeClass, egress, and gateway binding is present", "")
	return matches[0]
}

func crdServesStorageVersion(crd *unstructured.Unstructured, version string) bool {
	versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
	for _, raw := range versions {
		candidate, _ := raw.(map[string]any)
		name, _, _ := unstructured.NestedString(candidate, "name")
		served, _, _ := unstructured.NestedBool(candidate, "served")
		storage, _, _ := unstructured.NestedBool(candidate, "storage")
		if name == version && served && storage {
			return true
		}
	}
	return false
}

func checkNamedObject(ctx context.Context, add func(string, KubernetesDoctorStatus, string, string), cluster clusterReader, gvr schema.GroupVersionResource, namespace, name, checkName string) {
	if _, err := cluster.Get(ctx, gvr, namespace, name); err != nil {
		add(checkName, KubernetesDoctorFail, fmt.Sprintf("%s/%s is unavailable: %v", namespace, name, err), "Restore the supported platform resource.")
		return
	}
	add(checkName, KubernetesDoctorPass, fmt.Sprintf("%s/%s exists", namespace, name), "")
}

func checkEndpointService(ctx context.Context, add func(string, KubernetesDoctorStatus, string, string), cluster clusterReader, namespace, service, name string) {
	if _, err := cluster.Get(ctx, servicesGVR, namespace, service); err != nil {
		add(name, KubernetesDoctorFail, "Service is unavailable: "+err.Error(), "Restore the supported upstream controller Service.")
		return
	}
	slices, err := cluster.List(ctx, endpointSlicesGVR, namespace, "kubernetes.io/service-name="+service)
	if err != nil || !hasReadyEndpoint(slices) {
		add(name, KubernetesDoctorFail, "no ready EndpointSlice endpoint is visible", "Restore the selected controller or webhook Pods.")
		return
	}
	add(name, KubernetesDoctorPass, "Service has a ready EndpointSlice endpoint", "")
}

func checkControllerPods(ctx context.Context, add func(string, KubernetesDoctorStatus, string, string), cluster clusterReader) {
	pods, err := cluster.List(ctx, podsGVR, agentSandboxSystemNamespace, "app=agent-sandbox-controller")
	if err != nil || len(pods.Items) == 0 {
		add("Agent Sandbox controller image identity", KubernetesDoctorWarn, "running controller Pod image identity is unavailable", "Confirm the controller Pod imageID after it starts.")
		return
	}
	var seen bool
	for _, pod := range pods.Items {
		statuses, _, _ := unstructured.NestedSlice(pod.Object, "status", "containerStatuses")
		for _, raw := range statuses {
			status, _ := raw.(map[string]any)
			name, _, _ := unstructured.NestedString(status, "name")
			imageID, _, _ := unstructured.NestedString(status, "imageID")
			if name != agentSandboxControllerName || imageID == "" {
				continue
			}
			seen = true
			if !strings.Contains(imageID, supportedAgentSandboxAMD64Digest) && !strings.Contains(imageID, supportedAgentSandboxARM64Digest) && !strings.Contains(imageID, supportedAgentSandboxControllerDigest) {
				add("Agent Sandbox controller image identity", KubernetesDoctorFail, "running controller imageID is not a supported v0.5.3 digest", "Restore the pinned upstream controller image.")
				return
			}
		}
	}
	if !seen {
		add("Agent Sandbox controller image identity", KubernetesDoctorWarn, "running controller Pod has no imageID yet", "Wait for Pod startup and rerun doctor.")
		return
	}
	add("Agent Sandbox controller image identity", KubernetesDoctorPass, "running controller imageID matches a supported v0.5.3 manifest", "")
}

func checkRuntimeNodes(ctx context.Context, add func(string, KubernetesDoctorStatus, string, string), cluster clusterReader, runtimeClass *unstructured.Unstructured) {
	selector, _, _ := unstructured.NestedStringMap(runtimeClass.Object, "scheduling", "nodeSelector")
	tolerations, _, _ := unstructured.NestedSlice(runtimeClass.Object, "scheduling", "tolerations")
	nodes, err := cluster.List(ctx, nodesGVR, "", "")
	if err != nil {
		add("secure runtime nodes", KubernetesDoctorFail, "nodes are unavailable: "+err.Error(), "Restore read access to node readiness and labels.")
		return
	}
	matches := 0
	for _, node := range nodes.Items {
		if nestedBool(node.Object, "spec", "unschedulable") || !nodeReady(&node) || !labelsMatch(node.GetLabels(), selector) || !nodeTaintsTolerated(&node, tolerations) {
			continue
		}
		matches++
	}
	if matches == 0 {
		add("secure runtime nodes", KubernetesDoctorFail, "no Ready schedulable node matches the RuntimeClass scheduling selector", "Provision or repair compatible secure-runtime nodes.")
		return
	}
	add("secure runtime nodes", KubernetesDoctorPass, fmt.Sprintf("%d Ready schedulable node(s) match observable RuntimeClass selectors", matches), "")
	add("runtime handler enforcement", KubernetesDoctorUnverified, "node labels and RuntimeClass metadata do not prove the handler is installed or functional", "Validate the runtime handler during target-cluster release acceptance.")
}

func nodeTaintsTolerated(node *unstructured.Unstructured, tolerations []any) bool {
	taints, _, _ := unstructured.NestedSlice(node.Object, "spec", "taints")
	for _, raw := range taints {
		taint, _ := raw.(map[string]any)
		effect, _, _ := unstructured.NestedString(taint, "effect")
		if effect != "NoSchedule" && effect != "NoExecute" {
			continue
		}
		if !taintTolerated(taint, tolerations) {
			return false
		}
	}
	return true
}

func taintTolerated(taint map[string]any, tolerations []any) bool {
	taintKey, _, _ := unstructured.NestedString(taint, "key")
	taintValue, _, _ := unstructured.NestedString(taint, "value")
	taintEffect, _, _ := unstructured.NestedString(taint, "effect")
	for _, raw := range tolerations {
		toleration, _ := raw.(map[string]any)
		key, _, _ := unstructured.NestedString(toleration, "key")
		value, _, _ := unstructured.NestedString(toleration, "value")
		effect, _, _ := unstructured.NestedString(toleration, "effect")
		operator, _, _ := unstructured.NestedString(toleration, "operator")
		if operator == "" {
			operator = "Equal"
		}
		if effect != "" && effect != taintEffect {
			continue
		}
		if operator == "Exists" && (key == "" || key == taintKey) {
			return true
		}
		if operator == "Equal" && key == taintKey && value == taintValue {
			return true
		}
	}
	return false
}

func checkExecutionBounds(ctx context.Context, add func(string, KubernetesDoctorStatus, string, string), cluster clusterReader, namespace string) {
	quotas, err := cluster.List(ctx, resourceQuotasGVR, namespace, "")
	if err != nil || len(quotas.Items) == 0 {
		add("Agent Sandbox ResourceQuota", KubernetesDoctorFail, "execution namespace has no visible ResourceQuota", "Install the platform bundle quota before enabling Fix runtime.")
	} else {
		valid := false
		for _, quota := range quotas.Items {
			pods, _, _ := unstructured.NestedString(quota.Object, "spec", "hard", "pods")
			sandboxes, _, _ := unstructured.NestedString(quota.Object, "spec", "hard", "count/sandboxes.agents.x-k8s.io")
			if positiveQuantity(pods) && positiveQuantity(sandboxes) {
				valid = true
				break
			}
		}
		if !valid {
			add("Agent Sandbox ResourceQuota", KubernetesDoctorFail, "quota does not bound both Pods and Sandbox objects", "Set positive pods and count/sandboxes.agents.x-k8s.io hard limits.")
		} else {
			add("Agent Sandbox ResourceQuota", KubernetesDoctorPass, "execution namespace bounds Pods and Sandbox objects", "")
		}
	}
	limits, err := cluster.List(ctx, limitRangesGVR, namespace, "")
	if err != nil || len(limits.Items) == 0 {
		add("Agent Sandbox LimitRange", KubernetesDoctorFail, "execution namespace has no visible LimitRange", "Install the platform bundle container bounds.")
	} else {
		add("Agent Sandbox LimitRange", KubernetesDoctorPass, "execution namespace has a LimitRange", "")
	}
	policies, err := cluster.List(ctx, networkPoliciesGVR, namespace, "")
	if err != nil {
		add("Agent Sandbox network policy", KubernetesDoctorFail, "execution namespace NetworkPolicies are unreadable: "+err.Error(), "Restore LIST access to the execution namespace.")
		return
	}
	namespaceObject, err := cluster.Get(ctx, namespacesGVR, "", namespace)
	if err != nil {
		add("Agent Sandbox Cilium policy", KubernetesDoctorFail, "execution namespace annotations are unavailable: "+err.Error(), "Restore read access to the platform namespace contract.")
		return
	}
	annotations := namespaceObject.GetAnnotations()
	defaultDenyName := annotations["prow-ai-dashboard/default-deny-policy-name"]
	platformRelease := annotations["prow-ai-dashboard/platform-release"]
	if len(policies.Items) != 1 || policies.Items[0].GetName() != defaultDenyName || !hasStrictDefaultDeny(&policies.Items[0]) {
		add("Agent Sandbox network policy", KubernetesDoctorFail, "execution namespace must contain exactly the release-owned default-deny NetworkPolicy", "Remove stale or additional policies and restore the platform chart boundary.")
		return
	}
	policyLabels := policies.Items[0].GetLabels()
	if policyLabels["app.kubernetes.io/part-of"] != "prow-ai-dashboard-platform" || policyLabels["app.kubernetes.io/component"] != "agent-sandbox-execution" || policyLabels["app.kubernetes.io/instance"] != platformRelease {
		add("Agent Sandbox network policy", KubernetesDoctorFail, "default-deny NetworkPolicy lacks the exact platform release ownership labels", "Restore the chart-owned policy.")
		return
	}
	add("Agent Sandbox network policy", KubernetesDoctorPass, "execution namespace contains only the release-owned default-deny policy", "")
	if annotations["prow-ai-dashboard/network-policy-mode"] != "cilium" {
		add("Agent Sandbox network-policy backend", KubernetesDoctorFail, "the selected platform contract is not the supported FQDN-aware Cilium backend", "Install or restore the platform chart with execution.networkPolicy.mode=cilium.")
		return
	}
	add("Agent Sandbox network-policy backend", KubernetesDoctorPass, "the platform contract selects the supported FQDN-aware Cilium backend", "")
	policyName := annotations["prow-ai-dashboard/execution-policy-name"]
	served, err := cluster.HasResource(ctx, ciliumPoliciesGVR)
	if err != nil || !served {
		add("Agent Sandbox Cilium policy", KubernetesDoctorFail, "the cilium.io/v2 CiliumNetworkPolicy API is unavailable", "Install the supported FQDN-aware Cilium network-policy backend before the platform bundle.")
		return
	}
	ciliumPolicies, err := cluster.List(ctx, ciliumPoliciesGVR, namespace, "")
	if err != nil {
		add("Agent Sandbox Cilium policy", KubernetesDoctorFail, "execution namespace Cilium policies are unreadable: "+err.Error(), "Restore LIST access to the execution namespace.")
		return
	}
	if len(ciliumPolicies.Items) != 1 || ciliumPolicies.Items[0].GetName() != policyName {
		add("Agent Sandbox Cilium policy", KubernetesDoctorFail, "execution namespace must contain exactly the release-owned Cilium egress policy", "Remove stale or additional Cilium policies and restore the platform chart boundary.")
		return
	}
	policy := &ciliumPolicies.Items[0]
	if err := validateCiliumExecutionPolicy(policy, annotations); err != nil {
		add("Agent Sandbox Cilium policy", KubernetesDoctorFail, err.Error(), "Restore the exact platform-chart egress policy and rerun doctor.")
		return
	}
	clusterwideServed, err := cluster.HasResource(ctx, ciliumClusterwidePoliciesGVR)
	if err != nil || !clusterwideServed {
		add("Agent Sandbox Cilium policy", KubernetesDoctorFail, "the cluster-wide Cilium policy API is unavailable", "Restore the supported Cilium policy APIs.")
		return
	}
	clusterwide, err := cluster.List(ctx, ciliumClusterwidePoliciesGVR, "", "")
	if err != nil {
		add("Agent Sandbox Cilium policy", KubernetesDoctorFail, "cluster-wide Cilium policies are unreadable: "+err.Error(), "Restore LIST access to cluster-wide Cilium policy metadata.")
		return
	}
	for _, candidate := range clusterwide.Items {
		if clusterwidePolicyMaySelectNamespace(&candidate, namespace) {
			add("Agent Sandbox Cilium policy", KubernetesDoctorFail, "cluster-wide Cilium policy "+candidate.GetName()+" may select the release-dedicated execution namespace", "Remove or narrowly scope the cluster-wide policy before enabling hostile workloads.")
			return
		}
	}
	add("Agent Sandbox Cilium policy", KubernetesDoctorPass, "release-owned Cilium egress policy matches the bounded exact-host contract and no cluster-wide policy can select the namespace", "")
}

func hasStrictDefaultDeny(policy *unstructured.Unstructured) bool {
	selector, found, _ := unstructured.NestedMap(policy.Object, "spec", "podSelector")
	return found && len(selector) == 0 && hasNoAllowRules(policy)
}

func hasNoAllowRules(policy *unstructured.Unstructured) bool {
	types, _, _ := unstructured.NestedStringSlice(policy.Object, "spec", "policyTypes")
	if !containsString(types, "Ingress") || !containsString(types, "Egress") {
		return false
	}
	for _, field := range []string{"ingress", "egress"} {
		rules, found, _ := unstructured.NestedSlice(policy.Object, "spec", field)
		if found && len(rules) != 0 {
			return false
		}
	}
	return true
}

func clusterwidePolicyMaySelectNamespace(policy *unstructured.Unstructured, namespace string) bool {
	selector, found, err := unstructured.NestedMap(policy.Object, "spec", "endpointSelector")
	if err != nil || !found || len(selector) == 0 {
		return true
	}
	labels, _, _ := unstructured.NestedStringMap(selector, "matchLabels")
	if selectedNamespace, ok := labels["k8s:io.kubernetes.pod.namespace"]; ok {
		return selectedNamespace == namespace
	}
	if selectedNamespace, ok := labels["io.kubernetes.pod.namespace"]; ok {
		return selectedNamespace == namespace
	}
	return true
}

func validateCiliumExecutionPolicy(policy *unstructured.Unstructured, annotations map[string]string) error {
	platformRelease := annotations["prow-ai-dashboard/platform-release"]
	expectedHash := annotations["prow-ai-dashboard/execution-policy-sha256"]
	labels := policy.GetLabels()
	if labels["app.kubernetes.io/part-of"] != "prow-ai-dashboard-platform" || labels["app.kubernetes.io/component"] != "agent-sandbox-execution" || labels["app.kubernetes.io/instance"] != platformRelease || platformRelease == "" {
		return fmt.Errorf("cilium egress policy lacks the exact platform release ownership labels")
	}
	if expectedHash == "" || policy.GetAnnotations()["prow-ai-dashboard/execution-policy-sha256"] != expectedHash {
		return fmt.Errorf("cilium egress policy hash does not match the namespace contract")
	}
	selector, found, err := unstructured.NestedMap(policy.Object, "spec", "endpointSelector")
	if err != nil || !found || len(selector) != 0 {
		return fmt.Errorf("cilium egress policy must select only the dedicated namespace through an empty endpointSelector")
	}
	ingress, found, err := unstructured.NestedSlice(policy.Object, "spec", "ingress")
	if err != nil || !found || len(ingress) != 0 {
		return fmt.Errorf("cilium egress policy must deny ingress")
	}
	egress, found, err := unstructured.NestedSlice(policy.Object, "spec", "egress")
	if err != nil || !found || len(egress) == 0 {
		return fmt.Errorf("cilium egress policy has no bounded egress rules")
	}
	var fqdnNames []string
	hasDNS := false
	gatewayRules := 0
	for _, raw := range egress {
		rule, _ := raw.(map[string]any)
		for _, forbidden := range []string{"toEntities", "toCIDR", "toCIDRSet", "toServices", "toGroups", "toNodes"} {
			if value, ok := rule[forbidden]; ok && value != nil {
				return fmt.Errorf("cilium egress policy contains forbidden %s access", forbidden)
			}
		}
		fqdns, hasFQDNs, _ := unstructured.NestedSlice(rule, "toFQDNs")
		endpoints, hasEndpoints, _ := unstructured.NestedSlice(rule, "toEndpoints")
		switch {
		case hasFQDNs:
			if hasEndpoints || len(fqdns) == 0 || !ruleUsesOnlyPort(rule, "443") {
				return fmt.Errorf("cilium FQDN egress must use only TCP port 443")
			}
			for _, fqdnRaw := range fqdns {
				fqdn, _ := fqdnRaw.(map[string]any)
				name, hasName, _ := unstructured.NestedString(fqdn, "matchName")
				if !hasName || len(fqdn) != 1 || !validPublicHostname(name) {
					return fmt.Errorf("cilium FQDN egress must contain exact public matchName entries only")
				}
				fqdnNames = append(fqdnNames, strings.ToLower(name))
			}
		case hasEndpoints:
			if isDNSRule(rule, endpoints) {
				hasDNS = true
			} else if isGatewayRule(rule, endpoints, annotations) {
				gatewayRules++
			} else {
				return fmt.Errorf("cilium egress policy contains an unexpected endpoint destination")
			}
		default:
			return fmt.Errorf("cilium egress policy contains an unbounded egress rule")
		}
	}
	if !hasDNS || len(fqdnNames) == 0 {
		return fmt.Errorf("cilium egress policy must contain DNS and exact-host HTTPS rules")
	}
	gatewayEnabled := annotations["prow-ai-dashboard/model-gateway-enabled"] == "true"
	if gatewayEnabled && gatewayRules != 1 || !gatewayEnabled && gatewayRules != 0 {
		return fmt.Errorf("cilium egress gateway rule does not match the namespace contract")
	}
	actualHash := executionPolicyHash(fqdnNames, annotations)
	if actualHash != expectedHash {
		return fmt.Errorf("cilium egress destinations do not match the namespace policy hash")
	}
	return nil
}

func ruleUsesOnlyPort(rule map[string]any, want string) bool {
	toPorts, found, _ := unstructured.NestedSlice(rule, "toPorts")
	if !found || len(toPorts) != 1 {
		return false
	}
	entry, _ := toPorts[0].(map[string]any)
	ports, found, _ := unstructured.NestedSlice(entry, "ports")
	if !found || len(ports) != 1 {
		return false
	}
	port, _ := ports[0].(map[string]any)
	value, _, _ := unstructured.NestedString(port, "port")
	protocol, _, _ := unstructured.NestedString(port, "protocol")
	return value == want && protocol == "TCP"
}

func isDNSRule(rule map[string]any, endpoints []any) bool {
	if !ruleUsesOnlyPortOrAny(rule, "53") || len(endpoints) != 1 {
		return false
	}
	endpoint, _ := endpoints[0].(map[string]any)
	labels, _, _ := unstructured.NestedStringMap(endpoint, "matchLabels")
	if labels["k8s:io.kubernetes.pod.namespace"] != "kube-system" || labels["k8s:k8s-app"] != "kube-dns" {
		return false
	}
	toPorts, _, _ := unstructured.NestedSlice(rule, "toPorts")
	entry, _ := toPorts[0].(map[string]any)
	dnsRules, found, _ := unstructured.NestedSlice(entry, "rules", "dns")
	if !found || len(dnsRules) != 1 {
		return false
	}
	dns, _ := dnsRules[0].(map[string]any)
	pattern, _, _ := unstructured.NestedString(dns, "matchPattern")
	return pattern == "*" && len(dns) == 1
}

func ruleUsesOnlyPortOrAny(rule map[string]any, want string) bool {
	toPorts, found, _ := unstructured.NestedSlice(rule, "toPorts")
	if !found || len(toPorts) != 1 {
		return false
	}
	entry, _ := toPorts[0].(map[string]any)
	ports, found, _ := unstructured.NestedSlice(entry, "ports")
	if !found || len(ports) != 1 {
		return false
	}
	port, _ := ports[0].(map[string]any)
	value, _, _ := unstructured.NestedString(port, "port")
	protocol, _, _ := unstructured.NestedString(port, "protocol")
	return value == want && (protocol == "ANY" || protocol == "UDP" || protocol == "TCP")
}

func isGatewayRule(rule map[string]any, endpoints []any, annotations map[string]string) bool {
	if annotations["prow-ai-dashboard/model-gateway-enabled"] != "true" || len(endpoints) != 1 {
		return false
	}
	endpoint, _ := endpoints[0].(map[string]any)
	labels, _, _ := unstructured.NestedStringMap(endpoint, "matchLabels")
	if labels["k8s:io.kubernetes.pod.namespace"] != annotations["prow-ai-dashboard/model-gateway-namespace"] || labels["k8s:app.kubernetes.io/name"] != annotations["prow-ai-dashboard/model-gateway-name"] || labels["k8s:app.kubernetes.io/instance"] != annotations["prow-ai-dashboard/platform-release"] || labels["k8s:app.kubernetes.io/component"] != "model-gateway" {
		return false
	}
	toPorts, found, _ := unstructured.NestedSlice(rule, "toPorts")
	if !found || len(toPorts) != 1 {
		return false
	}
	entry, _ := toPorts[0].(map[string]any)
	ports, found, _ := unstructured.NestedSlice(entry, "ports")
	if !found || len(ports) != 1 {
		return false
	}
	port, _ := ports[0].(map[string]any)
	value, _, _ := unstructured.NestedString(port, "port")
	protocol, _, _ := unstructured.NestedString(port, "protocol")
	return value == annotations["prow-ai-dashboard/model-gateway-target-port"] && protocol == "TCP"
}

func validPublicHostname(value string) bool {
	value = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value), "."))
	return publicHostPattern.MatchString(value) && net.ParseIP(value) == nil && value != "localhost" && !strings.HasSuffix(value, ".svc") && !strings.HasSuffix(value, ".cluster.local") && !strings.HasSuffix(value, ".local")
}

func executionPolicyHash(names []string, annotations map[string]string) string {
	gatewayEnabled := annotations["prow-ai-dashboard/model-gateway-enabled"] == "true"
	gatewayNamespace, gatewayName, platformRelease, targetPort := "", "", "", ""
	if gatewayEnabled {
		gatewayNamespace = annotations["prow-ai-dashboard/model-gateway-namespace"]
		gatewayName = annotations["prow-ai-dashboard/model-gateway-name"]
		platformRelease = annotations["prow-ai-dashboard/platform-release"]
		targetPort = annotations["prow-ai-dashboard/model-gateway-target-port"]
	}
	data, _ := json.Marshal([]any{names, gatewayEnabled, gatewayNamespace, gatewayName, platformRelease, targetPort})
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func checkWorkloadServiceAccount(ctx context.Context, add func(string, KubernetesDoctorStatus, string, string), cluster clusterReader, fix doctorFixRuntimeValues, releaseExists bool) {
	name := strings.TrimSpace(fix.WorkloadServiceAccount.Name)
	if name == "" {
		add("Agent Sandbox workload ServiceAccount", KubernetesDoctorFail, "workload ServiceAccount name is empty", "Select the tokenless ServiceAccount created by the platform bundle.")
		return
	}
	if _, err := cluster.Get(ctx, serviceAccountsGVR, fix.Namespace, name); err != nil {
		switch {
		case apierrors.IsForbidden(err):
			add("Agent Sandbox workload ServiceAccount", KubernetesDoctorFail, "RBAC forbids reading the configured ServiceAccount", "Restore metadata read access to the execution namespace.")
		case apierrors.IsNotFound(err) && fix.WorkloadServiceAccount.Create && !releaseExists:
			add("Agent Sandbox workload ServiceAccount", KubernetesDoctorPass, "ServiceAccount is owned by the planned application chart render", "")
		default:
			add("Agent Sandbox workload ServiceAccount", KubernetesDoctorFail, "configured ServiceAccount is unavailable: "+err.Error(), "Install the owning platform or application resource and rerun doctor.")
		}
		return
	}
	add("Agent Sandbox workload ServiceAccount", KubernetesDoctorPass, "tokenless workload ServiceAccount exists", "")
}

func checkActiveSandboxes(ctx context.Context, add func(string, KubernetesDoctorStatus, string, string), cluster clusterReader, namespace, release string) {
	namespaceObject, err := cluster.Get(ctx, namespacesGVR, "", namespace)
	if err != nil {
		add("active Sandboxes", KubernetesDoctorFail, "execution namespace metadata is unavailable: "+err.Error(), "Restore read access to the release-dedicated execution namespace.")
		return
	}
	if namespaceObject.GetLabels()["prow-ai-dashboard/release"] != release {
		add("active Sandboxes", KubernetesDoctorFail, "execution namespace is not labeled for release "+release, "Use the release-dedicated namespace created by the platform bundle.")
		return
	}
	sandboxes, err := cluster.List(ctx, sandboxesGVR, namespace, "")
	if err != nil {
		add("active Sandboxes", KubernetesDoctorFail, "Sandbox objects are unavailable: "+err.Error(), "Restore read access to the execution namespace.")
		return
	}
	var active []string
	for _, sandbox := range sandboxes.Items {
		active = append(active, sandbox.GetName())
	}
	if len(active) > 0 {
		sort.Strings(active)
		add("active Sandboxes", KubernetesDoctorFail, "release-dedicated execution namespace still has Sandbox objects: "+strings.Join(active, ", "), "Wait for bounded cleanup or investigate the stale workload before deployment.")
		return
	}
	add("active Sandboxes", KubernetesDoctorPass, "no Sandbox object remains in the release-dedicated execution namespace", "")
}

func checkGateway(ctx context.Context, add func(string, KubernetesDoctorStatus, string, string), cluster clusterReader, applicationNamespace string, fix doctorFixRuntimeValues, binding map[string]string) {
	provider := fix.ModelProvider
	parsed, err := url.Parse(provider.Endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" {
		add("model gateway", KubernetesDoctorFail, "provider endpoint is not a valid HTTPS URL", "Configure the reviewed encrypted endpoint.")
		return
	}
	if provider.CredentialMode != "gateway" {
		add("model gateway", KubernetesDoctorUnverified, "direct provider mode does not use an in-cluster gateway", "Confirm the external provider and Secret-manager contract separately.")
		return
	}
	if binding == nil || binding["modelGatewayEnabled"] != "true" {
		add("model gateway", KubernetesDoctorFail, "the retained platform binding does not enable the configured gateway", "Restore the platform binding before using gateway mode.")
		return
	}
	service, namespace, internal := kubernetesServiceHost(parsed.Hostname())
	if internal {
		checkGatewayResources(ctx, add, cluster, namespace, service, "", binding)
		return
	}
	if !provider.PublicCAPrivateDNS {
		add("model gateway", KubernetesDoctorFail, "gateway mode requires an in-cluster Service or explicit publicCAPrivateDNS acknowledgement", "Use the reviewed gateway trust contract without weakening TLS.")
		return
	}
	selector := "app.kubernetes.io/part-of=prow-ai-dashboard-platform,app.kubernetes.io/component=model-gateway"
	services, err := cluster.List(ctx, servicesGVR, applicationNamespace, selector)
	if err != nil {
		add("model gateway Service", KubernetesDoctorFail, "platform gateway Services are unreadable: "+err.Error(), "Restore LIST access to the application namespace.")
		return
	}
	var matches []string
	for _, candidate := range services.Items {
		if strings.EqualFold(candidate.GetAnnotations()["prow-ai-dashboard/model-gateway-host"], parsed.Hostname()) {
			matches = append(matches, candidate.GetName())
		}
	}
	if len(matches) != 1 {
		add("model gateway Service", KubernetesDoctorFail, fmt.Sprintf("expected one platform gateway for host %s, found %d", parsed.Hostname(), len(matches)), "Install the platform bundle in the application namespace with the reviewed public host.")
		return
	}
	checkGatewayResources(ctx, add, cluster, applicationNamespace, matches[0], parsed.Hostname(), binding)
	add("model gateway private DNS", KubernetesDoctorUnverified, "resource metadata cannot prove that the public hostname privately resolves to the selected ClusterIP Service", "Verify private DNS resolution from the secure runtime during real release acceptance.")
}

func checkGatewayResources(ctx context.Context, add func(string, KubernetesDoctorStatus, string, string), cluster clusterReader, namespace, service, expectedHost string, binding map[string]string) {
	gatewayService, err := cluster.Get(ctx, servicesGVR, namespace, service)
	if err != nil {
		add("model gateway Service", KubernetesDoctorFail, "Service is unavailable: "+err.Error(), "Install or repair the platform gateway.")
		return
	}
	if expectedHost != "" && !strings.EqualFold(gatewayService.GetAnnotations()["prow-ai-dashboard/model-gateway-host"], expectedHost) {
		add("model gateway Service", KubernetesDoctorFail, "Service host annotation does not match "+expectedHost, "Upgrade the platform bundle with the reviewed public gateway host.")
		return
	}
	if serviceType, _, _ := unstructured.NestedString(gatewayService.Object, "spec", "type"); serviceType != "ClusterIP" {
		add("model gateway Service", KubernetesDoctorFail, "gateway Service type is "+serviceType+", not ClusterIP", "Keep the platform gateway private to the cluster or private DNS path.")
	} else {
		add("model gateway Service", KubernetesDoctorPass, "gateway ClusterIP Service exists", "")
	}
	slices, err := cluster.List(ctx, endpointSlicesGVR, namespace, "kubernetes.io/service-name="+service)
	if err != nil {
		add("model gateway endpoints", KubernetesDoctorFail, "gateway EndpointSlices are unreadable: "+err.Error(), "Restore LIST access to gateway EndpointSlices.")
		return
	}
	if !hasReadyEndpoint(slices) {
		add("model gateway endpoints", KubernetesDoctorFail, "gateway has no ready EndpointSlice endpoint", "Restore gateway Deployment readiness.")
		return
	}
	add("model gateway endpoints", KubernetesDoctorPass, "gateway has a ready endpoint", "")
	selector, _, _ := unstructured.NestedStringMap(gatewayService.Object, "spec", "selector")
	deployments, err := cluster.List(ctx, deploymentsGVR, namespace, selectorString(selector))
	if err != nil {
		add("model gateway Deployment", KubernetesDoctorFail, "gateway Deployments are unreadable: "+err.Error(), "Restore LIST access to the gateway namespace.")
		return
	}
	if len(deployments.Items) != 1 {
		add("model gateway Deployment", KubernetesDoctorFail, "exactly one selected gateway Deployment was not found", "Correct the platform gateway Service selector and Deployment labels.")
		return
	}
	deployment := &deployments.Items[0]
	if expectedHost != "" && !strings.EqualFold(deployment.GetAnnotations()["prow-ai-dashboard/model-gateway-host"], expectedHost) {
		add("model gateway Deployment", KubernetesDoctorFail, "Deployment host annotation does not match "+expectedHost, "Upgrade the platform bundle with the reviewed public gateway host.")
		return
	}
	available, _, _ := unstructured.NestedInt64(deployment.Object, "status", "availableReplicas")
	if available < 1 {
		add("model gateway Deployment", KubernetesDoctorFail, "gateway Deployment has no available replicas", "Restore gateway readiness.")
		return
	}
	add("model gateway Deployment", KubernetesDoctorPass, "gateway Deployment is available", "")
	image := containerImage(deployment, "gateway")
	if !strings.Contains(image, "@sha256:") {
		add("model gateway image", KubernetesDoctorFail, "gateway image is not pinned by digest", "Use a reviewed published gateway image digest.")
	} else {
		add("model gateway image", KubernetesDoctorPass, "gateway image is pinned by digest", "")
	}
	if !gatewayDeploymentMatchesBinding(deployment, binding) {
		add("model gateway Deployment", KubernetesDoctorFail, "gateway Deployment does not match the retained upstream and policy binding", "Restore the platform chart gateway Deployment.")
		return
	}
	checkGatewayNetworkPolicies(ctx, add, cluster, namespace, deployment, binding)
	tlsSecretName := strings.TrimSpace(deployment.GetAnnotations()[modelGatewayTLSSecretAnnotation])
	if tlsSecretName == "" {
		add("model gateway TLS", KubernetesDoctorFail, "gateway Deployment lacks the explicit TLS Secret annotation", "Set "+modelGatewayTLSSecretAnnotation+" to the mounted existing TLS Secret name.")
		return
	}
	tlsSecrets, otherSecrets, tlsMounted := deploymentSecretReferences(deployment, tlsSecretName)
	tlsSet := map[string]struct{}{}
	for _, name := range tlsSecrets {
		tlsSet[name] = struct{}{}
	}
	tlsMetadataExists := false
	allSecrets := append(append([]string(nil), tlsSecrets...), otherSecrets...)
	for _, name := range uniqueStrings(allSecrets) {
		if _, err := cluster.SecretMetadata(ctx, namespace, name); err != nil {
			add("model gateway Secret", KubernetesDoctorFail, fmt.Sprintf("Secret metadata %s/%s is unavailable", namespace, name), "Have the Secret manager provision the referenced name.")
			continue
		}
		if _, ok := tlsSet[name]; ok {
			tlsMetadataExists = true
		}
		add("model gateway Secret", KubernetesDoctorPass, fmt.Sprintf("Secret metadata %s/%s exists; key names and values were not read", namespace, name), "")
	}
	switch {
	case len(tlsSecrets) == 0 || !tlsMounted:
		add("model gateway TLS", KubernetesDoctorFail, "annotated TLS Secret is not mounted read-only by the gateway", "Mount the exact annotated existing Secret through a read-only volume.")
	case !tlsMetadataExists:
		add("model gateway TLS", KubernetesDoctorFail, "no referenced TLS Secret metadata could be read", "Have the Secret manager provision the TLS Secret and restore metadata read access.")
	default:
		add("model gateway TLS", KubernetesDoctorPass, "gateway Deployment references a TLS Secret whose metadata exists", "")
	}
	add("model gateway TLS identity", KubernetesDoctorUnverified, "metadata cannot prove certificate SAN, trust chain, private-key pairing, reload behavior, or a successful TLS handshake", "Validate the public-CA/private-DNS certificate path in real release acceptance.")
}

func gatewayDeploymentMatchesBinding(deployment *unstructured.Unstructured, binding map[string]string) bool {
	if binding == nil {
		return false
	}
	annotations := deployment.GetAnnotations()
	if annotations["prow-ai-dashboard/model-gateway-policy-sha256"] != binding["modelGatewayPolicySHA256"] || annotations["prow-ai-dashboard/model-gateway-execution-namespace"] != binding["modelGatewayExecutionNamespace"] || !strings.EqualFold(annotations["prow-ai-dashboard/model-gateway-upstream-host"], binding["modelGatewayUpstreamHost"]) || annotations["prow-ai-dashboard/model-gateway-target-port"] != binding["modelGatewayTargetPort"] {
		return false
	}
	containers, _, _ := unstructured.NestedSlice(deployment.Object, "spec", "template", "spec", "containers")
	for _, raw := range containers {
		container, _ := raw.(map[string]any)
		name, _, _ := unstructured.NestedString(container, "name")
		if name != "gateway" {
			continue
		}
		env, _, _ := unstructured.NestedSlice(container, "env")
		for _, envRaw := range env {
			envVar, _ := envRaw.(map[string]any)
			envName, _, _ := unstructured.NestedString(envVar, "name")
			if envName != "UPSTREAM_URL" {
				continue
			}
			value, _, _ := unstructured.NestedString(envVar, "value")
			parsed, err := url.Parse(value)
			return err == nil && parsed.Scheme == "https" && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == "" && strings.EqualFold(parsed.Hostname(), binding["modelGatewayUpstreamHost"])
		}
	}
	return false
}

func checkGatewayNetworkPolicies(ctx context.Context, add func(string, KubernetesDoctorStatus, string, string), cluster clusterReader, namespace string, deployment *unstructured.Unstructured, binding map[string]string) {
	podLabels, _, _ := unstructured.NestedStringMap(deployment.Object, "spec", "template", "metadata", "labels")
	annotations := deployment.GetAnnotations()
	defaultDenyName := annotations["prow-ai-dashboard/model-gateway-default-deny-policy"]
	ciliumName := annotations["prow-ai-dashboard/model-gateway-cilium-policy"]
	if defaultDenyName == "" || ciliumName == "" || annotations["prow-ai-dashboard/model-gateway-policy-sha256"] == "" {
		add("model gateway network policy", KubernetesDoctorFail, "gateway Deployment lacks the stable network-policy contract annotations", "Restore the platform chart gateway Deployment.")
		return
	}
	policies, err := cluster.List(ctx, networkPoliciesGVR, namespace, "")
	if err != nil {
		add("model gateway network policy", KubernetesDoctorFail, "gateway NetworkPolicies are unreadable: "+err.Error(), "Restore LIST access to the gateway namespace.")
		return
	}
	var selected []unstructured.Unstructured
	for _, policy := range policies.Items {
		selector, found, _ := unstructured.NestedMap(policy.Object, "spec", "podSelector")
		if !found || labelSelectorMaySelect(selector, podLabels) {
			selected = append(selected, policy)
		}
	}
	if len(selected) != 1 || selected[0].GetName() != defaultDenyName || !hasNoAllowRules(&selected[0]) {
		add("model gateway network policy", KubernetesDoctorFail, "gateway Pods must be selected by exactly the release-owned default-deny NetworkPolicy", "Remove stale or additional policies and restore the platform chart boundary.")
		return
	}
	labels := selected[0].GetLabels()
	if labels["app.kubernetes.io/part-of"] != "prow-ai-dashboard-platform" || labels["app.kubernetes.io/component"] != "model-gateway" || labels["app.kubernetes.io/instance"] != deployment.GetLabels()["app.kubernetes.io/instance"] {
		add("model gateway network policy", KubernetesDoctorFail, "gateway default-deny policy lacks the exact platform release labels", "Restore the chart-owned gateway policy.")
		return
	}
	served, err := cluster.HasResource(ctx, ciliumPoliciesGVR)
	if err != nil || !served {
		add("model gateway network policy", KubernetesDoctorFail, "the CiliumNetworkPolicy API is unavailable for the gateway", "Restore the supported FQDN-aware Cilium network-policy backend.")
		return
	}
	ciliumPolicies, err := cluster.List(ctx, ciliumPoliciesGVR, namespace, "")
	if err != nil {
		add("model gateway network policy", KubernetesDoctorFail, "gateway Cilium policies are unreadable: "+err.Error(), "Restore LIST access to the gateway namespace.")
		return
	}
	var selectedCilium []unstructured.Unstructured
	for _, policy := range ciliumPolicies.Items {
		selector, found, _ := unstructured.NestedMap(policy.Object, "spec", "endpointSelector")
		if !found || labelSelectorMaySelect(selector, podLabels) {
			selectedCilium = append(selectedCilium, policy)
		}
	}
	if len(selectedCilium) != 1 || selectedCilium[0].GetName() != ciliumName {
		add("model gateway network policy", KubernetesDoctorFail, "gateway Pods must be selected by exactly the release-owned Cilium policy", "Remove stale or additional Cilium policies and restore the platform chart boundary.")
		return
	}
	if err := validateGatewayCiliumPolicy(&selectedCilium[0], deployment, podLabels, binding); err != nil {
		add("model gateway network policy", KubernetesDoctorFail, err.Error(), "Restore the exact platform-chart gateway policy.")
		return
	}
	clusterwide, err := cluster.List(ctx, ciliumClusterwidePoliciesGVR, "", "")
	if err != nil {
		add("model gateway network policy", KubernetesDoctorFail, "cluster-wide Cilium policies are unreadable: "+err.Error(), "Restore LIST access to cluster-wide Cilium policy metadata.")
		return
	}
	for _, policy := range clusterwide.Items {
		selector, found, _ := unstructured.NestedMap(policy.Object, "spec", "endpointSelector")
		if !found || labelSelectorMaySelect(selector, podLabels) {
			add("model gateway network policy", KubernetesDoctorFail, "cluster-wide Cilium policy "+policy.GetName()+" may select the credential-bearing gateway", "Remove or narrowly scope the cluster-wide policy.")
			return
		}
	}
	add("model gateway network policy", KubernetesDoctorPass, "gateway default-deny and exact-host Cilium policies are the only policies selecting the gateway", "")
}

func validateGatewayCiliumPolicy(policy, deployment *unstructured.Unstructured, podLabels map[string]string, binding map[string]string) error {
	annotations := deployment.GetAnnotations()
	labels := policy.GetLabels()
	if labels["app.kubernetes.io/part-of"] != "prow-ai-dashboard-platform" || labels["app.kubernetes.io/component"] != "model-gateway" || labels["app.kubernetes.io/instance"] != deployment.GetLabels()["app.kubernetes.io/instance"] {
		return fmt.Errorf("gateway Cilium policy lacks the exact platform release labels")
	}
	if binding == nil || policy.GetAnnotations()["prow-ai-dashboard/model-gateway-policy-sha256"] != binding["modelGatewayPolicySHA256"] || annotations["prow-ai-dashboard/model-gateway-policy-sha256"] != binding["modelGatewayPolicySHA256"] {
		return fmt.Errorf("gateway Cilium policy hash does not match the Deployment contract")
	}
	selector, found, _ := unstructured.NestedMap(policy.Object, "spec", "endpointSelector")
	expectedSelectorLabels := map[string]string{
		"app.kubernetes.io/name":      podLabels["app.kubernetes.io/name"],
		"app.kubernetes.io/instance":  podLabels["app.kubernetes.io/instance"],
		"app.kubernetes.io/component": podLabels["app.kubernetes.io/component"],
	}
	if !found || !labelSelectorExactlyMatches(selector, expectedSelectorLabels) {
		return fmt.Errorf("gateway Cilium policy endpoint selector does not match the gateway Pod labels")
	}
	ingress, found, _ := unstructured.NestedSlice(policy.Object, "spec", "ingress")
	if !found || len(ingress) != 1 || !gatewayIngressRuleValid(ingress[0], binding) {
		return fmt.Errorf("gateway Cilium policy ingress does not match the execution namespace and target port")
	}
	egress, found, _ := unstructured.NestedSlice(policy.Object, "spec", "egress")
	if !found || len(egress) != 2 {
		return fmt.Errorf("gateway Cilium policy must contain only DNS and exact upstream-host egress")
	}
	hasDNS := false
	hasUpstream := false
	for _, raw := range egress {
		rule, _ := raw.(map[string]any)
		for _, forbidden := range []string{"toEntities", "toCIDR", "toCIDRSet", "toServices", "toGroups", "toNodes"} {
			if value, ok := rule[forbidden]; ok && value != nil {
				return fmt.Errorf("gateway Cilium policy contains forbidden %s access", forbidden)
			}
		}
		endpoints, hasEndpoints, _ := unstructured.NestedSlice(rule, "toEndpoints")
		fqdns, hasFQDNs, _ := unstructured.NestedSlice(rule, "toFQDNs")
		switch {
		case hasEndpoints && isDNSRule(rule, endpoints):
			hasDNS = true
		case hasFQDNs && !hasEndpoints && len(fqdns) == 1 && ruleUsesOnlyPort(rule, "443"):
			fqdn, _ := fqdns[0].(map[string]any)
			name, ok, _ := unstructured.NestedString(fqdn, "matchName")
			if !ok || len(fqdn) != 1 || !strings.EqualFold(name, binding["modelGatewayUpstreamHost"]) || !validPublicHostname(name) {
				return fmt.Errorf("gateway Cilium policy upstream FQDN does not match the Deployment contract")
			}
			hasUpstream = true
		default:
			return fmt.Errorf("gateway Cilium policy contains an unexpected egress rule")
		}
	}
	if !hasDNS || !hasUpstream {
		return fmt.Errorf("gateway Cilium policy lacks DNS or exact upstream-host egress")
	}
	actualHash := gatewayPolicyHash(binding["modelGatewayExecutionNamespace"], binding["modelGatewayUpstreamHost"], binding["modelGatewayTargetPort"])
	if actualHash != binding["modelGatewayPolicySHA256"] {
		return fmt.Errorf("gateway Cilium policy contract hash is invalid")
	}
	return nil
}

func gatewayIngressRuleValid(raw any, binding map[string]string) bool {
	rule, _ := raw.(map[string]any)
	from, found, _ := unstructured.NestedSlice(rule, "fromEndpoints")
	if !found || len(from) != 1 {
		return false
	}
	endpoint, _ := from[0].(map[string]any)
	labels, _, _ := unstructured.NestedStringMap(endpoint, "matchLabels")
	if len(labels) != 1 || labels["k8s:io.kubernetes.pod.namespace"] != binding["modelGatewayExecutionNamespace"] {
		return false
	}
	return len(rule) == 2 && ruleUsesOnlyPort(rule, binding["modelGatewayTargetPort"])
}

func labelSelectorMaySelect(selector map[string]any, labels map[string]string) bool {
	matchLabels, _, _ := unstructured.NestedStringMap(selector, "matchLabels")
	for key, want := range matchLabels {
		if got, ok := labels[key]; ok && got != want {
			return false
		}
		if _, ok := labels[key]; !ok {
			return false
		}
	}
	expressions, found, _ := unstructured.NestedSlice(selector, "matchExpressions")
	if found && len(expressions) > 0 {
		return true
	}
	return true
}

func labelSelectorExactlyMatches(selector map[string]any, labels map[string]string) bool {
	expressions, found, _ := unstructured.NestedSlice(selector, "matchExpressions")
	if found && len(expressions) > 0 {
		return false
	}
	matchLabels, found, _ := unstructured.NestedStringMap(selector, "matchLabels")
	if !found || len(matchLabels) != len(labels) {
		return false
	}
	for key, value := range labels {
		if matchLabels[key] != value {
			return false
		}
	}
	return true
}

func gatewayPolicyHash(executionNamespace, upstreamHost, targetPort string) string {
	data, _ := json.Marshal([]string{executionNamespace, upstreamHost, targetPort})
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func containerImage(object *unstructured.Unstructured, name string) string {
	containers, _, _ := unstructured.NestedSlice(object.Object, "spec", "template", "spec", "containers")
	for _, raw := range containers {
		container, _ := raw.(map[string]any)
		containerName, _, _ := unstructured.NestedString(container, "name")
		if containerName == name || (name == "" && containerName != "") {
			image, _, _ := unstructured.NestedString(container, "image")
			return image
		}
	}
	return ""
}

func deploymentSecretReferences(deployment *unstructured.Unstructured, tlsSecretName string) (tlsSecrets, otherSecrets []string, tlsMounted bool) {
	volumeSecrets := map[string]string{}
	volumes, _, _ := unstructured.NestedSlice(deployment.Object, "spec", "template", "spec", "volumes")
	for _, raw := range volumes {
		volume, _ := raw.(map[string]any)
		volumeName, _, _ := unstructured.NestedString(volume, "name")
		secretName, _, _ := unstructured.NestedString(volume, "secret", "secretName")
		if volumeName == "" || secretName == "" {
			continue
		}
		volumeSecrets[volumeName] = secretName
		if secretName == tlsSecretName {
			tlsSecrets = append(tlsSecrets, secretName)
		} else {
			otherSecrets = append(otherSecrets, secretName)
		}
	}
	containers, _, _ := unstructured.NestedSlice(deployment.Object, "spec", "template", "spec", "containers")
	for _, raw := range containers {
		container, _ := raw.(map[string]any)
		containerName, _, _ := unstructured.NestedString(container, "name")
		mounts, _, _ := unstructured.NestedSlice(container, "volumeMounts")
		for _, mountRaw := range mounts {
			mount, _ := mountRaw.(map[string]any)
			volumeName, _, _ := unstructured.NestedString(mount, "name")
			readOnly, _, _ := unstructured.NestedBool(mount, "readOnly")
			if containerName == "gateway" && volumeSecrets[volumeName] == tlsSecretName && readOnly {
				tlsMounted = true
			}
		}
		env, _, _ := unstructured.NestedSlice(container, "env")
		for _, envRaw := range env {
			envVar, _ := envRaw.(map[string]any)
			name, _, _ := unstructured.NestedString(envVar, "valueFrom", "secretKeyRef", "name")
			if name != "" {
				otherSecrets = append(otherSecrets, name)
			}
		}
	}
	return uniqueStrings(tlsSecrets), uniqueStrings(otherSecrets), tlsMounted
}

func uniqueStrings(values []string) []string {
	set := map[string]struct{}{}
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			set[value] = struct{}{}
		}
	}
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func hasReadyEndpoint(slices *unstructured.UnstructuredList) bool {
	if slices == nil {
		return false
	}
	for _, slice := range slices.Items {
		endpoints, _, _ := unstructured.NestedSlice(slice.Object, "endpoints")
		for _, raw := range endpoints {
			endpoint, _ := raw.(map[string]any)
			ready, found, _ := unstructured.NestedBool(endpoint, "conditions", "ready")
			if !found || ready {
				return true
			}
		}
	}
	return false
}

func positiveQuantity(value string) bool {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	return err == nil && parsed > 0
}

func nestedBool(object map[string]any, fields ...string) bool {
	value, _, _ := unstructured.NestedBool(object, fields...)
	return value
}

func nodeReady(node *unstructured.Unstructured) bool {
	conditions, _, _ := unstructured.NestedSlice(node.Object, "status", "conditions")
	for _, raw := range conditions {
		condition, _ := raw.(map[string]any)
		typeName, _, _ := unstructured.NestedString(condition, "type")
		status, _, _ := unstructured.NestedString(condition, "status")
		if typeName == "Ready" {
			return status == "True"
		}
	}
	return false
}

func labelsMatch(labels, selector map[string]string) bool {
	for key, value := range selector {
		if labels[key] != value {
			return false
		}
	}
	return true
}

func selectorString(selector map[string]string) string {
	keys := make([]string, 0, len(selector))
	for key := range selector {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+selector[key])
	}
	return strings.Join(parts, ",")
}

func kubernetesServiceHost(host string) (service, namespace string, ok bool) {
	parts := strings.Split(strings.TrimSuffix(strings.TrimSpace(host), "."), ".")
	if len(parts) < 3 || parts[2] != "svc" {
		return "", "", false
	}
	return parts[0], parts[1], parts[0] != "" && parts[1] != ""
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func boundedDoctorText(value string, limit int) string {
	value = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' || r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > limit {
		return value[:limit] + "..."
	}
	return value
}

const maxKubernetesDoctorOutputBytes = 64 << 10

// WriteKubernetesDoctorReport prints bounded text and the required final summary.
func WriteKubernetesDoctorReport(out io.Writer, report KubernetesDoctorReport) error {
	var body bytes.Buffer
	shown := 0
	for _, check := range report.Checks {
		var item bytes.Buffer
		fmt.Fprintf(&item, "[%s] %s: %s\n", check.Status, boundedDoctorText(check.Name, 256), boundedDoctorText(check.Detail, 4096))
		if check.Action != "" {
			fmt.Fprintf(&item, "  next: %s\n", boundedDoctorText(check.Action, 2048))
		}
		if body.Len()+item.Len() > maxKubernetesDoctorOutputBytes-2048 {
			break
		}
		body.Write(item.Bytes())
		shown++
	}
	omitted := len(report.Checks) - shown
	failures, warnings, unverified := report.counts()
	if omitted > 0 {
		fmt.Fprintf(&body, "[warn] report output: %d additional checks omitted by the output budget\n", omitted)
		warnings++
	}
	status := "pass"
	if failures > 0 {
		status = "fail"
	}
	summary := fmt.Sprintf("kubernetes_doctor=%s\nblocking_checks=%d\nwarnings=%d\nunverified_assumptions=%d\n", status, failures, warnings, unverified)
	if _, err := out.Write(body.Bytes()); err != nil {
		return err
	}
	_, err := io.WriteString(out, summary)
	return err
}

type doctorImageValues struct {
	Repository string `yaml:"repository"`
	Tag        string `yaml:"tag"`
	Digest     string `yaml:"digest"`
}

type doctorProviderValues struct {
	CredentialMode     string `yaml:"credentialMode"`
	API                string `yaml:"api"`
	Endpoint           string `yaml:"endpoint"`
	Model              string `yaml:"model"`
	PublicCAPrivateDNS bool   `yaml:"publicCAPrivateDNS"`
	Auth               struct {
		Type           string `yaml:"type"`
		ExistingSecret string `yaml:"existingSecret"`
		TokenKey       string `yaml:"tokenKey"`
	} `yaml:"auth"`
}

type doctorFixRuntimeValues struct {
	Enabled                bool              `yaml:"enabled"`
	Namespace              string            `yaml:"namespace"`
	RuntimeClassName       string            `yaml:"runtimeClassName"`
	Image                  doctorImageValues `yaml:"image"`
	DashboardImage         doctorImageValues `yaml:"dashboardImage"`
	WorkloadServiceAccount struct {
		Create bool   `yaml:"create"`
		Name   string `yaml:"name"`
	} `yaml:"workloadServiceAccount"`
	ModelProvider doctorProviderValues `yaml:"modelProvider"`
}

type kubernetesDoctorValues struct {
	Global struct {
		ImageTag string `yaml:"imageTag"`
	} `yaml:"global"`
	Image       doctorImageValues `yaml:"image"`
	Mode        string            `yaml:"mode"`
	Persistence struct {
		ExistingClaim string `yaml:"existingClaim"`
		StorageClass  string `yaml:"storageClass"`
		AccessMode    string `yaml:"accessMode"`
	} `yaml:"persistence"`
	AI struct {
		Token                     string `yaml:"token"`
		ExistingSecret            string `yaml:"existingSecret"`
		GitHubReadToken           string `yaml:"githubReadToken"`
		GitHubReadTokenSecretName string `yaml:"githubReadTokenSecretName"`
	} `yaml:"ai"`
	AgentSandbox struct {
		FixRuntime doctorFixRuntimeValues `yaml:"fixRuntime"`
		Analyzer   struct {
			Input struct {
				ExistingClaim string `yaml:"existingClaim"`
			} `yaml:"input"`
		} `yaml:"analyzer"`
		CausalCritic struct {
			Ledger struct {
				ExistingClaim string `yaml:"existingClaim"`
			} `yaml:"ledger"`
		} `yaml:"causalCritic"`
	} `yaml:"agentSandbox"`
	Server struct {
		ExtraEnv []struct {
			Name  string `yaml:"name"`
			Value string `yaml:"value"`
		} `yaml:"extraEnv"`
		Security struct {
			HSTS struct {
				Enabled *bool `yaml:"enabled"`
			} `yaml:"hsts"`
		} `yaml:"security"`
		Development struct {
			AllowInsecureHTTP    bool `yaml:"allowInsecureHTTP"`
			AllowInsecureCookies bool `yaml:"allowInsecureCookies"`
		} `yaml:"development"`
		Actions struct {
			Enabled bool   `yaml:"enabled"`
			Mode    string `yaml:"mode"`
			OAuth   struct {
				RedirectURL    string `yaml:"redirectUrl"`
				ClientSecret   string `yaml:"clientSecret"`
				SessionKey     string `yaml:"sessionKey"`
				BotToken       string `yaml:"botToken"`
				ExistingSecret string `yaml:"existingSecret"`
			} `yaml:"oauth"`
			Proxy struct {
				BotToken       string `yaml:"botToken"`
				Secret         string `yaml:"secret"`
				ExistingSecret string `yaml:"existingSecret"`
			} `yaml:"proxy"`
		} `yaml:"actions"`
		Service struct {
			Type                     string   `yaml:"type"`
			LoadBalancerSourceRanges []string `yaml:"loadBalancerSourceRanges"`
			Internal                 struct {
				Enabled bool `yaml:"enabled"`
			} `yaml:"internal"`
		} `yaml:"service"`
	} `yaml:"server"`
	Ingress struct {
		Enabled bool `yaml:"enabled"`
	} `yaml:"ingress"`
}
