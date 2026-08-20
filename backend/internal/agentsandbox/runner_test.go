package agentsandbox

import (
	"strings"
	"testing"
	"time"
)

func TestValidateSpec(t *testing.T) {
	valid := Spec{Purpose: "causal-critic", RequestEnv: "PROW_AI_CRITIC_REQUEST_B64", Request: []byte(`{"version":1}`), Timeout: time.Minute, OutputLimitBytes: 4096}
	if err := ValidateSpec(valid); err != nil {
		t.Fatal(err)
	}
	staged := valid
	staged.StagedWorkspace = &StagedWorkspace{RequestEnv: "PROW_AI_ANALYSIS_STAGE_REQUEST_B64", Request: []byte(`{"version":1}`), ManifestHash: strings.Repeat("a", 64), IdentityHash: strings.Repeat("b", 64)}
	if err := ValidateSpec(staged); err != nil {
		t.Fatal(err)
	}
	prepared := valid
	prepared.PreparedWorkspace = &PreparedWorkspace{ManifestHash: strings.Repeat("a", 64), IdentityHash: strings.Repeat("b", 64)}
	if err := ValidateSpec(prepared); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Spec){
		func(s *Spec) { s.Purpose = "Fix Critic" },
		func(s *Spec) { s.Purpose = " causal-critic" },
		func(s *Spec) { s.Purpose = "causal-critic-" },
		func(s *Spec) { s.RequestEnv = "bad-name" },
		func(s *Spec) { s.RequestEnv = "PROW_AI_CRITIC_REQUEST_B64 " },
		func(s *Spec) { s.Request = nil },
		func(s *Spec) { s.Request = []byte{0xff} },
		func(s *Spec) { s.Timeout = 0 },
		func(s *Spec) { s.FinalizationGrace = -time.Second },
		func(s *Spec) { s.FinalizationGrace = 10*time.Minute + time.Second },
		func(s *Spec) { s.OutputLimitBytes = 1024 },
		func(s *Spec) { s.ExecutionID = strings.Repeat("x", 129) },
		func(s *Spec) { s.StagedWorkspace = &StagedWorkspace{RequestEnv: s.RequestEnv, Request: []byte(`{}`)} },
		func(s *Spec) {
			s.StagedWorkspace = &StagedWorkspace{RequestEnv: "PROW_AI_STAGE_REQUEST_B64", Request: nil}
		},
		func(s *Spec) {
			s.WritableWorkspace = true
			s.StagedWorkspace = &StagedWorkspace{RequestEnv: "PROW_AI_STAGE_REQUEST_B64", Request: []byte(`{}`)}
		},
		func(s *Spec) {
			s.PreparedWorkspace = &PreparedWorkspace{ManifestHash: "main", IdentityHash: strings.Repeat("b", 64)}
		},
		func(s *Spec) {
			s.PreparedWorkspace = &PreparedWorkspace{ManifestHash: strings.Repeat("a", 64)}
		},
		func(s *Spec) {
			s.PreparedWorkspace = &PreparedWorkspace{ManifestHash: strings.Repeat("a", 64), IdentityHash: strings.Repeat("b", 64)}
			s.StagedWorkspace = &StagedWorkspace{RequestEnv: "PROW_AI_STAGE_REQUEST_B64", Request: []byte(`{}`)}
		},
	} {
		candidate := valid
		mutate(&candidate)
		if err := ValidateSpec(candidate); err == nil {
			t.Fatalf("invalid spec accepted: %+v", candidate)
		}
	}
}
