package analysispublisher

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/agentanalysis"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestPublisherJobIsTokenlessAndPinned(t *testing.T) {
	opts := Options{
		Namespace: "analysis-eval", Image: "registry.example.test/stager@sha256:" + strings.Repeat("a", 64), InputClaim: "analysis-input", ServiceAccountName: "analysis-workload",
		PollEvery: time.Millisecond, Timeout: time.Minute,
		Resources: Resources{CPURequest: "100m", CPULimit: "1", MemoryRequest: "128Mi", MemoryLimit: "512Mi", EphemeralStorage: "256Mi"},
	}
	runtime, err := New(fake.NewSimpleClientset(), opts)
	if err != nil {
		t.Fatal(err)
	}
	request := []byte(`{"version":1}`)
	job := runtime.job("analysis-input-publish-0123456789ab", "publish", publishEnv, request, strings.Repeat("b", 64))
	pod := job.Spec.Template.Spec
	if pod.ServiceAccountName != opts.ServiceAccountName || pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken || len(pod.Containers) != 1 || len(pod.Volumes) != 2 {
		t.Fatalf("pod=%+v", pod)
	}
	container := pod.Containers[0]
	if container.Image != opts.Image || len(container.Env) != 1 || container.Env[0].Name != publishEnv || container.Env[0].Value != base64.StdEncoding.EncodeToString(request) || len(container.EnvFrom) != 0 || len(container.VolumeMounts) != 2 {
		t.Fatalf("container=%+v", container)
	}
	if container.SecurityContext == nil || container.SecurityContext.ReadOnlyRootFilesystem == nil || !*container.SecurityContext.ReadOnlyRootFilesystem || container.SecurityContext.AllowPrivilegeEscalation == nil || *container.SecurityContext.AllowPrivilegeEscalation || pod.Volumes[0].PersistentVolumeClaim == nil || pod.Volumes[0].PersistentVolumeClaim.ClaimName != opts.InputClaim {
		t.Fatalf("security/storage=%+v %+v", container.SecurityContext, pod.Volumes)
	}
	if job.Labels["prow-ai-dashboard/purpose"] != "analysis-input-publish" || job.Annotations["prow-ai-dashboard/prepared-manifest-sha256"] != strings.Repeat("b", 64) {
		t.Fatalf("metadata=%+v %+v", job.Labels, job.Annotations)
	}
	if job.Spec.ManualSelector != nil || job.Spec.Selector != nil || job.Spec.CompletionMode != nil || job.Spec.PodFailurePolicy != nil || job.Spec.SuccessPolicy != nil || job.Spec.PodReplacementPolicy != nil || job.Spec.ManagedBy != nil ||
		pod.NodeName != "" || len(pod.NodeSelector) != 0 || pod.Affinity != nil || len(pod.Tolerations) != 0 || pod.SchedulerName != "" || pod.Priority != nil || pod.PriorityClassName != "" || len(pod.ImagePullSecrets) != 0 || pod.HostUsers != nil || pod.DNSConfig != nil ||
		container.SecurityContext.Privileged != nil || len(container.SecurityContext.Capabilities.Add) != 0 || container.SecurityContext.ProcMount != nil || container.SecurityContext.SELinuxOptions != nil || container.SecurityContext.WindowsOptions != nil || len(container.VolumeDevices) != 0 || container.Lifecycle != nil {
		t.Fatalf("optional Job or Pod fields broadened: job=%+v pod=%+v container=%+v", job.Spec, pod, container)
	}
}

func TestPublisherEncodedRequestMatchesAdmissionBound(t *testing.T) {
	opts := Options{
		Namespace: "analysis-eval", Image: "registry.example.test/stager@sha256:" + strings.Repeat("a", 64), InputClaim: "analysis-input", ServiceAccountName: "analysis-workload",
		PollEvery: time.Millisecond, Timeout: time.Minute,
		Resources: Resources{CPURequest: "100m", CPULimit: "1", MemoryRequest: "128Mi", MemoryLimit: "512Mi", EphemeralStorage: "256Mi"},
	}
	runtimeAdapter, err := New(fake.NewSimpleClientset(), opts)
	if err != nil {
		t.Fatal(err)
	}
	raw := bytes.Repeat([]byte{'a'}, agentanalysis.WorkspacePublishRawMaxBytes)
	job := runtimeAdapter.job("analysis-input-publish-0123456789ab", "publish", publishEnv, raw, strings.Repeat("b", 64))
	encoded := job.Spec.Template.Spec.Containers[0].Env[0].Value
	if len(encoded) != agentanalysis.WorkspacePublishEncodedMaxBytes {
		t.Fatalf("encoded bytes=%d want=%d", len(encoded), agentanalysis.WorkspacePublishEncodedMaxBytes)
	}
}

func TestPublisherRecoversAndDeletesAmbiguousCreate(t *testing.T) {
	client := fake.NewSimpleClientset()
	opts := Options{
		Namespace: "analysis-eval", Image: "registry.example.test/stager@sha256:" + strings.Repeat("a", 64), InputClaim: "analysis-input", ServiceAccountName: "analysis-workload",
		PollEvery: time.Millisecond, Timeout: time.Minute,
		Resources: Resources{CPURequest: "100m", CPULimit: "1", MemoryRequest: "128Mi", MemoryLimit: "512Mi", EphemeralStorage: "256Mi"},
	}
	runtimeAdapter, err := New(client, opts)
	if err != nil {
		t.Fatal(err)
	}
	client.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		created := action.(k8stesting.CreateAction).GetObject().(*batchv1.Job).DeepCopy()
		created.UID = types.UID("ambiguous-uid")
		gvr := schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}
		if err := client.Tracker().Create(gvr, created, opts.Namespace); err != nil {
			t.Fatal(err)
		}
		return true, nil, errors.New("ambiguous transport failure")
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = runtimeAdapter.run(ctx, "analysis-input-publish-0123456789ab", "publish", publishEnv, map[string]any{"version": 1}, strings.Repeat("b", 64))
	if err == nil || !strings.Contains(err.Error(), "ambiguous transport failure") {
		t.Fatalf("error=%v", err)
	}
	if _, getErr := client.BatchV1().Jobs(opts.Namespace).Get(context.Background(), "analysis-input-publish-0123456789ab", metav1.GetOptions{}); !apierrors.IsNotFound(getErr) {
		t.Fatalf("ambiguous Job remains: %v", getErr)
	}
}

func TestPublisherDeletionWaitsForJobAndPodAbsence(t *testing.T) {
	uid := types.UID("job-uid")
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "analysis-input-cleanup-0123456789ab", Namespace: "analysis-eval", UID: uid}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "cleanup-pod", Namespace: "analysis-eval", Labels: map[string]string{"job-name": job.Name}}}
	client := fake.NewSimpleClientset(job, pod)
	runtimeAdapter, err := New(client, Options{
		Namespace: "analysis-eval", Image: "registry.example.test/stager@sha256:" + strings.Repeat("a", 64), InputClaim: "analysis-input", ServiceAccountName: "analysis-workload",
		PollEvery: time.Millisecond, Timeout: time.Minute,
		Resources: Resources{CPURequest: "100m", CPULimit: "1", MemoryRequest: "128Mi", MemoryLimit: "512Mi", EphemeralStorage: "256Mi"},
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(10 * time.Millisecond)
		_ = client.CoreV1().Pods("analysis-eval").Delete(context.Background(), pod.Name, metav1.DeleteOptions{})
	}()
	if err := runtimeAdapter.deleteJob(context.Background(), job.Name, uid); err != nil {
		t.Fatal(err)
	}
	if _, err := client.BatchV1().Jobs("analysis-eval").Get(context.Background(), job.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("Job remains: %v", err)
	}
	pods, err := client.CoreV1().Pods("analysis-eval").List(context.Background(), metav1.ListOptions{})
	if err != nil || len(pods.Items) != 0 {
		t.Fatalf("pods=%v err=%v", pods.Items, err)
	}
}

func TestPublisherDeletesSuccessfulCreateWhenIdentityValidationFails(t *testing.T) {
	client := fake.NewSimpleClientset()
	opts := Options{
		Namespace: "analysis-eval", Image: "registry.example.test/stager@sha256:" + strings.Repeat("a", 64), InputClaim: "analysis-input", ServiceAccountName: "analysis-workload",
		PollEvery: time.Millisecond, Timeout: time.Minute,
		Resources: Resources{CPURequest: "100m", CPULimit: "1", MemoryRequest: "128Mi", MemoryLimit: "512Mi", EphemeralStorage: "256Mi"},
	}
	runtimeAdapter, err := New(client, opts)
	if err != nil {
		t.Fatal(err)
	}
	client.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		created := action.(k8stesting.CreateAction).GetObject().(*batchv1.Job).DeepCopy()
		created.UID = types.UID("created-uid")
		created.Spec.Template.Spec.Containers[0].Image = "registry.example.test/wrong@sha256:" + strings.Repeat("c", 64)
		gvr := schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}
		if err := client.Tracker().Create(gvr, created, opts.Namespace); err != nil {
			t.Fatal(err)
		}
		return true, created, nil
	})
	_, err = runtimeAdapter.run(context.Background(), "analysis-input-publish-abcdef012345", "publish", publishEnv, map[string]any{"version": 1}, strings.Repeat("b", 64))
	if err == nil || !strings.Contains(err.Error(), "workload identity changed") {
		t.Fatalf("error=%v", err)
	}
	if _, getErr := client.BatchV1().Jobs(opts.Namespace).Get(context.Background(), "analysis-input-publish-abcdef012345", metav1.GetOptions{}); !apierrors.IsNotFound(getErr) {
		t.Fatalf("invalid Job remains: %v", getErr)
	}
}

func TestPublisherOptionsRejectMutableImage(t *testing.T) {
	_, err := New(fake.NewSimpleClientset(), Options{Namespace: "analysis", Image: "registry/stager:latest", InputClaim: "input", ServiceAccountName: "workload", PollEvery: time.Second, Timeout: time.Minute, Resources: Resources{CPURequest: "1", CPULimit: "1", MemoryRequest: "1Mi", MemoryLimit: "1Mi", EphemeralStorage: "1Mi"}})
	if err == nil {
		t.Fatal("mutable publisher image was accepted")
	}
}

func TestPublisherRequestNamesAreContentAddressed(t *testing.T) {
	first := jobName("publish", "execution-1", strings.Repeat("a", 64))
	second := jobName("publish", "execution-2", strings.Repeat("a", 64))
	if first == second || !strings.HasPrefix(first, "analysis-input-publish-") {
		t.Fatalf("names=%q %q", first, second)
	}
	cleanup, err := agentanalysis.NewWorkspaceCleanupRequest(strings.Repeat("b", 64), "analysis-lease")
	if err != nil || jobName("cleanup", "execution-1", cleanup.Hash) == first {
		t.Fatalf("cleanup=%+v err=%v", cleanup, err)
	}
}
