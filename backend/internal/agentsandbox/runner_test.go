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
	for _, mutate := range []func(*Spec){
		func(s *Spec) { s.Purpose = "Fix Critic" },
		func(s *Spec) { s.Purpose = " causal-critic" },
		func(s *Spec) { s.Purpose = "causal-critic-" },
		func(s *Spec) { s.RequestEnv = "bad-name" },
		func(s *Spec) { s.RequestEnv = "PROW_AI_CRITIC_REQUEST_B64 " },
		func(s *Spec) { s.Request = nil },
		func(s *Spec) { s.Request = []byte{0xff} },
		func(s *Spec) { s.Timeout = 0 },
		func(s *Spec) { s.OutputLimitBytes = 1024 },
		func(s *Spec) { s.ExecutionID = strings.Repeat("x", 129) },
	} {
		candidate := valid
		mutate(&candidate)
		if err := ValidateSpec(candidate); err == nil {
			t.Fatalf("invalid spec accepted: %+v", candidate)
		}
	}
}
