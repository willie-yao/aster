package fixruntime

import (
	"encoding/base64"
	"fmt"
	"time"

	k8sruntime "k8s.io/apimachinery/pkg/runtime"

	"github.com/willie-yao/aster/backend/internal/agentanalysis"
	"github.com/willie-yao/aster/backend/internal/agentsandbox"
	"github.com/willie-yao/aster/backend/internal/modelprovider"
	engineruntime "github.com/willie-yao/aster/backend/internal/runtime"
)

type appArmorCapability uint8

const (
	appArmorRuntimeDefault appArmorCapability = iota
	appArmorUnavailableForKindTest
	agentSandboxResultVolumeLimit = "4Mi"
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
	resultGrace := agentSandboxResultGraceForPurpose(spec.Purpose)
	activeDeadline := int64(spec.Timeout.Round(time.Second)/time.Second) + int64(resultGrace/time.Second)
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
	resources := map[string]any{
		"requests": map[string]any{"cpu": r.opts.Resources.CPURequest, "memory": r.opts.Resources.MemoryRequest, "ephemeral-storage": r.opts.Resources.EphemeralStorage},
		"limits":   map[string]any{"cpu": r.opts.Resources.CPULimit, "memory": r.opts.Resources.MemoryLimit, "ephemeral-storage": r.opts.Resources.EphemeralStorage},
	}
	fileBackedAnalysis := spec.Purpose == "analysis" && (spec.StagedWorkspace != nil || spec.PreparedWorkspace != nil)
	env := []any{}
	if !fileBackedAnalysis {
		env = append(env, map[string]any{
			"name":  spec.RequestEnv,
			"value": base64.StdEncoding.EncodeToString(spec.Request),
		})
	}
	if r.opts.CABundle.Enabled() {
		env = append(env,
			map[string]any{"name": "NODE_EXTRA_CA_CERTS", "value": modelprovider.CABundleMountPath},
			map[string]any{"name": modelprovider.CABundleHashEnv, "value": r.opts.CABundle.SHA256},
		)
	}
	if r.opts.ModelProvider.Auth.Type == modelprovider.AuthTypeBearer {
		env = append(env, map[string]any{
			"name": modelprovider.TokenEnv,
			"valueFrom": map[string]any{"secretKeyRef": map[string]any{
				"name": r.opts.ProviderSecretRef.Name,
				"key":  r.opts.ProviderSecretRef.Key,
			}},
		})
	}
	container := map[string]any{
		"name":            agentSandboxContainerName,
		"image":           r.opts.Image,
		"imagePullPolicy": "IfNotPresent",
		"env":             env,
		"securityContext": containerSecurity,
		"resources":       resources,
	}
	podSpec := map[string]any{
		"serviceAccountName":            r.opts.ServiceAccountName,
		"automountServiceAccountToken":  false,
		"restartPolicy":                 "Never",
		"activeDeadlineSeconds":         activeDeadline,
		"terminationGracePeriodSeconds": int64(5),
		"enableServiceLinks":            false,
		"securityContext":               podSecurity,
		"containers":                    []any{container},
	}
	switch {
	case spec.PreparedWorkspace != nil:
		manifestHash := spec.PreparedWorkspace.ManifestHash
		requestEnv := analysisExecutionRequestChunkEnvironment(spec.Request)
		container["volumeMounts"] = []any{
			map[string]any{"name": "input", "mountPath": agentsandbox.StagedWorkspaceSourcePath, "subPath": manifestHash + "/" + agentanalysis.WorkspaceSourceDir, "readOnly": true},
			map[string]any{"name": "input", "mountPath": agentsandbox.StagedWorkspaceArtifactsPath, "subPath": manifestHash + "/" + agentanalysis.WorkspaceArtifactsDir, "readOnly": true},
			map[string]any{"name": "request", "mountPath": agentanalysis.WorkspaceExecutionRequestRoot, "readOnly": true},
			map[string]any{"name": "result", "mountPath": agentsandbox.StagedWorkspaceResultPath},
			map[string]any{"name": "executor-tmp", "mountPath": "/tmp"},
		}
		podSpec["initContainers"] = []any{map[string]any{
			"name":            agentSandboxStagerName,
			"image":           r.opts.StagerImage,
			"imagePullPolicy": "IfNotPresent",
			"args":            []any{"request"},
			"env":             requestEnv,
			"securityContext": k8sruntime.DeepCopyJSONValue(containerSecurity),
			"resources":       k8sruntime.DeepCopyJSONValue(resources),
			"volumeMounts": []any{
				map[string]any{"name": "request", "mountPath": agentanalysis.WorkspaceExecutionRequestRoot},
				map[string]any{"name": "stager-tmp", "mountPath": "/tmp"},
			},
		}}
		podSpec["volumes"] = preparedReadOnlyWorkspaceVolumes(r.opts.StagerInputClaim)
	case spec.StagedWorkspace != nil:
		stage := spec.StagedWorkspace
		requestEnv := append([]any{map[string]any{
			"name":  stage.RequestEnv,
			"value": base64.StdEncoding.EncodeToString(stage.Request),
		}}, analysisExecutionRequestChunkEnvironment(spec.Request)...)
		container["volumeMounts"] = []any{
			map[string]any{"name": "workspace", "mountPath": agentsandbox.StagedWorkspaceRoot, "readOnly": true},
			map[string]any{"name": "request", "mountPath": agentanalysis.WorkspaceExecutionRequestRoot, "readOnly": true},
			map[string]any{"name": "result", "mountPath": agentsandbox.StagedWorkspaceResultPath},
			map[string]any{"name": "executor-tmp", "mountPath": "/tmp"},
		}
		podSpec["initContainers"] = []any{map[string]any{
			"name":            agentSandboxStagerName,
			"image":           r.opts.StagerImage,
			"imagePullPolicy": "IfNotPresent",
			"env":             requestEnv,
			"securityContext": k8sruntime.DeepCopyJSONValue(containerSecurity),
			"resources":       k8sruntime.DeepCopyJSONValue(resources),
			"volumeMounts": []any{
				map[string]any{"name": "input", "mountPath": agentsandbox.StagedWorkspaceInputPath, "readOnly": true},
				map[string]any{"name": "workspace", "mountPath": agentsandbox.StagedWorkspaceRoot},
				map[string]any{"name": "request", "mountPath": agentanalysis.WorkspaceExecutionRequestRoot},
				map[string]any{"name": "stager-tmp", "mountPath": "/tmp"},
			},
		}}
		podSpec["volumes"] = stagedReadOnlyWorkspaceVolumes(r.opts.Resources.EphemeralStorage, r.opts.StagerInputClaim)
	case spec.WritableWorkspace:
		mounts := []any{
			map[string]any{"name": "workspace", "mountPath": agentsandbox.StagedWorkspaceRoot},
			map[string]any{"name": "tmp", "mountPath": "/tmp"},
		}
		volumes := writableWorkspaceVolumes(r.opts.Resources.EphemeralStorage)
		if r.opts.CABundle.Enabled() {
			mounts = append(mounts, map[string]any{
				"name": modelprovider.CABundleVolumeName, "mountPath": modelprovider.CABundleMountDir, "readOnly": true,
			})
			volumes = append(volumes, map[string]any{
				"name": modelprovider.CABundleVolumeName,
				"configMap": map[string]any{
					"name": r.opts.CABundle.ExistingConfigMap, "optional": false, "defaultMode": int64(0o444),
					"items": []any{map[string]any{"key": r.opts.CABundle.Key, "path": "ca-bundle.pem"}},
				},
			})
		}
		container["volumeMounts"] = mounts
		podSpec["volumes"] = volumes
	}
	if r.opts.RuntimeClassName != "" {
		podSpec["runtimeClassName"] = r.opts.RuntimeClassName
	}
	return podSpec
}

func analysisExecutionRequestChunkEnvironment(request []byte) []any {
	chunks, _ := agentanalysis.EncodeWorkspaceExecutionRequestChunks(request)
	env := make([]any, 0, len(chunks))
	for index, value := range chunks {
		env = append(env, map[string]any{"name": agentanalysis.WorkspaceExecutionRequestChunkEnv(index), "value": value})
	}
	return env
}

func writableWorkspaceVolumes(sizeLimit string) []any {
	return []any{
		map[string]any{"name": "workspace", "emptyDir": map[string]any{"sizeLimit": sizeLimit}},
		map[string]any{"name": "tmp", "emptyDir": map[string]any{"sizeLimit": "64Mi"}},
	}
}

func preparedReadOnlyWorkspaceVolumes(inputClaim string) []any {
	return []any{
		map[string]any{"name": "input", "persistentVolumeClaim": map[string]any{"claimName": inputClaim, "readOnly": true}},
		map[string]any{"name": "request", "emptyDir": map[string]any{"sizeLimit": "1Mi"}},
		map[string]any{"name": "result", "emptyDir": map[string]any{"sizeLimit": agentSandboxResultVolumeLimit}},
		map[string]any{"name": "executor-tmp", "emptyDir": map[string]any{"sizeLimit": "64Mi"}},
		map[string]any{"name": "stager-tmp", "emptyDir": map[string]any{"sizeLimit": "64Mi"}},
	}
}

func stagedReadOnlyWorkspaceVolumes(sizeLimit, inputClaim string) []any {
	return []any{
		map[string]any{"name": "input", "persistentVolumeClaim": map[string]any{"claimName": inputClaim, "readOnly": true}},
		map[string]any{"name": "workspace", "emptyDir": map[string]any{"sizeLimit": sizeLimit}},
		map[string]any{"name": "request", "emptyDir": map[string]any{"sizeLimit": "1Mi"}},
		map[string]any{"name": "result", "emptyDir": map[string]any{"sizeLimit": agentSandboxResultVolumeLimit}},
		map[string]any{"name": "executor-tmp", "emptyDir": map[string]any{"sizeLimit": "64Mi"}},
		map[string]any{"name": "stager-tmp", "emptyDir": map[string]any{"sizeLimit": "64Mi"}},
	}
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
