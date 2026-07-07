package repotemplate

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"sync"
	"time"
)

// Completer is the subset of the AI client this package needs.
type Completer interface {
	Complete(ctx context.Context, system, user string) (string, error)
}

// fillTimeout bounds the optional reformat call so a slow or hung template fill
// falls back without consuming the caller's remaining budget for the GitHub
// create/open request.
const fillTimeout = 60 * time.Second

const prFillSystem = `You reformat a proposed pull-request description to follow a repository's PULL REQUEST template. Rules: keep all factual content from the description; fill the template's sections using that content; for sections you have no information for, leave the template's placeholder text, HTML comments, and checklists intact; for Prow-style "/kind" lines choose the single most fitting kind; do not invent issue numbers or facts. Output only the filled markdown, with no code fences and no commentary.`

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
	out, err := c.Complete(ctx, prFillSystem, user)
	if err != nil {
		log.Printf("repotemplate: PR fill failed, using default body: %v", err)
		return description
	}
	out = stripCodeFence(strings.TrimSpace(out))
	if out == "" {
		return description
	}
	return out
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
	out, err := c.Complete(ctx, issueFillSystem, user)
	if err != nil {
		log.Printf("repotemplate: issue fill failed, using default body: %v", err)
		return title, body
	}
	var v struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	if err := parseJSONObject(out, &v); err != nil || strings.TrimSpace(v.Body) == "" {
		log.Printf("repotemplate: issue fill returned no usable body, using default")
		return title, body
	}
	newTitle := title
	if strings.TrimSpace(v.Title) != "" {
		newTitle = strings.TrimSpace(v.Title)
	}
	return newTitle, v.Body
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

// stripCodeFence removes a wrapping ```...``` fence if the model added one.
func stripCodeFence(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSuffix(strings.TrimRight(s, "\n"), "```")
}

// parseJSONObject extracts and decodes the first JSON object in s.
func parseJSONObject(s string, v any) error {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end < start {
		return errNoJSON
	}
	return json.Unmarshal([]byte(s[start:end+1]), v)
}

var errNoJSON = jsonErr("no JSON object in response")

type jsonErr string

func (e jsonErr) Error() string { return string(e) }
