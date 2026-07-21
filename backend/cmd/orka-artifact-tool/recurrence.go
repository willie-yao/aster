package main

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"path"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/prowbuild"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/storage"
)

const (
	recurrenceDefaultCount   = 10
	recurrenceMaxCount       = 15
	recurrenceMaxReads       = 150
	recurrenceMaxObjectBytes = 16 << 20
	recurrenceMaxTotalBytes  = 96 << 20
)

type recurrenceRecentBuild struct {
	Build   string `json:"build"`
	Checked bool   `json:"checked"`
	Failed  bool   `json:"failed"`
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
	backend := env.rawBackend
	if backend == nil {
		backend = env.backend
	}
	job := &models.ProwJob{Name: jobName, JobType: models.JobTypePeriodic}
	builds, err := prowbuild.ListRecentBuilds(ctx, backend, job, count)
	if err != nil {
		writeToolError(w, http.StatusBadGateway, err.Error())
		return
	}

	recent := make([]recurrenceRecentBuild, 0, len(builds))
	failedIn := 0
	checked := 0
	reads := 0
	bytesScanned := int64(0)
	truncated := false
	for _, build := range builds {
		failed, complete, err := recurrenceBuildFailed(
			ctx,
			backend,
			jobName,
			build.ID,
			args.TestName,
			&reads,
			&bytesScanned,
		)
		if err != nil {
			truncated = true
			log.Printf("⚠ recurrence build=%s: %v", build.ID, err)
		}
		if complete {
			checked++
		}
		if failed {
			failedIn++
		}
		recent = append(recent, recurrenceRecentBuild{Build: build.ID, Checked: complete, Failed: failed})
	}

	log.Printf(
		"♻ recurrence test=%q builds=%d checked=%d failed_in=%d bytes=%d truncated=%t",
		args.TestName,
		len(recent),
		checked,
		failedIn,
		bytesScanned,
		truncated,
	)
	writeJSON(w, map[string]any{
		"test_name":      args.TestName,
		"job":            jobName,
		"builds_checked": checked,
		"failed_in":      failedIn,
		"bytes_scanned":  bytesScanned,
		"truncated":      truncated,
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

func recurrenceBuildFailed(
	ctx context.Context,
	backend storage.Backend,
	jobName, buildID, testName string,
	reads *int,
	bytesScanned *int64,
) (failed, complete bool, err error) {
	loc := prowbuild.BuildLocation{
		JobLocation: prowbuild.JobLocation{JobType: models.JobTypePeriodic},
		JobName:     jobName,
		BuildID:     buildID,
	}
	buildPrefix := loc.BuildPath()
	junitPaths, err := prowbuild.DiscoverJUnitPaths(ctx, backend, loc)
	if err != nil {
		return false, false, err
	}
	for _, junitPath := range junitPaths {
		if *reads >= recurrenceMaxReads || *bytesScanned >= recurrenceMaxTotalBytes {
			return false, false, fmt.Errorf("recurrence scan limit reached")
		}
		*reads++
		failed, _, err := recurrenceJUnitFailed(
			ctx,
			backend,
			recurrenceJUnitPath(buildPrefix, junitPath),
			testName,
			bytesScanned,
		)
		if err != nil {
			return false, false, err
		}
		if failed {
			return true, true, nil
		}
	}
	return false, true, nil
}

func recurrenceJUnitFailed(
	ctx context.Context,
	backend storage.Backend,
	object, testName string,
	bytesScanned *int64,
) (failed, matched bool, err error) {
	remaining := int64(recurrenceMaxTotalBytes) - *bytesScanned
	if remaining <= 0 {
		return false, false, fmt.Errorf("recurrence total byte limit reached")
	}
	limit := min(int64(recurrenceMaxObjectBytes), remaining)
	reader, size, err := backend.Open(ctx, object)
	if err != nil {
		return false, false, err
	}
	defer reader.Close()
	if size > limit {
		return false, false, fmt.Errorf("JUnit object is %d bytes, limit is %d", size, limit)
	}
	counter := &countingReader{Reader: io.LimitReader(reader, limit+1)}
	defer func() { *bytesScanned += counter.n }()
	decoder := xml.NewDecoder(counter)
	found := false
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			if counter.n > limit {
				return false, found, fmt.Errorf("JUnit object exceeded %d-byte limit", limit)
			}
			return false, found, nil
		}
		if err != nil {
			return false, found, err
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Local != "testcase" || xmlAttr(start.Attr, "name") != testName {
			continue
		}
		found = true
		var testCase struct {
			Failure *struct{} `xml:"failure"`
		}
		if err := decoder.DecodeElement(&testCase, &start); err != nil {
			return false, found, err
		}
		if testCase.Failure != nil {
			return true, true, nil
		}
	}
}

type countingReader struct {
	io.Reader
	n int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.n += int64(n)
	return n, err
}

func xmlAttr(attrs []xml.Attr, name string) string {
	for _, attr := range attrs {
		if attr.Name.Local == name {
			return attr.Value
		}
	}
	return ""
}

func recurrenceJUnitPath(buildPrefix, junitPath string) string {
	junitPath = strings.TrimPrefix(junitPath, "/")
	if strings.HasPrefix(junitPath, buildPrefix) {
		return junitPath
	}
	return path.Join(buildPrefix, junitPath)
}
