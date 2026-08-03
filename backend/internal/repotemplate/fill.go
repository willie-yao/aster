package repotemplate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/actiondraft"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
)

// Completer is the subset of the AI client this package needs.
type Completer interface {
	CompleteStructured(ctx context.Context, system, user string, format ai.ResponseFormat, validate ai.StructuredValidator) error
}

// fillTimeout bounds the optional reformat call so a slow or hung template fill
// falls back without consuming the caller's remaining budget for the GitHub
// create/open request.
const fillTimeout = 60 * time.Second

const prFillSystem = `You reformat a proposed pull-request description to follow a repository's PULL REQUEST template. Rules: keep all factual content from the description; fill the template's sections using that content; for sections you have no information for, leave the template's placeholder text, HTML comments, and checklists intact; for Prow-style "/kind" lines choose the single most fitting kind; do not invent issue numbers or facts. Return one JSON object with a single "body" field containing the filled markdown. Do not add commentary or code fences outside the JSON object.`

const issueFillSystem = `You reformat a bug/failure report to follow a repository's ISSUE template. You are given one or more candidate templates and the report content. Choose the single most appropriate template, then fill its sections using the report; leave placeholder text, HTML comments, and checklists intact where you lack information; choose the single most fitting "/kind" line if present; do not invent facts. Answer with one JSON object only: {"title": "<concise issue title>", "body": "<filled markdown>"}.`

// FillPR reformats description to follow the PR template. It returns the
// original description unchanged when c is nil, the template is empty, or the
// call fails.
func FillPR(ctx context.Context, c Completer, template, description string) string {
	if c == nil || strings.TrimSpace(template) == "" || strings.TrimSpace(description) == "" {
		return description
	}
	user := "PULL REQUEST TEMPLATE:\n" + template + "\n\nDESCRIPTION TO FIT INTO THE TEMPLATE:\n" + description
	ctx, cancel := context.WithTimeout(ctx, fillTimeout)
	defer cancel()
	var result struct {
		Body string `json:"body"`
	}
	err := c.CompleteStructured(ctx, prFillSystem, user, bodyResponseFormat("format_pull_request"), func(raw json.RawMessage) error {
		var candidate struct {
			Body string `json:"body"`
		}
		if err := decodeStructuredObject(raw, &candidate); err != nil {
			return err
		}
		candidate.Body = strings.TrimSpace(candidate.Body)
		if err := actiondraft.ValidateBody(candidate.Body); err != nil {
			return err
		}
		result = candidate
		return nil
	})
	if err != nil {
		log.Printf("repotemplate: PR fill rejected, using default body")
		return description
	}
	return result.Body
}

// FillIssue picks the best-fit template and reformats the report to follow it.
// It returns the original title and body unchanged when c is nil, there are no
// templates, or the call fails.
func FillIssue(ctx context.Context, c Completer, templates []Template, title, body string) (string, string) {
	if c == nil || len(templates) == 0 || strings.TrimSpace(body) == "" {
		return title, body
	}
	var sb strings.Builder
	for i, t := range templates {
		sb.WriteString("=== TEMPLATE ")
		sb.WriteString(t.Name)
		if i == 0 {
			sb.WriteString(" (index 0)")
		}
		sb.WriteString(" ===\n")
		sb.WriteString(t.Body)
		sb.WriteString("\n")
	}
	user := "CANDIDATE TEMPLATES:\n" + sb.String() + "\nREPORT TITLE: " + title + "\nREPORT CONTENT:\n" + body
	ctx, cancel := context.WithTimeout(ctx, fillTimeout)
	defer cancel()
	var result struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	err := c.CompleteStructured(ctx, issueFillSystem, user, issueResponseFormat(), func(raw json.RawMessage) error {
		var candidate struct {
			Title string `json:"title"`
			Body  string `json:"body"`
		}
		if err := decodeStructuredObject(raw, &candidate); err != nil {
			return err
		}
		candidate.Title = strings.TrimSpace(candidate.Title)
		candidate.Body = strings.TrimSpace(candidate.Body)
		if err := actiondraft.ValidateTitleBody(candidate.Title, candidate.Body); err != nil {
			return err
		}
		result = candidate
		return nil
	})
	if err != nil {
		log.Printf("repotemplate: issue fill rejected, using default body")
		return title, body
	}
	return result.Title, result.Body
}

func bodyResponseFormat(name string) ai.ResponseFormat {
	return ai.ResponseFormat{Name: name, Description: "Return the validated GitHub markdown body.", Schema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"body": map[string]any{"type": "string"},
		},
		"required": []string{"body"}, "additionalProperties": false,
	}}
}

func issueResponseFormat() ai.ResponseFormat {
	return ai.ResponseFormat{Name: "format_issue", Description: "Return the validated issue title and GitHub markdown body.", Schema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"title": map[string]any{"type": "string"},
			"body":  map[string]any{"type": "string"},
		},
		"required": []string{"title", "body"}, "additionalProperties": false,
	}}
}

func decodeStructuredObject(raw json.RawMessage, dst any) error {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("unexpected trailing JSON")
	}
	return nil
}

// PRFiller binds a fetcher and completer to one repo for PR-body templating. It
// lazily loads the template once. A nil *PRFiller is a no-op, so callers can
// pass one unconditionally.
type PRFiller struct {
	f     *Fetcher
	c     Completer
	owner string
	repo  string

	once sync.Once
	tmpl string
	has  bool
}

// NewPRFiller returns a filler, or nil when no completer is available (which
// makes FillBody a pass-through).
func NewPRFiller(token string, c Completer, owner, repo string) *PRFiller {
	if c == nil {
		return nil
	}
	return &PRFiller{f: NewFetcher(token), c: c, owner: owner, repo: repo}
}

// FillBody returns description reformatted to follow the repo PR template, or
// description unchanged when there is no template or on any error.
func (p *PRFiller) FillBody(ctx context.Context, description string) string {
	if p == nil {
		return description
	}
	p.once.Do(func() {
		t, ok, err := p.f.PRTemplate(ctx, p.owner, p.repo)
		if err != nil {
			log.Printf("repotemplate: fetching PR template failed: %v", err)
			return
		}
		p.tmpl, p.has = t, ok
	})
	if !p.has {
		return description
	}
	return FillPR(ctx, p.c, p.tmpl, description)
}

// IssueFiller binds a fetcher and completer to one repo for issue templating.
type IssueFiller struct {
	f     *Fetcher
	c     Completer
	owner string
	repo  string

	once      sync.Once
	templates []Template
}

// NewIssueFiller returns a filler, or nil when no completer is available.
func NewIssueFiller(token string, c Completer, owner, repo string) *IssueFiller {
	if c == nil {
		return nil
	}
	return &IssueFiller{f: NewFetcher(token), c: c, owner: owner, repo: repo}
}

// FillIssue returns title and body reformatted to follow the best-fit repo
// issue template, or unchanged when there is no template or on any error.
func (i *IssueFiller) FillIssue(ctx context.Context, title, body string) (string, string) {
	if i == nil {
		return title, body
	}
	i.once.Do(func() {
		ts, err := i.f.IssueTemplates(ctx, i.owner, i.repo)
		if err != nil {
			log.Printf("repotemplate: fetching issue templates failed: %v", err)
			return
		}
		i.templates = ts
	})
	if len(i.templates) == 0 {
		return title, body
	}
	return FillIssue(ctx, i.c, i.templates, title, body)
}
