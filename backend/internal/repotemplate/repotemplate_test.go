package repotemplate

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// contentsServer serves the GitHub contents API for a fixed file/dir set.
// files maps repo path -> content; dirs maps dir path -> child file paths.
func contentsServer(t *testing.T, files map[string]string, dirs map[string][]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /repos/{o}/{r}/contents/{path}
		i := strings.Index(r.URL.Path, "/contents/")
		if i < 0 {
			http.NotFound(w, r)
			return
		}
		p := r.URL.Path[i+len("/contents/"):]
		if children, ok := dirs[p]; ok {
			var entries []dirEntry
			for _, c := range children {
				entries = append(entries, dirEntry{Name: c[strings.LastIndex(c, "/")+1:], Path: c, Type: "file"})
			}
			_ = json.NewEncoder(w).Encode(entries)
			return
		}
		if body, ok := files[p]; ok {
			_ = json.NewEncoder(w).Encode(map[string]string{
				"content":  base64.StdEncoding.EncodeToString([]byte(body)),
				"encoding": "base64",
				"type":     "file",
			})
			return
		}
		http.NotFound(w, r)
	}))
}

func fetcherFor(srv *httptest.Server) *Fetcher {
	return &Fetcher{client: srv.Client(), token: "", base: srv.URL}
}

func TestFetcher_PRTemplate(t *testing.T) {
	srv := contentsServer(t, map[string]string{
		".github/PULL_REQUEST_TEMPLATE.md": "**What this PR does**:\n",
	}, nil)
	defer srv.Close()
	body, ok, err := fetcherFor(srv).PRTemplate(context.Background(), "o", "r")
	if err != nil || !ok {
		t.Fatalf("PRTemplate ok=%v err=%v", ok, err)
	}
	if !strings.Contains(body, "What this PR does") {
		t.Errorf("body = %q", body)
	}
}

func TestFetcher_PRTemplate_None(t *testing.T) {
	srv := contentsServer(t, map[string]string{}, nil)
	defer srv.Close()
	_, ok, err := fetcherFor(srv).PRTemplate(context.Background(), "o", "r")
	if err != nil || ok {
		t.Fatalf("expected no PR template, ok=%v err=%v", ok, err)
	}
}

func TestFetcher_IssueTemplates_DirMarkdownOnly(t *testing.T) {
	srv := contentsServer(t,
		map[string]string{
			".github/ISSUE_TEMPLATE/bug.md":   "---\nname: Bug Report\n---\n**What happened**:\n",
			".github/ISSUE_TEMPLATE/flaky.md": "---\nname: Flaky Test\n---\n**Which tests are flaky**:\n",
		},
		map[string][]string{
			".github/ISSUE_TEMPLATE": {
				".github/ISSUE_TEMPLATE/bug.md",
				".github/ISSUE_TEMPLATE/flaky.md",
				".github/ISSUE_TEMPLATE/form.yml", // must be skipped
				".github/ISSUE_TEMPLATE/config.yml",
			},
		},
	)
	defer srv.Close()
	ts, err := fetcherFor(srv).IssueTemplates(context.Background(), "o", "r")
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 2 {
		t.Fatalf("want 2 markdown templates, got %d", len(ts))
	}
	names := ts[0].Name + "," + ts[1].Name
	if !strings.Contains(names, "Bug Report") || !strings.Contains(names, "Flaky Test") {
		t.Errorf("names = %q", names)
	}
	// Front-matter stripped.
	if strings.Contains(ts[0].Body, "name:") {
		t.Errorf("front-matter not stripped: %q", ts[0].Body)
	}
}

func TestStripFrontMatter(t *testing.T) {
	in := "---\nname: X\nabout: Y\n---\n\nbody here\n"
	if got := stripFrontMatter(in); strings.Contains(got, "name:") || !strings.Contains(got, "body here") {
		t.Errorf("stripFrontMatter = %q", got)
	}
	noFM := "just body\n"
	if got := stripFrontMatter(noFM); got != noFM {
		t.Errorf("no-front-matter changed: %q", got)
	}
}

// fakeCompleter returns a canned response, or an error.
type fakeCompleter struct {
	resp string
	err  error
}

func (f fakeCompleter) Complete(context.Context, string, string) (string, error) {
	return f.resp, f.err
}

func TestFillPR_Fallbacks(t *testing.T) {
	// nil completer -> unchanged.
	if got := FillPR(context.Background(), nil, "tmpl", "desc"); got != "desc" {
		t.Errorf("nil completer changed body: %q", got)
	}
	// error -> unchanged.
	if got := FillPR(context.Background(), fakeCompleter{err: context.DeadlineExceeded}, "tmpl", "desc"); got != "desc" {
		t.Errorf("error did not fall back: %q", got)
	}
	// success -> filled.
	if got := FillPR(context.Background(), fakeCompleter{resp: "**What this PR does**: fixes it"}, "tmpl", "desc"); !strings.Contains(got, "fixes it") {
		t.Errorf("fill not applied: %q", got)
	}
	// code fence stripped.
	if got := FillPR(context.Background(), fakeCompleter{resp: "```md\nfilled\n```"}, "tmpl", "desc"); strings.Contains(got, "```") {
		t.Errorf("code fence not stripped: %q", got)
	}
}

func TestFillIssue_PicksAndFills(t *testing.T) {
	tmpls := []Template{{Name: "Bug", Body: "**What happened**:"}}
	c := fakeCompleter{resp: `{"title": "etcd join times out", "body": "**What happened**: etcd learner promotion timeout"}`}
	title, body := FillIssue(context.Background(), c, tmpls, "orig title", "orig body")
	if title != "etcd join times out" || !strings.Contains(body, "learner promotion") {
		t.Errorf("fill = %q / %q", title, body)
	}
	// nil completer or no templates -> unchanged.
	if tt, bb := FillIssue(context.Background(), nil, tmpls, "t", "b"); tt != "t" || bb != "b" {
		t.Errorf("nil completer changed: %q %q", tt, bb)
	}
	if tt, bb := FillIssue(context.Background(), c, nil, "t", "b"); tt != "t" || bb != "b" {
		t.Errorf("no templates changed: %q %q", tt, bb)
	}
	// bad JSON -> unchanged.
	if tt, bb := FillIssue(context.Background(), fakeCompleter{resp: "not json"}, tmpls, "t", "b"); tt != "t" || bb != "b" {
		t.Errorf("bad JSON did not fall back: %q %q", tt, bb)
	}
}

func TestPRFiller_NilIsPassThrough(t *testing.T) {
	var pf *PRFiller
	if got := pf.FillBody(context.Background(), "desc"); got != "desc" {
		t.Errorf("nil PRFiller changed body: %q", got)
	}
	if NewPRFiller("tok", nil, "o", "r") != nil {
		t.Error("NewPRFiller with nil completer should be nil")
	}
}
