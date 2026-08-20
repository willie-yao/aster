package fixruntime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestKubeAgentSandboxPodLogsReportsScheduledNeverCreated(t *testing.T) {
	pod := testExecutorPod("fix-eval", "fix-1", map[string]any{
		"phase":      "Pending",
		"conditions": []any{map[string]any{"type": "PodScheduled", "status": "True"}},
	})
	body, err := json.Marshal(metav1.Status{Reason: metav1.StatusReasonBadRequest, Message: `Authorization: Bearer secret-token https://secret.example/log failed`})
	if err != nil {
		t.Fatal(err)
	}
	api := testPodLogAPI(t, http.StatusBadRequest, body, func(context.Context, string, string) string { return describePodLogLifecycle(pod.Object) })
	_, err = api.PodLogs(context.Background(), "fix-eval", "fix-1", agentSandboxContainerName, 4096)
	if err == nil {
		t.Fatal("expected Pod log error")
	}
	message := err.Error()
	for _, want := range []string{"executor container never created after Pod scheduling", "Kubernetes API HTTP 400", "BadRequest"} {
		if !strings.Contains(message, want) {
			t.Fatalf("error %q does not contain %q", message, want)
		}
	}
	for _, forbidden := range []string{"secret-token", "secret.example"} {
		if strings.Contains(message, forbidden) {
			t.Fatalf("error leaked %q: %s", forbidden, message)
		}
	}
}

func TestDescribePodLogLifecycle(t *testing.T) {
	cases := []struct {
		name   string
		status map[string]any
		want   string
	}{
		{name: "image pull", status: containerStatus(map[string]any{"waiting": map[string]any{"reason": "ImagePullBackOff", "message": "retry"}}), want: "image pull failure"},
		{name: "image inspect", status: containerStatus(map[string]any{"waiting": map[string]any{"reason": "ImageInspectError", "message": "inspect failed"}}), want: "image pull failure"},
		{name: "never started", status: containerStatus(map[string]any{"waiting": map[string]any{"reason": "ContainerCreating"}}), want: "never started"},
		{name: "waiting", status: containerStatus(map[string]any{"waiting": map[string]any{"reason": "CrashLoopBackOff"}}), want: "container waiting"},
		{name: "terminated", status: containerStatus(map[string]any{"terminated": map[string]any{"reason": "Error", "exitCode": int64(2), "startedAt": "2026-08-07T00:00:00Z"}}), want: "terminated executor container logs unavailable"},
		{name: "terminated before start", status: containerStatus(map[string]any{"terminated": map[string]any{"reason": "StartError", "exitCode": int64(128)}}), want: "never started and terminated"},
		{name: "running", status: containerStatus(map[string]any{"running": map[string]any{"startedAt": "2026-08-07T00:00:00Z"}}), want: "running but logs are unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pod := testExecutorPod("fix-eval", "fix-1", map[string]any{"containerStatuses": []any{tc.status}})
			if got := describePodLogLifecycle(pod.Object); !strings.Contains(got, tc.want) {
				t.Fatalf("lifecycle = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDescribePodLogLifecycleReportsStagerState(t *testing.T) {
	cases := []struct {
		name  string
		state map[string]any
		want  string
	}{
		{name: "image pull", state: map[string]any{"waiting": map[string]any{"reason": "ImagePullBackOff"}}, want: "stager image pull failure"},
		{name: "waiting", state: map[string]any{"waiting": map[string]any{"reason": "PodInitializing"}}, want: "stager container waiting"},
		{name: "failed", state: map[string]any{"terminated": map[string]any{"reason": "Error", "exitCode": int64(2), "startedAt": "2026-08-10T00:00:00Z"}}, want: "stager container failed with exit code 2"},
		{name: "never started", state: map[string]any{"terminated": map[string]any{"reason": "StartError", "exitCode": int64(128)}}, want: "stager container never started"},
		{name: "running", state: map[string]any{"running": map[string]any{"startedAt": "2026-08-10T00:00:00Z"}}, want: "stager container is running"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pod := testExecutorPod("analysis", "analysis-1", map[string]any{
				"initContainerStatuses": []any{map[string]any{"name": agentSandboxStagerName, "state": tc.state}},
			})
			if got := describePodLogLifecycle(pod.Object); !strings.Contains(got, tc.want) {
				t.Fatalf("lifecycle=%q want=%q", got, tc.want)
			}
		})
	}
}

func TestKubeAgentSandboxPodLogsReportsNotFound(t *testing.T) {
	body, _ := json.Marshal(metav1.Status{Reason: metav1.StatusReasonNotFound, Message: "pod not found"})
	api := testPodLogAPI(t, http.StatusNotFound, body, func(context.Context, string, string) string { return "Pod not found" })
	_, err := api.PodLogs(context.Background(), "fix-eval", "missing", agentSandboxContainerName, 4096)
	if err == nil || !strings.Contains(err.Error(), "Pod not found") || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("error = %v", err)
	}
}

func TestKubeAgentSandboxPodLogsBoundsStatusErrors(t *testing.T) {
	pod := testExecutorPod("fix-eval", "fix-1", map[string]any{})
	for _, tc := range []struct {
		name string
		body []byte
		want string
	}{
		{name: "malformed", body: []byte(`{bad token=secret-value`), want: "malformed Kubernetes API status response"},
		{name: "oversized", body: []byte(strings.Repeat("x", int(maxKubernetesErrorBodyBytes)+1)), want: "status response exceeds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := testPodLogAPI(t, http.StatusInternalServerError, tc.body, func(context.Context, string, string) string { return describePodLogLifecycle(pod.Object) })
			_, err := api.PodLogs(context.Background(), "fix-eval", "fix-1", agentSandboxContainerName, 4096)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "secret-value") {
				t.Fatalf("error leaked credential: %v", err)
			}
		})
	}
}

func TestKubeAgentSandboxPodLogsBoundsSuccessfulBody(t *testing.T) {
	api := testPodLogAPI(t, http.StatusOK, []byte("01234567890"), nil, func(request *http.Request) {
		if got := request.URL.Query().Get("limitBytes"); got != "11" {
			t.Fatalf("limitBytes = %q, want 11", got)
		}
	})
	_, err := api.PodLogs(context.Background(), "fix-eval", "fix-1", agentSandboxContainerName, 10)
	if err == nil || !strings.Contains(err.Error(), "exceeds 10 bytes") {
		t.Fatalf("error = %v", err)
	}
}

func TestKubeAgentSandboxPodLogsReportsEmptySuccessfulBody(t *testing.T) {
	api := testPodLogAPI(t, http.StatusOK, nil, func(context.Context, string, string) string {
		return "stager container failed with exit code 2"
	})
	_, err := api.PodLogs(context.Background(), "analysis", "analysis-1", agentSandboxContainerName, 4096)
	if err == nil || !strings.Contains(err.Error(), "logs for analysis/analysis-1 container executor are empty") || !strings.Contains(err.Error(), "stager container failed") {
		t.Fatalf("error = %v", err)
	}
}

func TestKubeAgentSandboxPodLogsBoundsSuccessfulReadError(t *testing.T) {
	api := &kubeAgentSandboxAPI{
		http: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(errorReader{err: errors.New(`read https://secret.example/log: token=secret-value ` + strings.Repeat("x", maxKubernetesErrorTextBytes+100))})}, nil
		})},
		host: "https://kubernetes.invalid",
	}
	_, err := api.PodLogs(context.Background(), "fix-eval", "fix-1", agentSandboxContainerName, 4096)
	if err == nil || !strings.Contains(err.Error(), "read pod logs") {
		t.Fatalf("error = %v", err)
	}
	if len(err.Error()) > maxKubernetesErrorTextBytes+64 || strings.Contains(err.Error(), "secret.example") || strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("unbounded or unsafe error: %v", err)
	}
}

func testPodLogAPI(t *testing.T, status int, body []byte, lifecycle func(context.Context, string, string) string, checks ...func(*http.Request)) *kubeAgentSandboxAPI {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		for _, check := range checks {
			check(request)
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	return &kubeAgentSandboxAPI{http: server.Client(), host: server.URL, podLifecycle: lifecycle}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

func testExecutorPod(namespace, name string, status map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata":   map[string]any{"namespace": namespace, "name": name},
		"status":     status,
	}}
}

func containerStatus(state map[string]any) map[string]any {
	return map[string]any{"name": agentSandboxContainerName, "state": state}
}

func TestStagerFailureStateClassifiesTerminalFailures(t *testing.T) {
	for _, test := range []struct {
		name       string
		state      map[string]any
		wantCode   string
		wantExit   int64
		wantReason string
	}{
		{name: "nonzero", state: map[string]any{"terminated": map[string]any{"reason": "Error", "exitCode": int64(1)}}, wantCode: "stager_exit_nonzero", wantExit: 1, wantReason: "Error"},
		{name: "image pull", state: map[string]any{"waiting": map[string]any{"reason": "ImagePullBackOff"}}, wantCode: "stager_image_pull", wantExit: -1, wantReason: "ImagePullBackOff"},
		{name: "configuration", state: map[string]any{"waiting": map[string]any{"reason": "CreateContainerConfigError"}}, wantCode: "stager_start_failure", wantExit: -1, wantReason: "CreateContainerConfigError"},
	} {
		t.Run(test.name, func(t *testing.T) {
			pod := testExecutorPod("analysis", "analysis-1", map[string]any{
				"initContainerStatuses": []any{map[string]any{"name": agentSandboxStagerName, "state": test.state}},
			})
			code, exitCode, reason := stagerFailureState(pod.Object)
			if code != test.wantCode || exitCode != test.wantExit || reason != test.wantReason {
				t.Fatalf("failure=(%q,%d,%q), want=(%q,%d,%q)", code, exitCode, reason, test.wantCode, test.wantExit, test.wantReason)
			}
		})
	}
}

func TestStagerDiagnosticCategoryRetainsOnlyAllowlistedCategory(t *testing.T) {
	logs := "analysis staging failed: verify staged source: source_untracked_files Authorization: Bearer secret-token https://secret.example/request"
	if got := stagerDiagnosticCategory(logs); got != "source_untracked_files" {
		t.Fatalf("category=%q", got)
	}
	for _, logs := range []string{
		"unexpected raw output token=secret-value",
		"analysis staging failed: private path /secret/source failed",
	} {
		if got := stagerDiagnosticCategory(logs); got != "unclassified" {
			t.Fatalf("category=%q for %q", got, logs)
		}
	}
}

func TestKubeAgentSandboxPodLogsSelectsStagerContainer(t *testing.T) {
	api := testPodLogAPI(t, http.StatusOK, []byte("analysis staging failed: source_untracked_files\n"), nil, func(request *http.Request) {
		if got := request.URL.Query().Get("container"); got != agentSandboxStagerName {
			t.Fatalf("container=%q", got)
		}
		if got := request.URL.Query().Get("limitBytes"); got != "4097" {
			t.Fatalf("limitBytes=%q", got)
		}
	})
	logs, err := api.PodLogs(context.Background(), "analysis", "analysis-1", agentSandboxStagerName, 4096)
	if err != nil || !strings.Contains(logs, "source_untracked_files") {
		t.Fatalf("logs=%q err=%v", logs, err)
	}
}

func TestKubeAgentSandboxPodLogsRejectsUnknownContainer(t *testing.T) {
	api := &kubeAgentSandboxAPI{}
	if _, err := api.PodLogs(context.Background(), "analysis", "analysis-1", "other", 4096); err == nil {
		t.Fatal("unknown container was accepted")
	}
}
