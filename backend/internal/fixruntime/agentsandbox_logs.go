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

	"github.com/willie-yao/aster/backend/internal/redact"
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
