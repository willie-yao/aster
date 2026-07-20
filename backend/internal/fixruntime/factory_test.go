package fixruntime

import (
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
