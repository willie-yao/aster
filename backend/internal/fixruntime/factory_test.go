package fixruntime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
)

func TestNewDefaultsToLocalAgent(t *testing.T) {
	got, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.(*runtime.LocalAgentRuntime); !ok {
		t.Fatalf("runtime = %T, want LocalAgentRuntime", got)
	}
}

func TestNewOrkaRequiresConfiguration(t *testing.T) {
	if _, err := New(&project.FixAgentRuntime{Type: "orka"}); err == nil {
		t.Fatal("incomplete Orka runtime config was accepted")
	}
}

func TestNewOrkaRejectsPartialDelegatedIdentity(t *testing.T) {
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	config := `apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: https://127.0.0.1:65535
    insecure-skip-tls-verify: true
users:
- name: test
  user:
    token: test
contexts:
- name: test
  context:
    cluster: test
    user: test
current-context: test
`
	if err := os.WriteFile(kubeconfig, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", kubeconfig)
	t.Setenv("ORKA_FIX_SERVICE_ACCOUNT_NAME", "dashboard-fix")
	_, err := New(&project.FixAgentRuntime{
		Type: "orka", OrkaAgentRef: "fixer", OrkaAPI: "http://orka.invalid", OrkaNamespace: "orka-system",
	})
	if err == nil || !strings.Contains(err.Error(), "delegated ServiceAccount namespace is required") {
		t.Fatalf("error = %v", err)
	}
}
