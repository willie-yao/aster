package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/prowbuild"
)

const (
	diffLastPassingReadChunk      = 64 * 1024
	diffLastPassingReadCap        = 256 * 1024
	diffLastPassingMaxInputLines  = 4000
	diffLastPassingMaxOutputLines = 400
	diffLastPassingContextLines   = 3
)

func init() {
	registerQTool("/tool/diff_last_passing", diffLastPassing)
}

func diffLastPassing(env *toolEnv, w http.ResponseWriter, r *http.Request) {
	if !requirePOST(w, r) {
		return
	}
	var args struct {
		Path string `json:"path"`
	}
	if err := readArgs(r, &args); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if args.Path == "" {
		writeJSON(w, map[string]any{"error": "path is required"})
		return
	}

	jobName, currentBuild, ok := diffLastPassingCurrentBuild(env.buildPrefix)
	if !ok {
		writeJSON(w, map[string]any{"error": fmt.Sprintf("unsupported build prefix %q", env.buildPrefix)})
		return
	}

	ctx, cancel := requestCtx(r)
	defer cancel()

	job := &models.ProwJob{Name: jobName, JobType: models.JobTypePeriodic}
	passingBuild := diffLastPassingFindRecentGreen(ctx, env, job, currentBuild)
	if passingBuild == "" {
		writeJSON(w, map[string]any{"passing_build": "", "note": "no recent passing build found"})
		return
	}

	currentData, currentTruncated, err := diffLastPassingReadArtifact(ctx, env.browser, args.Path)
	if err != nil {
		writeJSON(w, map[string]any{"error": fmt.Sprintf("read current build %s %s: %v", currentBuild, args.Path, err)})
		return
	}

	passingBrowser := env.browserForBuild("logs/"+jobName+"/"+passingBuild+"/", jobName+"/"+passingBuild)
	passingData, passingTruncated, err := diffLastPassingReadArtifact(ctx, passingBrowser, args.Path)
	if err != nil {
		writeJSON(w, map[string]any{"error": fmt.Sprintf("read passing build %s %s: %v", passingBuild, args.Path, err)})
		return
	}

	diff, diffLines, diffTruncated := diffLastPassingDiff(passingData, currentData, "passing/"+passingBuild+"/"+args.Path, "current/"+currentBuild+"/"+args.Path)
	truncated := currentTruncated || passingTruncated || diffTruncated

	log.Printf("🩹 diff_last_passing path=%s passing=%s diff_lines=%d", args.Path, passingBuild, diffLines)
	writeJSON(w, map[string]any{
		"path":          args.Path,
		"current_build": currentBuild,
		"passing_build": passingBuild,
		"diff":          diff,
		"truncated":     truncated,
	})
}

func diffLastPassingCurrentBuild(prefix string) (string, string, bool) {
	if !strings.HasPrefix(prefix, "logs/") {
		return "", "", false
	}
	trimmed := strings.TrimSuffix(prefix, "/")
	trimmed = strings.TrimPrefix(trimmed, "logs/")
	parts := strings.Split(trimmed, "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func diffLastPassingFindRecentGreen(ctx context.Context, env *toolEnv, job *models.ProwJob, currentBuild string) string {
	builds, _ := prowbuild.ListRecentBuilds(ctx, env.backend, job, 15)
	for _, build := range builds {
		if build.ID == currentBuild {
			continue
		}
		info, err := prowbuild.FetchBuildInfo(ctx, env.backend, prowbuild.BuildLocation{
			JobLocation: prowbuild.JobLocation{JobType: models.JobTypePeriodic},
			JobName:     job.Name,
			BuildID:     build.ID,
		})
		if err != nil || info == nil {
			continue
		}
		if info.Passed || info.Result == "SUCCESS" {
			return build.ID
		}
	}
	return ""
}

func diffLastPassingReadArtifact(ctx context.Context, browser artifacts.Browser, path string) ([]byte, bool, error) {
	content := make([]byte, 0, diffLastPassingReadCap)
	total := int64(-1)
	for offset := 0; offset < diffLastPassingReadCap; {
		want := min(diffLastPassingReadChunk, diffLastPassingReadCap-offset)
		chunk, size, err := browser.Read(ctx, path, offset, want)
		if err != nil {
			return nil, false, err
		}
		if size >= 0 {
			total = size
		}
		content = append(content, chunk...)
		offset += len(chunk)
		if len(chunk) == 0 || len(chunk) < want || (total >= 0 && int64(offset) >= total) {
			break
		}
	}
	if total >= 0 {
		return content, int64(len(content)) < total, nil
	}
	return content, len(content) >= diffLastPassingReadCap, nil
}

func diffLastPassingDiff(oldData, newData []byte, oldLabel, newLabel string) (string, int, bool) {
	oldLines, oldTruncated := diffLastPassingSplitLines(oldData)
	newLines, newTruncated := diffLastPassingSplitLines(newData)
	truncated := oldTruncated || newTruncated

	prefix := 0
	for prefix < len(oldLines) && prefix < len(newLines) && oldLines[prefix] == newLines[prefix] {
		prefix++
	}
	oldEnd, newEnd := len(oldLines), len(newLines)
	for oldEnd > prefix && newEnd > prefix && oldLines[oldEnd-1] == newLines[newEnd-1] {
		oldEnd--
		newEnd--
	}
	if prefix == len(oldLines) && prefix == len(newLines) {
		return "", 0, truncated
	}

	contextStart := max(0, prefix-diffLastPassingContextLines)
	oldContextEnd := min(len(oldLines), oldEnd+diffLastPassingContextLines)
	newContextEnd := min(len(newLines), newEnd+diffLastPassingContextLines)

	oldStart := contextStart + 1
	newStart := contextStart + 1
	oldCount := oldContextEnd - contextStart
	newCount := newContextEnd - contextStart
	if oldCount == 0 {
		oldStart = contextStart
	}
	if newCount == 0 {
		newStart = contextStart
	}

	out := make([]string, 0, min(diffLastPassingMaxOutputLines, oldCount+newCount+3))
	appendLine := func(line string) {
		if len(out) >= diffLastPassingMaxOutputLines {
			truncated = true
			return
		}
		out = append(out, line)
	}

	appendLine("--- " + oldLabel)
	appendLine("+++ " + newLabel)
	appendLine(fmt.Sprintf("@@ -%d,%d +%d,%d @@", oldStart, oldCount, newStart, newCount))
	for _, line := range oldLines[contextStart:prefix] {
		appendLine(" " + line)
	}
	for _, line := range oldLines[prefix:oldEnd] {
		appendLine("-" + line)
	}
	for _, line := range newLines[prefix:newEnd] {
		appendLine("+" + line)
	}
	for _, line := range newLines[newEnd:newContextEnd] {
		appendLine(" " + line)
	}

	if len(out) == 0 {
		return "", 0, truncated
	}
	return strings.Join(out, "\n") + "\n", len(out), truncated
}

func diffLastPassingSplitLines(data []byte) ([]string, bool) {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > diffLastPassingMaxInputLines {
		return lines[:diffLastPassingMaxInputLines], true
	}
	return lines, false
}
