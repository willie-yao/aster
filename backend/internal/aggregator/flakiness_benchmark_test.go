package aggregator

import (
	"fmt"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

func BenchmarkComputeFlakinessReport(b *testing.B) {
	const testsPerRun = 2000
	runs := make([]models.BuildResult, 10)
	for ri := range runs {
		runs[ri].BuildInfo = models.BuildInfo{BuildID: fmt.Sprint(ri), Started: time.Unix(int64(ri), 0)}
		runs[ri].TestCases = make([]models.TestCase, testsPerRun)
		for ti := range runs[ri].TestCases {
			status := "passed"
			if (ri+ti)%7 == 0 {
				status = "failed"
			}
			runs[ri].TestCases[ti] = models.TestCase{Name: fmt.Sprintf("Test%04d", ti), Status: status}
		}
	}
	results := map[string][]models.BuildResult{"job": runs}
	jobs := []models.ProwJob{{JobID: "job", Name: "job"}}
	b.ResetTimer()
	for range b.N {
		ComputeFlakinessReport(results, jobs, time.Now())
	}
}
