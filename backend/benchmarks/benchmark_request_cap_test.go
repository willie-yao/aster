package benchmarks

import (
	"testing"

	"github.com/willie-yao/aster/backend/internal/project"
)

type benchmarkRequestCap struct {
	ConfiguredIterations         int
	ByteFloorExtensions          int
	MainLoopRequests             int
	ForcedFinalizationRequests   int
	CritiqueToolRequests         int
	CritiqueFinalizationRequests int
	PerOperation                 int
}

func deriveBenchmarkRequestCap(agentic project.Agentic) benchmarkRequestCap {
	configured := max(agentic.MaxIters, 0)
	byteFloorExtensions := 0
	if configured > 0 && agentic.MinGCSBytes > 0 {
		byteFloorExtensions = 1
	}
	critiqueToolRequests := 0
	critiqueFinalizationRequests := 0
	if agentic.Critique.MaxRetries != nil && *agentic.Critique.MaxRetries > 0 {
		critiqueToolRequests = 1
		critiqueFinalizationRequests = 1
	}
	cap := benchmarkRequestCap{
		ConfiguredIterations:         configured,
		ByteFloorExtensions:          byteFloorExtensions,
		MainLoopRequests:             configured + byteFloorExtensions,
		ForcedFinalizationRequests:   1,
		CritiqueToolRequests:         critiqueToolRequests,
		CritiqueFinalizationRequests: critiqueFinalizationRequests,
	}
	cap.PerOperation = cap.MainLoopRequests + cap.ForcedFinalizationRequests +
		cap.CritiqueToolRequests + cap.CritiqueFinalizationRequests
	return cap
}

func (c benchmarkRequestCap) total(compatibilityRequests, operations int) int {
	return compatibilityRequests + operations*c.PerOperation
}

func TestDeriveBenchmarkRequestCap(t *testing.T) {
	oneRetry := 1
	cap := deriveBenchmarkRequestCap(project.Agentic{
		MaxIters: 11, MinGCSBytes: 5_000_000,
		Critique: project.AgenticCritique{MaxRetries: &oneRetry},
	})
	if cap.ConfiguredIterations != 11 || cap.ByteFloorExtensions != 1 || cap.MainLoopRequests != 12 ||
		cap.ForcedFinalizationRequests != 1 || cap.CritiqueToolRequests != 1 || cap.CritiqueFinalizationRequests != 1 || cap.PerOperation != 15 {
		t.Fatalf("cap = %+v", cap)
	}
	if total := cap.total(2, 4); total != 62 {
		t.Fatalf("total cap = %d, want 62", total)
	}
}

func TestDeriveBenchmarkRequestCapDisablesOptionalPaths(t *testing.T) {
	zeroRetries := 0
	cap := deriveBenchmarkRequestCap(project.Agentic{
		MaxIters: 11,
		Critique: project.AgenticCritique{MaxRetries: &zeroRetries},
	})
	if cap.ByteFloorExtensions != 0 || cap.CritiqueToolRequests != 0 || cap.CritiqueFinalizationRequests != 0 || cap.PerOperation != 12 {
		t.Fatalf("cap = %+v", cap)
	}
}

func TestDeriveBenchmarkRequestCapBoundsCritiqueRepairOnce(t *testing.T) {
	configuredRetries := 5
	cap := deriveBenchmarkRequestCap(project.Agentic{
		MaxIters: 1,
		Critique: project.AgenticCritique{MaxRetries: &configuredRetries},
	})
	if cap.CritiqueToolRequests != 1 || cap.CritiqueFinalizationRequests != 1 || cap.PerOperation != 4 {
		t.Fatalf("cap = %+v", cap)
	}
}
