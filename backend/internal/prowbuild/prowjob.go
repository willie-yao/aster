package prowbuild

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/storage"
)

const maxProwJobBytes = 2 * 1024 * 1024

// ErrProwJobNotFound means the build did not publish prowjob.json.
var ErrProwJobNotFound = errors.New("prowjob metadata not found")

// ProwJobMetadata is the verification metadata published with a decorated build.
type ProwJobMetadata struct {
	Name         string
	Type         string
	Job          string
	Context      string
	RerunCommand string
	Cluster      string
	Report       bool
	Refs         *ProwRef
	ExtraRefs    []ProwRef
	State        string
	Description  string
	URL          string
	BuildID      string
	StartTime    time.Time
	Completion   time.Time
}

// ProwRef identifies one repository checkout in a ProwJob.
type ProwRef struct {
	Org     string
	Repo    string
	BaseRef string
	BaseSHA string
	Pulls   []ProwPull
}

// FullRepo returns the ref as "org/repo".
func (r ProwRef) FullRepo() string {
	if r.Org == "" || r.Repo == "" {
		return ""
	}
	return r.Org + "/" + r.Repo
}

// ProwPull identifies a pull request revision in a ProwJob ref.
type ProwPull struct {
	Number  int
	Author  string
	SHA     string
	HeadRef string
	Link    string
}

type prowJobJSON struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Type         string        `json:"type"`
		Job          string        `json:"job"`
		Context      string        `json:"context"`
		RerunCommand string        `json:"rerun_command"`
		Cluster      string        `json:"cluster"`
		Report       bool          `json:"report"`
		Refs         *prowRefJSON  `json:"refs"`
		ExtraRefs    []prowRefJSON `json:"extra_refs"`
	} `json:"spec"`
	Status struct {
		State          string `json:"state"`
		Description    string `json:"description"`
		URL            string `json:"url"`
		BuildID        string `json:"build_id"`
		StartTime      string `json:"startTime"`
		CompletionTime string `json:"completionTime"`
	} `json:"status"`
}

type prowRefJSON struct {
	Org     string         `json:"org"`
	Repo    string         `json:"repo"`
	BaseRef string         `json:"base_ref"`
	BaseSHA string         `json:"base_sha"`
	Pulls   []prowPullJSON `json:"pulls"`
}

type prowPullJSON struct {
	Number  int    `json:"number"`
	Author  string `json:"author"`
	SHA     string `json:"sha"`
	HeadRef string `json:"head_ref"`
	Link    string `json:"link"`
}

// FetchProwJobMetadata reads and validates the build's prowjob.json.
func FetchProwJobMetadata(ctx context.Context, b storage.Backend, loc BuildLocation) (*ProwJobMetadata, error) {
	path := loc.BuildPath() + "prowjob.json"
	rc, size, err := b.Open(ctx, path)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, fmt.Errorf("%w: %s", ErrProwJobNotFound, path)
		}
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	defer rc.Close()
	if size > maxProwJobBytes {
		return nil, fmt.Errorf("prowjob: %s is %d bytes, exceeds %d-byte limit", path, size, maxProwJobBytes)
	}
	data, err := io.ReadAll(io.LimitReader(rc, maxProwJobBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if len(data) > maxProwJobBytes {
		return nil, fmt.Errorf("prowjob: %s exceeds %d-byte limit", path, maxProwJobBytes)
	}
	var raw prowJobJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if raw.Spec.Job == "" || raw.Spec.Type == "" {
		return nil, fmt.Errorf("prowjob: %s is missing spec.job or spec.type", path)
	}
	if loc.JobName != "" && raw.Spec.Job != loc.JobName {
		return nil, fmt.Errorf("prowjob: %s job is %q, want %q", path, raw.Spec.Job, loc.JobName)
	}
	out := &ProwJobMetadata{
		Name: raw.Metadata.Name, Type: strings.ToLower(raw.Spec.Type), Job: raw.Spec.Job,
		Context: raw.Spec.Context, RerunCommand: raw.Spec.RerunCommand,
		Cluster: raw.Spec.Cluster, Report: raw.Spec.Report,
		State: strings.ToLower(raw.Status.State), Description: raw.Status.Description,
		URL: raw.Status.URL, BuildID: raw.Status.BuildID,
		StartTime: parseProwTime(raw.Status.StartTime), Completion: parseProwTime(raw.Status.CompletionTime),
	}
	if loc.JobType != "" && out.Type != loc.JobType {
		return nil, fmt.Errorf("prowjob: %s type is %q, want %q", path, out.Type, loc.JobType)
	}
	if raw.Spec.Refs != nil {
		ref := convertProwRef(*raw.Spec.Refs)
		out.Refs = &ref
	}
	for _, ref := range raw.Spec.ExtraRefs {
		out.ExtraRefs = append(out.ExtraRefs, convertProwRef(ref))
	}
	if out.Type == "presubmit" && (out.Refs == nil || len(out.Refs.Pulls) == 0) {
		return nil, fmt.Errorf("prowjob: %s presubmit is missing pull refs", path)
	}
	if out.Type == "presubmit" && strings.TrimSpace(out.Refs.Pulls[0].SHA) == "" {
		return nil, fmt.Errorf("prowjob: %s presubmit is missing pull head SHA", path)
	}
	if loc.Repo != "" && (out.Refs == nil || out.Refs.FullRepo() != loc.Repo) {
		got := ""
		if out.Refs != nil {
			got = out.Refs.FullRepo()
		}
		return nil, fmt.Errorf("prowjob: %s repo is %q, want %q", path, got, loc.Repo)
	}
	if loc.PullNumber != "" && out.Refs != nil && len(out.Refs.Pulls) > 0 {
		if got := fmt.Sprint(out.Refs.Pulls[0].Number); got != loc.PullNumber {
			return nil, fmt.Errorf("prowjob: %s pull is %s, want %s", path, got, loc.PullNumber)
		}
	}
	return out, nil
}

func convertProwRef(raw prowRefJSON) ProwRef {
	out := ProwRef{Org: raw.Org, Repo: raw.Repo, BaseRef: raw.BaseRef, BaseSHA: raw.BaseSHA}
	for _, pull := range raw.Pulls {
		out.Pulls = append(out.Pulls, ProwPull(pull))
	}
	return out
}

func parseProwTime(value string) time.Time {
	parsed, _ := time.Parse(time.RFC3339, value)
	return parsed
}
