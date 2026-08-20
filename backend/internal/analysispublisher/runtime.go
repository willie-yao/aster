// Package analysispublisher runs deterministic namespace-local snapshot Jobs.
package analysispublisher

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/willie-yao/aster/backend/internal/agentanalysis"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	publishEnv = "PROW_AI_ANALYSIS_PUBLISH_REQUEST_B64"
	cleanupEnv = "PROW_AI_ANALYSIS_CLEANUP_REQUEST_B64"
)

var prefixPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*_$`)

// Resources are the fixed publisher and cleanup Job resources.
type Resources struct {
	CPURequest       string
	CPULimit         string
	MemoryRequest    string
	MemoryLimit      string
	EphemeralStorage string
}

// Options configure one private publisher lifecycle.
type Options struct {
	Namespace          string
	Image              string
	InputClaim         string
	ServiceAccountName string
	PollEvery          time.Duration
	Timeout            time.Duration
	Resources          Resources
}

// Result records one completed namespace-local Job.
type Result struct {
	JobName            string
	PodName            string
	Duration           time.Duration
	Publication        agentanalysis.WorkspaceSourceModePolicy
	SourceModePolicies []agentanalysis.WorkspaceSourceMode
}

// Runtime creates and removes one exact bounded Job at a time.
type Runtime struct {
	client kubernetes.Interface
	opts   Options
	now    func() time.Time
}

// NewFromEnv constructs an in-cluster publisher from a reserved environment prefix.
func NewFromEnv(prefix string, expectedTimeout time.Duration) (*Runtime, error) {
	if !prefixPattern.MatchString(prefix) {
		return nil, fmt.Errorf("analysis publisher environment prefix is invalid")
	}
	env := func(name string) string { return strings.TrimSpace(os.Getenv(prefix + name)) }
	poll := 250 * time.Millisecond
	if value := env("POLL_INTERVAL"); value != "" {
		var err error
		poll, err = time.ParseDuration(value)
		if err != nil || poll <= 0 {
			return nil, fmt.Errorf("analysis publisher poll interval is invalid")
		}
	}
	timeout, err := time.ParseDuration(env("TIMEOUT"))
	if err != nil || timeout != expectedTimeout {
		return nil, fmt.Errorf("analysis publisher timeout does not match shadow configuration")
	}
	opts := Options{
		Namespace: env("NAMESPACE"), Image: env("STAGER_IMAGE"), InputClaim: env("STAGER_INPUT_CLAIM"), ServiceAccountName: env("SERVICE_ACCOUNT"),
		PollEvery: poll, Timeout: timeout,
		Resources: Resources{
			CPURequest: env("CPU_REQUEST"), CPULimit: env("CPU_LIMIT"), MemoryRequest: env("MEMORY_REQUEST"),
			MemoryLimit: env("MEMORY_LIMIT"), EphemeralStorage: env("EPHEMERAL_STORAGE_LIMIT"),
		},
	}
	if err := validateOptions(opts); err != nil {
		return nil, err
	}
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("analysis publisher requires in-cluster Kubernetes configuration: %w", err)
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return New(client, opts)
}

// New constructs a publisher with one Kubernetes client.
func New(client kubernetes.Interface, opts Options) (*Runtime, error) {
	if client == nil {
		return nil, fmt.Errorf("analysis publisher Kubernetes client is required")
	}
	if err := validateOptions(opts); err != nil {
		return nil, err
	}
	return &Runtime{client: client, opts: opts, now: time.Now}, nil
}

// Publish writes one exact remote snapshot and removes the publisher Job before returning.
func (r *Runtime) Publish(ctx context.Context, request agentanalysis.WorkspacePublishRequest, executionID string) (Result, error) {
	if err := agentanalysis.ValidateWorkspacePublishRequest(request); err != nil {
		return Result{}, err
	}
	name := jobName("publish", executionID, request.Hash)
	return r.run(ctx, name, "publish", publishEnv, request, request.Stage.ManifestHash)
}

// Cleanup removes one exact leased snapshot and its cleanup Job.
func (r *Runtime) Cleanup(ctx context.Context, request agentanalysis.WorkspaceCleanupRequest, executionID string) (Result, error) {
	if err := agentanalysis.ValidateWorkspaceCleanupRequest(request); err != nil {
		return Result{}, err
	}
	name := jobName("cleanup", executionID, request.Hash)
	return r.run(ctx, name, "cleanup", cleanupEnv, request, request.ManifestHash)
}

func (r *Runtime) run(ctx context.Context, name, purpose, envName string, request any, manifestHash string) (result Result, retErr error) {
	data, err := json.Marshal(request)
	if err != nil {
		return result, err
	}
	if envName == publishEnv && base64.StdEncoding.EncodedLen(len(data)) > agentanalysis.WorkspacePublishEncodedMaxBytes {
		return result, fmt.Errorf("analysis input publish request exceeds the admitted encoded bound")
	}
	started := r.now()
	job := r.job(name, purpose, envName, data, manifestHash)
	created, err := r.client.BatchV1().Jobs(r.opts.Namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		createErr := fmt.Errorf("create analysis input %s Job: %w", purpose, err)
		recoveryCtx, cancelRecovery := context.WithTimeout(context.Background(), 45*time.Second)
		recovered, getErr := r.client.BatchV1().Jobs(r.opts.Namespace).Get(recoveryCtx, name, metav1.GetOptions{})
		cancelRecovery()
		if apierrors.IsNotFound(getErr) {
			return result, createErr
		}
		if getErr != nil {
			return result, errors.Join(createErr, fmt.Errorf("recover analysis input %s Job: %w", purpose, getErr))
		}
		result.JobName = recovered.Name
		identityErr := validateRecoveredJob(job, recovered)
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		cleanupErr := r.deleteJob(cleanupCtx, recovered.Name, recovered.UID)
		cancel()
		result.Duration = r.now().Sub(started)
		return result, errors.Join(createErr, identityErr, cleanupErr)
	}
	result.JobName = created.Name
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		cleanupErr := r.deleteJob(cleanupCtx, created.Name, created.UID)
		if cleanupErr != nil {
			retErr = errors.Join(retErr, cleanupErr)
		}
		result.Duration = r.now().Sub(started)
	}()
	if err := validateRecoveredJob(job, created); err != nil {
		return result, err
	}
	poll := time.NewTicker(r.opts.PollEvery)
	defer poll.Stop()
	for {
		current, err := r.client.BatchV1().Jobs(r.opts.Namespace).Get(ctx, created.Name, metav1.GetOptions{})
		if err != nil {
			return result, fmt.Errorf("read analysis input %s Job: %w", purpose, err)
		}
		if current.Status.Succeeded > 0 {
			break
		}
		if current.Status.Failed > 0 {
			return result, fmt.Errorf("analysis input %s Job failed", purpose)
		}
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case <-poll.C:
		}
	}
	pods, err := r.client.CoreV1().Pods(r.opts.Namespace).List(ctx, metav1.ListOptions{LabelSelector: fields.OneTermEqualSelector("job-name", created.Name).String()})
	if err != nil || len(pods.Items) != 1 {
		return result, fmt.Errorf("analysis input %s Job Pod identity is unavailable", purpose)
	}
	result.PodName = pods.Items[0].Name
	logs, err := r.client.CoreV1().Pods(r.opts.Namespace).GetLogs(result.PodName, &corev1.PodLogOptions{}).DoRaw(ctx)
	if err != nil || len(logs) > 16<<10 {
		return result, fmt.Errorf("analysis input %s Job result is unavailable", purpose)
	}
	if purpose == "publish" {
		var published struct {
			Version            int                                 `json:"version"`
			Status             string                              `json:"status"`
			ManifestHash       string                              `json:"manifest_hash"`
			SourceModePolicies []agentanalysis.WorkspaceSourceMode `json:"source_mode_policies"`
		}
		decoder := json.NewDecoder(bytes.NewReader(logs))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&published); err != nil || published.Version != 2 || published.Status != "published" || published.ManifestHash != manifestHash || !validPublishedSourceModes(published.SourceModePolicies) {
			return result, fmt.Errorf("analysis input publisher result is invalid")
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			return result, fmt.Errorf("analysis input publisher result has trailing data")
		}
		result.SourceModePolicies = published.SourceModePolicies
		result.Publication = published.SourceModePolicies[0].Policy
	} else {
		var cleaned struct {
			Version      int    `json:"version"`
			Status       string `json:"status"`
			ManifestHash string `json:"manifest_hash"`
		}
		decoder := json.NewDecoder(bytes.NewReader(logs))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&cleaned); err != nil || cleaned.Version != 1 || (cleaned.Status != "deleted" && cleaned.Status != "absent") || cleaned.ManifestHash != manifestHash {
			return result, fmt.Errorf("analysis input cleanup result is invalid")
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			return result, fmt.Errorf("analysis input cleanup result has trailing data")
		}
	}
	return result, nil
}

func validPublishedSourceModes(policies []agentanalysis.WorkspaceSourceMode) bool {
	if len(policies) < 1 || len(policies) > agentanalysis.WorkspaceMaxSources {
		return false
	}
	previous := ""
	for _, policy := range policies {
		if len(validation.IsDNS1123Label(policy.SourceID)) > 0 || policy.SourceID <= previous || (policy.Policy != agentanalysis.WorkspaceSourceModePreserve && policy.Policy != agentanalysis.WorkspaceSourceModeIgnoreExecutable) {
			return false
		}
		previous = policy.SourceID
	}
	return true
}

func (r *Runtime) deleteJob(ctx context.Context, name string, uid types.UID) error {
	policy := metav1.DeletePropagationForeground
	grace := int64(0)
	options := metav1.DeleteOptions{PropagationPolicy: &policy, GracePeriodSeconds: &grace}
	if uid != "" {
		options.Preconditions = &metav1.Preconditions{UID: &uid}
	}
	if err := r.client.BatchV1().Jobs(r.opts.Namespace).Delete(ctx, name, options); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete analysis input Job: %w", err)
	}
	poll := time.NewTicker(r.opts.PollEvery)
	defer poll.Stop()
	for {
		job, err := r.client.BatchV1().Jobs(r.opts.Namespace).Get(ctx, name, metav1.GetOptions{})
		jobAbsent := apierrors.IsNotFound(err)
		if err != nil && !jobAbsent {
			return fmt.Errorf("confirm analysis input Job deletion: %w", err)
		}
		if err == nil && uid != "" && job.UID != uid {
			return fmt.Errorf("analysis input Job UID changed during deletion")
		}
		pods, err := r.client.CoreV1().Pods(r.opts.Namespace).List(ctx, metav1.ListOptions{LabelSelector: fields.OneTermEqualSelector("job-name", name).String()})
		if err != nil {
			return err
		}
		if jobAbsent && len(pods.Items) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-poll.C:
		}
	}
}

func validateRecoveredJob(expected, actual *batchv1.Job) error {
	if expected == nil || actual == nil || actual.UID == "" || expected.Name != actual.Name || expected.Namespace != actual.Namespace ||
		!reflect.DeepEqual(expected.Labels, actual.Labels) || !reflect.DeepEqual(expected.Annotations, actual.Annotations) {
		return fmt.Errorf("recovered analysis input Job identity is invalid")
	}
	if !apiequality.Semantic.DeepDerivative(&expected.Spec, &actual.Spec) {
		return fmt.Errorf("recovered analysis input Job workload identity changed")
	}
	return nil
}

func (r *Runtime) job(name, purpose, envName string, request []byte, manifestHash string) *batchv1.Job {
	backoff := int32(0)
	deadline := int64(r.opts.Timeout/time.Second) + 15
	labels := map[string]string{
		"app.kubernetes.io/managed-by": "prow-ai-dashboard", "prow-ai-dashboard/purpose": "analysis-input-" + purpose,
		"prow-ai-dashboard/execution": name,
	}
	annotations := map[string]string{
		"prow-ai-dashboard/execution-contract-sha256": hashBytes(request), "prow-ai-dashboard/prepared-manifest-sha256": manifestHash,
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.opts.Namespace, Labels: labels, Annotations: annotations},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoff, ActiveDeadlineSeconds: &deadline,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName: r.opts.ServiceAccountName, AutomountServiceAccountToken: boolPtr(false), RestartPolicy: corev1.RestartPolicyNever,
					EnableServiceLinks: boolPtr(false), TerminationGracePeriodSeconds: int64Ptr(5), ActiveDeadlineSeconds: &deadline,
					SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: boolPtr(true), RunAsUser: int64Ptr(65532), RunAsGroup: int64Ptr(65532), FSGroup: int64Ptr(65532), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}, AppArmorProfile: &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeRuntimeDefault}},
					Containers: []corev1.Container{{
						Name: "stager", Image: r.opts.Image, ImagePullPolicy: corev1.PullIfNotPresent,
						Env:             []corev1.EnvVar{{Name: envName, Value: base64.StdEncoding.EncodeToString(request)}},
						SecurityContext: &corev1.SecurityContext{RunAsNonRoot: boolPtr(true), RunAsUser: int64Ptr(65532), RunAsGroup: int64Ptr(65532), AllowPrivilegeEscalation: boolPtr(false), ReadOnlyRootFilesystem: boolPtr(true), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}, SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}, AppArmorProfile: &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeRuntimeDefault}},
						Resources:       corev1.ResourceRequirements{Requests: resourceList(r.opts.Resources, false), Limits: resourceList(r.opts.Resources, true)},
						VolumeMounts:    []corev1.VolumeMount{{Name: "input", MountPath: "/input"}, {Name: "tmp", MountPath: "/tmp"}},
					}},
					Volumes: []corev1.Volume{{Name: "input", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: r.opts.InputClaim}}}, {Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: resource.NewQuantity(64<<20, resource.BinarySI)}}}},
				},
			},
		},
	}
}

func validateOptions(opts Options) error {
	if len(validation.IsDNS1123Label(opts.Namespace)) > 0 || len(validation.IsDNS1123Subdomain(opts.InputClaim)) > 0 || len(validation.IsDNS1123Subdomain(opts.ServiceAccountName)) > 0 || !regexp.MustCompile(`^[^[:space:]@]+@sha256:[0-9a-f]{64}$`).MatchString(opts.Image) || opts.PollEvery <= 0 || opts.Timeout <= 0 || opts.Timeout > 30*time.Minute {
		return fmt.Errorf("analysis publisher options are invalid")
	}
	for _, value := range []string{opts.Resources.CPURequest, opts.Resources.CPULimit, opts.Resources.MemoryRequest, opts.Resources.MemoryLimit, opts.Resources.EphemeralStorage} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("analysis publisher resources are incomplete")
		}
		if _, err := resource.ParseQuantity(value); err != nil {
			return fmt.Errorf("analysis publisher resource quantity is invalid")
		}
	}
	return nil
}

func jobName(purpose, executionID, requestHash string) string {
	sum := sha256.Sum256([]byte(purpose + "\x00" + executionID + "\x00" + requestHash))
	return "analysis-input-" + purpose + "-" + hex.EncodeToString(sum[:6])
}

func hashBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func resourceList(resources Resources, limits bool) corev1.ResourceList {
	cpu, memory := resources.CPURequest, resources.MemoryRequest
	if limits {
		cpu, memory = resources.CPULimit, resources.MemoryLimit
	}
	return corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(memory), corev1.ResourceEphemeralStorage: resource.MustParse(resources.EphemeralStorage)}
}

func boolPtr(value bool) *bool    { return &value }
func int64Ptr(value int64) *int64 { return &value }
