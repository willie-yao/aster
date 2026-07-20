package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"path"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/junit"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/prowbuild"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/storage"
)

const (
	recurrenceDefaultCount = 10
	recurrenceMaxCount     = 15
	recurrenceMaxReads     = 150
)

type recurrenceRecentBuild struct {
	Build  string `json:"build"`
	Failed bool   `json:"failed"`
}

func init() {
	registerQTool("/tool/recurrence", recurrence)
}

func recurrence(env *toolEnv, w http.ResponseWriter, r *http.Request) {
	if !requirePOST(w, r) {
		return
	}
	var args struct {
		TestName string `json:"test_name"`
		Count    int    `json:"count"`
	}
	if err := readArgs(r, &args); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if args.TestName == "" {
		writeToolError(w, http.StatusBadRequest, "test_name is required")
		return
	}
	count := args.Count
	if count <= 0 {
		count = recurrenceDefaultCount
	}
	if count > recurrenceMaxCount {
		count = recurrenceMaxCount
	}

	jobName, ok := recurrenceJobName(env.buildPrefix)
	if !ok {
		writeToolError(w, http.StatusUnprocessableEntity, "invalid build prefix")
		return
	}

	ctx, cancel := requestCtx(r)
	defer cancel()

	job := &models.ProwJob{Name: jobName, JobType: models.JobTypePeriodic}
	builds, err := prowbuild.ListRecentBuilds(ctx, env.backend, job, count)
	if err != nil {
		writeToolError(w, http.StatusBadGateway, err.Error())
		return
	}

	recent := make([]recurrenceRecentBuild, 0, len(builds))
	failedIn := 0
	reads := 0
	for _, build := range builds {
		failed, err := recurrenceBuildFailed(ctx, env, jobName, build.ID, args.TestName, &reads)
		if err != nil {
			writeToolError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		if failed {
			failedIn++
		}
		recent = append(recent, recurrenceRecentBuild{Build: build.ID, Failed: failed})
	}

	log.Printf("♻ recurrence test=%q builds=%d failed_in=%d", args.TestName, len(recent), failedIn)
	writeJSON(w, map[string]any{
		"test_name":      args.TestName,
		"job":            jobName,
		"builds_checked": len(recent),
		"failed_in":      failedIn,
		"recent":         recent,
	})
}

func recurrenceJobName(buildPrefix string) (string, bool) {
	p := strings.Trim(strings.TrimSuffix(buildPrefix, "/"), "/")
	p = strings.TrimPrefix(p, "logs/")
	parts := strings.Split(p, "/")
	if len(parts) < 2 || parts[0] == "" {
		return "", false
	}
	return parts[0], true
}

func recurrenceBuildFailed(ctx context.Context, env *toolEnv, jobName, buildID, testName string, reads *int) (bool, error) {
	loc := prowbuild.BuildLocation{
		JobLocation: prowbuild.JobLocation{JobType: models.JobTypePeriodic},
		JobName:     jobName,
		BuildID:     buildID,
	}
	buildPrefix := loc.BuildPath()
	junitPaths, err := prowbuild.DiscoverJUnitPaths(ctx, env.backend, loc)
	if err != nil {
		return false, err
	}
	for _, junitPath := range junitPaths {
		if *reads >= recurrenceMaxReads {
			return false, nil
		}
		*reads++
		data, err := storage.ReadAll(ctx, env.backend, recurrenceJUnitPath(buildPrefix, junitPath))
		if err != nil {
			if errors.Is(err, errArtifactBudget) {
				return false, err
			}
			continue
		}
		cases, err := junit.Parse(data)
		if err != nil {
			continue
		}
		for _, tc := range cases {
			if tc.Name == testName && tc.Status == "failed" {
				return true, nil
			}
		}
	}
	return false, nil
}

func recurrenceJUnitPath(buildPrefix, junitPath string) string {
	junitPath = strings.TrimPrefix(junitPath, "/")
	if strings.HasPrefix(junitPath, buildPrefix) {
		return junitPath
	}
	return path.Join(buildPrefix, junitPath)
}
