package fixruntime

import (
	"encoding/base64"
	"fmt"
	"time"

	k8sruntime "k8s.io/apimachinery/pkg/runtime"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/agentsandbox"
	engineruntime "github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
)

type appArmorCapability uint8

const (
	appArmorRuntimeDefault appArmorCapability = iota
	appArmorUnavailableForKindTest
)

func (c appArmorCapability) String() string {
	switch c {
	case appArmorRuntimeDefault:
		return "runtime-default"
	case appArmorUnavailableForKindTest:
		return "unavailable-for-kind-test"
	default:
		return fmt.Sprintf("unknown-%d", c)
	}
}

func (r *AgentSandboxRuntime) sandboxWorkloadPodSpec(spec agentsandbox.Spec) map[string]any {
	activeDeadline := int64(spec.Timeout.Round(time.Second)/time.Second) + int64(agentSandboxResultGrace/time.Second)
	podSecurity := map[string]any{
		"runAsNonRoot":   true,
		"runAsUser":      int64(65532),
		"runAsGroup":     int64(65532),
		"fsGroup":        int64(65532),
		"seccompProfile": map[string]any{"type": "RuntimeDefault"},
	}
	containerSecurity := map[string]any{
		"allowPrivilegeEscalation": false,
		"readOnlyRootFilesystem":   true,
		"runAsNonRoot":             true,
		"runAsUser":                int64(65532),
		"runAsGroup":               int64(65532),
		"capabilities":             map[string]any{"drop": []any{"ALL"}},
		"seccompProfile":           map[string]any{"type": "RuntimeDefault"},
	}
	if r.opts.appArmorCapability == appArmorRuntimeDefault {
		podSecurity["appArmorProfile"] = map[string]any{"type": "RuntimeDefault"}
		containerSecurity["appArmorProfile"] = map[string]any{"type": "RuntimeDefault"}
	}
	podSpec := map[string]any{
		"serviceAccountName":            r.opts.ServiceAccountName,
		"automountServiceAccountToken":  false,
		"restartPolicy":                 "Never",
		"activeDeadlineSeconds":         activeDeadline,
		"terminationGracePeriodSeconds": int64(5),
		"enableServiceLinks":            false,
		"securityContext":               podSecurity,
		"containers": []any{map[string]any{
			"name":            agentSandboxContainerName,
			"image":           r.opts.Image,
			"imagePullPolicy": "IfNotPresent",
			"env": []any{map[string]any{
				"name":  spec.RequestEnv,
				"value": base64.StdEncoding.EncodeToString(spec.Request),
			}},
			"securityContext": containerSecurity,
			"resources": map[string]any{
				"requests": map[string]any{"cpu": r.opts.Resources.CPURequest, "memory": r.opts.Resources.MemoryRequest, "ephemeral-storage": r.opts.Resources.EphemeralStorage},
				"limits":   map[string]any{"cpu": r.opts.Resources.CPULimit, "memory": r.opts.Resources.MemoryLimit, "ephemeral-storage": r.opts.Resources.EphemeralStorage},
			},
		}},
	}
	if spec.WritableWorkspace {
		container := podSpec["containers"].([]any)[0].(map[string]any)
		container["volumeMounts"] = []any{
			map[string]any{"name": "workspace", "mountPath": "/workspace"},
			map[string]any{"name": "tmp", "mountPath": "/tmp"},
		}
		podSpec["volumes"] = []any{
			map[string]any{"name": "workspace", "emptyDir": map[string]any{"sizeLimit": r.opts.Resources.EphemeralStorage}},
			map[string]any{"name": "tmp", "emptyDir": map[string]any{"sizeLimit": "64Mi"}},
		}
	}
	if r.opts.RuntimeClassName != "" {
		podSpec["runtimeClassName"] = r.opts.RuntimeClassName
	}
	return podSpec
}

func (r *AgentSandboxRuntime) workloadPodSpec(requestJSON []byte, request engineruntime.ExecutionRequest) map[string]any {
	return r.sandboxWorkloadPodSpec(agentsandbox.Spec{
		Purpose: "fix", RequestEnv: agentSandboxRequestEnv, Request: requestJSON,
		Timeout: time.Duration(request.TimeoutSeconds) * time.Second, OutputLimitBytes: request.OutputLimitBytes, WritableWorkspace: true,
	})
}

func (r *AgentSandboxRuntime) preflightPodObject(namespace, name string, requestJSON []byte, request engineruntime.ExecutionRequest) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
			"labels":    map[string]any{"prow-ai-dashboard/preflight": name},
		},
		"spec": k8sruntime.DeepCopyJSONValue(r.workloadPodSpec(requestJSON, request)),
	}
}
