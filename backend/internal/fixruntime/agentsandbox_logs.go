package fixruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/willie-yao/aster/backend/internal/agentanalysis"
	"github.com/willie-yao/aster/backend/internal/redact"
	engineruntime "github.com/willie-yao/aster/backend/internal/runtime"
	"github.com/willie-yao/aster/backend/internal/textutil"
)

const (
	maxKubernetesErrorBodyBytes = int64(4 << 10)
	maxKubernetesErrorTextBytes = 2 << 10
)

var errKubernetesErrorBodyOversized = errors.New("kubernetes API error response is oversized")

func readBoundedKubernetesBody(reader io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maxKubernetesErrorBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxKubernetesErrorBodyBytes {
		return nil, errKubernetesErrorBodyOversized
	}
	return data, nil
}

func kubernetesStatusDetail(data []byte) string {
	var status metav1.Status
	if len(data) > 0 && json.Unmarshal(data, &status) == nil && (status.Message != "" || status.Reason != "") {
		return safeKubernetesDiagnostic(strings.TrimSpace(string(status.Reason) + ": " + status.Message))
	}
	if len(data) == 0 {
		return "malformed Kubernetes API status response: empty body"
	}
	return "malformed Kubernetes API status response: " + safeKubernetesDiagnostic(string(data))
}

func (k *kubeAgentSandboxAPI) podLogLifecycleContext(ctx context.Context, namespace, podName string) string {
	if k.podLifecycle != nil {
		return k.podLifecycle(ctx, namespace, podName)
	}
	if k.dynamic == nil {
		return "Pod status unavailable: Kubernetes client is not configured"
	}
	podsGVR := schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	object, err := k.dynamic.Resource(podsGVR).Namespace(namespace).Get(ctx, podName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "Pod not found"
	}
	if err != nil {
		return "Pod status unavailable: " + safeKubernetesDiagnostic(err.Error())
	}
	return describePodLogLifecycle(object.Object)
}

func describePodLogLifecycle(pod map[string]any) string {
	if pod == nil {
		return "Pod status is unavailable"
	}
	if detail := describeStagerLifecycle(pod); detail != "" {
		return detail
	}
	statuses, _, _ := unstructured.NestedSlice(pod, "status", "containerStatuses")
	var status map[string]any
	for _, raw := range statuses {
		value, ok := raw.(map[string]any)
		if ok && value["name"] == agentSandboxContainerName {
			status = value
			break
		}
	}
	if status == nil {
		if podScheduled(pod) {
			return "executor container never created after Pod scheduling"
		}
		return "executor container never created"
	}
	state, _, _ := unstructured.NestedMap(status, "state")
	if waiting, ok := state["waiting"].(map[string]any); ok {
		reason, _ := waiting["reason"].(string)
		message, _ := waiting["message"].(string)
		detail := lifecycleDetail(reason, message)
		switch reason {
		case "ErrImagePull", "ImagePullBackOff", "ImageInspectError", "InvalidImageName", "RegistryUnavailable":
			return "executor image pull failure" + detail
		case "ContainerCreating", "CreateContainerConfigError", "RunContainerError", "StartError":
			return "executor container never started" + detail
		default:
			return "executor container waiting" + detail
		}
	}
	if terminated, ok := state["terminated"].(map[string]any); ok {
		reason, _ := terminated["reason"].(string)
		message, _ := terminated["message"].(string)
		exitCode := int64Value(terminated["exitCode"])
		detail := lifecycleDetail(reason, message)
		startedAt, _ := terminated["startedAt"].(string)
		if strings.TrimSpace(startedAt) == "" {
			return fmt.Sprintf("executor container never started and terminated with exit code %d%s", exitCode, detail)
		}
		return fmt.Sprintf("terminated executor container logs unavailable with exit code %d%s", exitCode, detail)
	}
	if _, ok := state["running"].(map[string]any); ok {
		return "executor container is running but logs are unavailable"
	}
	return "executor container never started"
}

func describeStagerLifecycle(pod map[string]any) string {
	statuses, _, _ := unstructured.NestedSlice(pod, "status", "initContainerStatuses")
	for _, raw := range statuses {
		status, ok := raw.(map[string]any)
		if !ok || status["name"] != agentSandboxStagerName {
			continue
		}
		state, _, _ := unstructured.NestedMap(status, "state")
		if waiting, ok := state["waiting"].(map[string]any); ok {
			reason, _ := waiting["reason"].(string)
			message, _ := waiting["message"].(string)
			detail := lifecycleDetail(reason, message)
			switch reason {
			case "ErrImagePull", "ImagePullBackOff", "ImageInspectError", "InvalidImageName", "RegistryUnavailable":
				return "stager image pull failure" + detail
			case "ContainerCreating", "CreateContainerConfigError", "RunContainerError", "StartError":
				return "stager container never started" + detail
			default:
				return "stager container waiting" + detail
			}
		}
		if terminated, ok := state["terminated"].(map[string]any); ok {
			exitCode := int64Value(terminated["exitCode"])
			if exitCode == 0 {
				return ""
			}
			reason, _ := terminated["reason"].(string)
			message, _ := terminated["message"].(string)
			detail := lifecycleDetail(reason, message)
			startedAt, _ := terminated["startedAt"].(string)
			if strings.TrimSpace(startedAt) == "" {
				return fmt.Sprintf("stager container never started and terminated with exit code %d%s", exitCode, detail)
			}
			return fmt.Sprintf("stager container failed with exit code %d%s", exitCode, detail)
		}
		if _, ok := state["running"].(map[string]any); ok {
			return "stager container is running"
		}
	}
	return ""
}

func podScheduled(pod map[string]any) bool {
	conditions, _, _ := unstructured.NestedSlice(pod, "status", "conditions")
	for _, raw := range conditions {
		condition, ok := raw.(map[string]any)
		if ok && condition["type"] == "PodScheduled" && condition["status"] == "True" {
			return true
		}
	}
	return false
}

func int64Value(value any) int64 {
	switch typed := value.(type) {
	case int64:
		return typed
	case int:
		return int64(typed)
	case float64:
		return int64(typed)
	default:
		return -1
	}
}

func lifecycleDetail(reason, message string) string {
	value := strings.TrimSpace(reason)
	if text := strings.TrimSpace(message); text != "" {
		if value != "" {
			value += ": "
		}
		value += text
	}
	if value == "" {
		return ""
	}
	return " (" + safeKubernetesDiagnostic(value) + ")"
}

func safeKubernetesDiagnostic(value string) string {
	value = strings.Join(strings.Fields(redact.Credentials(redact.URLs(value))), " ")
	return textutil.Truncate(value, maxKubernetesErrorTextBytes)
}

func stagerFailureState(pod map[string]any) (string, int64, string) {
	statuses, _, _ := unstructured.NestedSlice(pod, "status", "initContainerStatuses")
	for _, raw := range statuses {
		status, ok := raw.(map[string]any)
		if !ok || status["name"] != agentSandboxStagerName {
			continue
		}
		state, _, _ := unstructured.NestedMap(status, "state")
		if waiting, ok := state["waiting"].(map[string]any); ok {
			reason, _ := waiting["reason"].(string)
			switch reason {
			case "ErrImagePull", "ImagePullBackOff", "ImageInspectError", "InvalidImageName", "RegistryUnavailable":
				return "stager_image_pull", -1, safeContainerReason(reason)
			case "CreateContainerConfigError", "RunContainerError", "StartError", "ContainerCannotRun":
				return "stager_start_failure", -1, safeContainerReason(reason)
			default:
				return "stager_waiting", -1, safeContainerReason(reason)
			}
		}
		if terminated, ok := state["terminated"].(map[string]any); ok {
			exitCode := int64Value(terminated["exitCode"])
			if exitCode == 0 {
				return "", 0, ""
			}
			reason, _ := terminated["reason"].(string)
			return "stager_exit_nonzero", exitCode, safeContainerReason(reason)
		}
	}
	return "", 0, ""
}

func safeContainerReason(reason string) string {
	switch strings.TrimSpace(reason) {
	case "Error", "OOMKilled", "DeadlineExceeded", "ContainerCannotRun", "StartError", "CreateContainerConfigError", "RunContainerError",
		"ErrImagePull", "ImagePullBackOff", "ImageInspectError", "InvalidImageName", "RegistryUnavailable":
		return strings.TrimSpace(reason)
	default:
		return ""
	}
}

func stagerDiagnosticCategory(logs string) string {
	message := strings.TrimSpace(logs)
	const prefix = "analysis staging failed: "
	if !strings.HasPrefix(message, prefix) {
		return "unclassified"
	}
	message = strings.TrimPrefix(message, prefix)
	for _, category := range []string{
		agentanalysis.SourceStagedContentChanged,
		agentanalysis.SourceWorktreeContentChanged,
		agentanalysis.SourceWorktreeModeChanged,
		agentanalysis.SourceIndexFlagsChanged,
		agentanalysis.SourceIndexModeChanged,
		agentanalysis.SourceModePolicyChanged,
		agentanalysis.SourceUntrackedFiles,
		agentanalysis.SourceGitDiffError,
	} {
		if strings.Contains(message, category) {
			return category
		}
	}
	for _, category := range []struct {
		contains string
		code     string
	}{
		{contains: "workspace execution", code: "execution_request_invalid"},
		{contains: "workspace stage", code: "stage_request_invalid"},
		{contains: "workspace root", code: "workspace_root_invalid"},
		{contains: "artifact manifest", code: "artifact_manifest_invalid"},
		{contains: "staged source", code: "source_snapshot_invalid"},
		{contains: "copied source", code: "source_copy_invalid"},
		{contains: "staged artifacts", code: "artifact_copy_failed"},
		{contains: "analysis result directory", code: "result_directory_failed"},
		{contains: "execution request", code: "execution_request_write_failed"},
	} {
		if strings.Contains(message, category.contains) {
			return category.code
		}
	}
	return "unclassified"
}

func agentSandboxStagingError(state sandboxState, diagnostic string) error {
	if diagnostic == "" {
		diagnostic = "unavailable"
	}
	detail := ""
	if state.StagerExitCode >= 0 {
		detail += fmt.Sprintf(" exit_code=%d", state.StagerExitCode)
	}
	if reason := safeContainerReason(state.StagerReason); reason != "" {
		detail += " reason=" + reason
	}
	return fmt.Errorf("%w: code=%s%s diagnostic=%s", engineruntime.ErrStaging, state.StagerFailureCode, detail, diagnostic)
}
