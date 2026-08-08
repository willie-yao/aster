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
	_, err = api.PodLogs(context.Background(), "fix-eval", "fix-1", 4096)
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

func TestKubeAgentSandboxPodLogsReportsNotFound(t *testing.T) {
	body, _ := json.Marshal(metav1.Status{Reason: metav1.StatusReasonNotFound, Message: "pod not found"})
	api := testPodLogAPI(t, http.StatusNotFound, body, func(context.Context, string, string) string { return "Pod not found" })
	_, err := api.PodLogs(context.Background(), "fix-eval", "missing", 4096)
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
			_, err := api.PodLogs(context.Background(), "fix-eval", "fix-1", 4096)
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
	_, err := api.PodLogs(context.Background(), "fix-eval", "fix-1", 10)
	if err == nil || !strings.Contains(err.Error(), "exceeds 10 bytes") {
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
	_, err := api.PodLogs(context.Background(), "fix-eval", "fix-1", 4096)
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
