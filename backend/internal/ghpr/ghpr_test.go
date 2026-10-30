package ghpr

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeGitHub is an in-memory GitHub git-data + pulls API for tests.
type fakeGitHub struct {
	*httptest.Server
	defaultBranch string // empty => repo has no default branch (uninitialized)
	branchHeads   map[string]string
	refRequests   []string
	repoGETCount  int
	baseTreeModes map[string]string
	treeTruncated bool
	treeGETStatus int
	treeRequests  []string
	createdTree   map[string]string
	createdModes  map[string]string
	createdBase   string
	createdBranch string
	commitAuthor  map[string]any
	commitMessage string
	prHead        string
	prBase        string
	prTitle       string
	prDraft       bool
	forkCreated   bool // set when POST /forks is called
	forkPOSTCount int
	forkGETCount  int
	forkGETStatus int
	existingFork  *forkRepository
	treeOwnerRepo string // owner/repo path the tree was created under
	postFailPath  string
	postFailCode  int
}

func newFakeGitHub(t *testing.T, defaultBranch string) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{defaultBranch: defaultBranch}
	f.Server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeGitHub) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	p := r.URL.Path
	if r.Method == http.MethodPost && f.postFailPath != "" && strings.HasSuffix(p, f.postFailPath) {
		http.Error(w, http.StatusText(f.postFailCode), f.postFailCode)
		return
	}
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/user"):
		writeJSON(w, 200, map[string]any{"login": "forker"})
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/forks"):
		f.forkCreated = true
		f.forkPOSTCount++
		writeJSON(w, 202, map[string]any{"name": "r", "owner": map[string]any{"login": "forker"}})
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/repos/forker/r"):
		f.forkGETCount++
		if f.forkGETStatus != 0 {
			http.Error(w, http.StatusText(f.forkGETStatus), f.forkGETStatus)
			return
		}
		if f.existingFork != nil {
			writeJSON(w, http.StatusOK, f.existingFork)
			return
		}
		if !f.forkCreated {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, forkMetadata("forker", "r", "main", "o/r"))
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/repos/o/r"):
		f.repoGETCount++
		writeJSON(w, 200, map[string]any{"default_branch": f.defaultBranch})
	case r.Method == http.MethodGet && strings.Contains(p, "/git/ref/heads/"):
		_, branch, _ := strings.Cut(p, "/git/ref/heads/")
		f.refRequests = append(f.refRequests, branch)
		if sha, ok := f.branchHeads[branch]; ok {
			writeJSON(w, 200, map[string]any{"object": map[string]any{"sha": sha}})
			return
		}
		if f.defaultBranch == "" || branch != f.defaultBranch {
			http.Error(w, "not found", 404)
			return
		}
		writeJSON(w, 200, map[string]any{"object": map[string]any{"sha": "basesha"}})
	case r.Method == http.MethodGet && strings.Contains(p, "/git/commits/"):
		writeJSON(w, 200, map[string]any{"tree": map[string]any{"sha": "basetree"}})
	case r.Method == http.MethodGet && strings.Contains(p, "/git/trees/"):
		f.treeRequests = append(f.treeRequests, p+"?"+r.URL.RawQuery)
		if f.treeGETStatus != 0 {
			http.Error(w, http.StatusText(f.treeGETStatus), f.treeGETStatus)
			return
		}
		entries := make([]map[string]string, 0, len(f.baseTreeModes))
		for path, mode := range f.baseTreeModes {
			kind := "blob"
			switch mode {
			case "040000":
				kind = "tree"
			case "160000":
				kind = "commit"
			}
			entries = append(entries, map[string]string{"path": path, "mode": mode, "type": kind})
		}
		writeJSON(w, 200, map[string]any{"tree": entries, "truncated": f.treeTruncated})
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/git/trees"):
		var in struct {
			BaseTree string `json:"base_tree"`
			Tree     []struct {
				Path    string `json:"path"`
				Mode    string `json:"mode"`
				Content string `json:"content"`
			} `json:"tree"`
		}
		_ = json.Unmarshal(body, &in)
		f.createdTree = map[string]string{}
		f.createdModes = map[string]string{}
		f.createdBase = in.BaseTree
		for _, e := range in.Tree {
			f.createdTree[e.Path] = e.Content
			f.createdModes[e.Path] = e.Mode
		}
		f.treeOwnerRepo = strings.TrimSuffix(strings.TrimPrefix(p, "/repos/"), "/git/trees")
		writeJSON(w, 201, map[string]any{"sha": "newtree"})
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/git/commits"):
		var in struct {
			Message string         `json:"message"`
			Author  map[string]any `json:"author"`
		}
		_ = json.Unmarshal(body, &in)
		f.commitMessage = in.Message
		f.commitAuthor = in.Author
		writeJSON(w, 201, map[string]any{"sha": "newcommit"})
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/git/refs"):
		var in struct {
			Ref string `json:"ref"`
		}
		_ = json.Unmarshal(body, &in)
		f.createdBranch = in.Ref
		writeJSON(w, 201, map[string]any{"ref": in.Ref})
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/pulls"):
		var in struct {
			Title, Body, Head, Base string
			Draft                   bool
		}
		_ = json.Unmarshal(body, &in)
		f.prHead, f.prBase, f.prTitle, f.prDraft = in.Head, in.Base, in.Title, in.Draft
		writeJSON(w, 201, map[string]any{"number": 7, "html_url": "https://github.com/o/r/pull/7"})
	default:
		http.Error(w, "unexpected "+r.Method+" "+p, 500)
	}
}

func forkMetadata(owner, repo, defaultBranch, upstream string) forkRepository {
	var fork forkRepository
	fork.Name = repo
	fork.Fork = true
	fork.DefaultBranch = defaultBranch
	fork.Owner.Login = owner
	fork.Parent.FullName = upstream
	fork.Source.FullName = upstream
	return fork
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func testClient(f *fakeGitHub) *Client {
	c := NewClient(f.Client(), "t")
	c.base = f.URL
	return c
}

func TestOpenPR_HappyPath(t *testing.T) {
	f := newFakeGitHub(t, "main")
	url, err := testClient(f).OpenPR(context.Background(), Request{
		Owner: "o", Repo: "r",
		Files:        map[string]string{"skills/x.yaml": "id: x", "prompts/system.md": "stub"},
		BranchPrefix: "onboard",
		Title:        "title", Body: "body",
	})
	if err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
	if url != "https://github.com/o/r/pull/7" {
		t.Errorf("url = %q", url)
	}
	if f.createdTree["skills/x.yaml"] != "id: x" || f.createdTree["prompts/system.md"] != "stub" {
		t.Errorf("tree missing files: %v", f.createdTree)
	}
	if !strings.HasPrefix(f.createdBranch, "refs/heads/onboard-") {
		t.Errorf("branch = %q, want onboard-*", f.createdBranch)
	}
	if f.prBase != "main" || f.prHead == "" || f.prTitle != "title" {
		t.Errorf("PR base/head/title wrong: base=%q head=%q title=%q", f.prBase, f.prHead, f.prTitle)
	}
	if f.prDraft {
		t.Errorf("PR should not be a draft by default")
	}
	if f.commitAuthor != nil {
		t.Errorf("commit author should be unset by default, got %v", f.commitAuthor)
	}
}

func TestOpenPRErrorsNameOperationAndRepository(t *testing.T) {
	for _, mode := range []struct {
		name string
		fork bool
	}{
		{name: "direct"},
		{name: "fork", fork: true},
	} {
		t.Run(mode.name, func(t *testing.T) {
			for _, tc := range []struct {
				name, path, operation string
				status                int
				hint                  bool
			}{
				{name: "tree", path: "/git/trees", operation: "creating tree", status: http.StatusForbidden},
				{name: "commit", path: "/git/commits", operation: "creating commit", status: http.StatusNotFound, hint: true},
				{name: "ref", path: "/git/refs", operation: "creating branch refs/heads/ai-fix-", status: http.StatusNotFound, hint: true},
				{name: "ref forbidden", path: "/git/refs", operation: "creating branch refs/heads/ai-fix-", status: http.StatusForbidden},
				{name: "pull request", path: "/pulls", operation: "opening pull request", status: http.StatusInternalServerError},
			} {
				t.Run(tc.name, func(t *testing.T) {
					f := newFakeGitHub(t, "main")
					f.postFailPath, f.postFailCode = tc.path, tc.status
					existing := forkMetadata("forker", "r", "main", "o/r")
					f.existingFork = &existing
					client := testClient(f)
					client.token = "private-write-token"
					_, err := client.OpenPR(t.Context(), Request{
						Owner: "o", Repo: "r", Fork: mode.fork, Files: map[string]string{"controller.go": "fixed"},
						BranchPrefix: "ai-fix", Title: "Fix",
					})
					repo := "o/r"
					if mode.fork && tc.path != "/pulls" {
						repo = "forker/r"
					}
					if err == nil || !strings.Contains(err.Error(), tc.operation) || !strings.Contains(err.Error(), " in "+repo) {
						t.Fatalf("error=%v, want operation=%q repo=%q", err, tc.operation, repo)
					}
					if strings.Contains(err.Error(), "workflow scope") != tc.hint ||
						strings.Contains(err.Error(), "private-write-token") {
						t.Fatalf("incorrect hint or leaked token: %v", err)
					}
					if !isResponseStatus(err, tc.status) {
						t.Fatalf("status error was not preserved: %v", err)
					}
				})
			}
		})
	}
}

func TestOpenPRPreservesPinnedBaseFileModes(t *testing.T) {
	for _, tc := range []struct {
		name string
		fork bool
	}{
		{name: "direct"},
		{name: "fork", fork: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitHub(t, "main")
			existing := forkMetadata("forker", "r", "main", "o/r")
			f.existingFork = &existing
			f.baseTreeModes = map[string]string{
				"hack/ensure-azcli.sh": "100755",
				"config.yaml":          "100644",
				"current-config":       "120000",
				"unchanged.sh":         "100755",
			}
			base := &Base{Branch: "release-1.25", HeadSHA: "pinned-head", TreeSHA: "pinned-tree"}
			files := map[string]string{
				"hack/ensure-azcli.sh": "#!/bin/sh\nexit 0\n",
				"config.yaml":          "suite: bookworm\n",
				"current-config":       "config.yaml",
				"hack/new-script.sh":   "#!/bin/sh\nexit 0\n",
			}
			_, err := testClient(f).OpenPR(t.Context(), Request{
				Owner: "o", Repo: "r", Files: files,
				BranchPrefix: "fix", Title: "Fix", Fork: tc.fork, Base: base,
			})
			if err != nil {
				t.Fatal(err)
			}
			for path, want := range map[string]string{
				"hack/ensure-azcli.sh": "100755",
				"config.yaml":          "100644",
				"current-config":       "120000",
				"hack/new-script.sh":   "100644",
			} {
				if got := f.createdModes[path]; got != want {
					t.Errorf("%s mode = %q, want %q", path, got, want)
				}
				if f.createdTree[path] != files[path] {
					t.Errorf("%s content = %q, want %q", path, f.createdTree[path], files[path])
				}
			}
			if len(f.createdTree) != len(files) || f.createdBase != base.TreeSHA {
				t.Errorf("created tree=%v base=%q", f.createdTree, f.createdBase)
			}
			if f.prBase != base.Branch || len(f.refRequests) != 0 || f.repoGETCount != 0 {
				t.Errorf("base not pinned: branch=%q refs=%v repo reads=%d", f.prBase, f.refRequests, f.repoGETCount)
			}
			wantOwner := "o"
			if tc.fork {
				wantOwner = "forker"
			}
			wantLookup := "/repos/" + wantOwner + "/r/git/trees/pinned-tree?recursive=1"
			if len(f.treeRequests) != 1 || f.treeRequests[0] != wantLookup {
				t.Errorf("tree lookups = %v, want [%s]", f.treeRequests, wantLookup)
			}
		})
	}
}

func TestOpenPRRejectsUnavailableFileModes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		truncated bool
		mode      string
		want      string
	}{
		{name: "lookup failed", status: http.StatusForbidden, want: "reading base tree basetree file modes"},
		{name: "truncated tree", truncated: true, want: "cannot preserve file modes"},
		{name: "directory", mode: "040000", want: "cannot update non-file path"},
		{name: "submodule", mode: "160000", want: "cannot update non-file path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitHub(t, "main")
			f.treeGETStatus, f.treeTruncated = tc.status, tc.truncated
			if tc.mode != "" {
				f.baseTreeModes = map[string]string{"hack/ensure-azcli.sh": tc.mode}
			}
			_, err := testClient(f).OpenPR(t.Context(), Request{
				Owner: "o", Repo: "r", Files: map[string]string{"hack/ensure-azcli.sh": "fixed"},
				BranchPrefix: "fix", Title: "Fix",
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if f.createdTree != nil || f.createdBranch != "" || f.prHead != "" {
				t.Fatalf("unexpected write: tree=%v branch=%q PR head=%q", f.createdTree, f.createdBranch, f.prHead)
			}
		})
	}
}

func TestOpenPR_DraftAndAuthorSignOff(t *testing.T) {
	f := newFakeGitHub(t, "main")
	_, err := testClient(f).OpenPR(context.Background(), Request{
		Owner: "o", Repo: "r",
		Files:        map[string]string{"a.txt": "b"},
		BranchPrefix: "fix",
		Title:        "Fix the thing", Body: "body",
		Draft:       true,
		AuthorName:  "Jane Maintainer",
		AuthorEmail: "jane@example.com",
		SignOff:     true,
	})
	if err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
	if !f.prDraft {
		t.Errorf("PR should be a draft")
	}
	if f.commitAuthor["name"] != "Jane Maintainer" || f.commitAuthor["email"] != "jane@example.com" {
		t.Errorf("commit author = %v", f.commitAuthor)
	}
	if !strings.Contains(f.commitMessage, "Signed-off-by: Jane Maintainer <jane@example.com>") {
		t.Errorf("commit message missing sign-off: %q", f.commitMessage)
	}
}

func TestOpenPR_EmptyRepoErrors(t *testing.T) {
	f := newFakeGitHub(t, "") // no default branch
	_, err := testClient(f).OpenPR(context.Background(), Request{
		Owner: "o", Repo: "r", Files: map[string]string{"a": "b"}, BranchPrefix: "x", Title: "t",
	})
	if err == nil || !strings.Contains(err.Error(), "initialize") {
		t.Errorf("expected an initialize-the-repo error, got %v", err)
	}
}

func TestOpenPR_NoToken(t *testing.T) {
	_, err := NewClient(nil, "").OpenPR(context.Background(), Request{
		Owner: "o", Repo: "r", Files: map[string]string{"a": "b"}, BranchPrefix: "x", Title: "t",
	})
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Errorf("expected a token error, got %v", err)
	}
}

func TestOpenPR_NoFiles(t *testing.T) {
	_, err := NewClient(nil, "t").OpenPR(context.Background(), Request{
		Owner: "o", Repo: "r", BranchPrefix: "x", Title: "t",
	})
	if err == nil || !strings.Contains(err.Error(), "no files") {
		t.Errorf("expected a no-files error, got %v", err)
	}
}

func TestOpenPR_ExistingForkSkipsCreation(t *testing.T) {
	f := newFakeGitHub(t, "main")
	existing := forkMetadata("forker", "r", "main", "o/r")
	f.existingFork = &existing

	_, err := testClient(f).OpenPR(context.Background(), Request{
		Owner: "o", Repo: "r", Files: map[string]string{"a.txt": "b"},
		BranchPrefix: "fix", Title: "Fix", Fork: true, Draft: true,
	})
	if err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
	if f.forkPOSTCount != 0 || f.forkCreated {
		t.Fatalf("fork creation calls = %d, created=%v", f.forkPOSTCount, f.forkCreated)
	}
	if f.forkGETCount != 1 {
		t.Fatalf("fork lookup calls = %d, want 1", f.forkGETCount)
	}
	if f.treeOwnerRepo != "forker/r" {
		t.Fatalf("tree pushed to %q, want forker/r", f.treeOwnerRepo)
	}
}

func TestOpenPR_ExistingForkSourceNetworkSkipsCreation(t *testing.T) {
	f := newFakeGitHub(t, "main")
	existing := forkMetadata("forker", "r", "main", "intermediate/r")
	existing.Source.FullName = "o/r"
	f.existingFork = &existing

	_, err := testClient(f).OpenPR(context.Background(), Request{
		Owner: "o", Repo: "r", Files: map[string]string{"a.txt": "b"},
		BranchPrefix: "fix", Title: "Fix", Fork: true, Draft: true,
	})
	if err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
	if f.forkPOSTCount != 0 || f.treeOwnerRepo != "forker/r" {
		t.Fatalf("fork post=%d tree owner=%q", f.forkPOSTCount, f.treeOwnerRepo)
	}
}

func TestOpenPR_ExistingSameNameNonForkFailsClosed(t *testing.T) {
	f := newFakeGitHub(t, "main")
	existing := forkMetadata("forker", "r", "main", "o/r")
	existing.Fork = false
	f.existingFork = &existing

	_, err := testClient(f).OpenPR(context.Background(), Request{
		Owner: "o", Repo: "r", Files: map[string]string{"a.txt": "b"},
		BranchPrefix: "fix", Title: "Fix", Fork: true,
	})
	if err == nil || !strings.Contains(err.Error(), "is not a fork") {
		t.Fatalf("error = %v", err)
	}
	if f.forkPOSTCount != 0 || f.createdTree != nil {
		t.Fatalf("unexpected write after non-fork: post=%d tree=%v", f.forkPOSTCount, f.createdTree)
	}
}

func TestOpenPR_ExistingWrongForkNetworkFailsClosed(t *testing.T) {
	f := newFakeGitHub(t, "main")
	existing := forkMetadata("forker", "r", "main", "other/project")
	f.existingFork = &existing

	_, err := testClient(f).OpenPR(context.Background(), Request{
		Owner: "o", Repo: "r", Files: map[string]string{"a.txt": "b"},
		BranchPrefix: "fix", Title: "Fix", Fork: true,
	})
	if err == nil || !strings.Contains(err.Error(), "not in the o/r fork network") {
		t.Fatalf("error = %v", err)
	}
	if f.forkPOSTCount != 0 || f.createdTree != nil {
		t.Fatalf("unexpected write after wrong fork: post=%d tree=%v", f.forkPOSTCount, f.createdTree)
	}
}

func TestOpenPR_ExistingForkWithoutDefaultBranchFailsClosed(t *testing.T) {
	f := newFakeGitHub(t, "main")
	existing := forkMetadata("forker", "r", "", "o/r")
	f.existingFork = &existing

	_, err := testClient(f).OpenPR(context.Background(), Request{
		Owner: "o", Repo: "r", Files: map[string]string{"a.txt": "b"},
		BranchPrefix: "fix", Title: "Fix", Fork: true,
	})
	if err == nil || !strings.Contains(err.Error(), "has no default branch") {
		t.Fatalf("error = %v", err)
	}
	if f.forkPOSTCount != 0 || f.createdTree != nil {
		t.Fatalf("unexpected write after empty fork: post=%d tree=%v", f.forkPOSTCount, f.createdTree)
	}
}

func TestOpenPR_ForkLookupErrorDoesNotCreateFork(t *testing.T) {
	f := newFakeGitHub(t, "main")
	f.forkGETStatus = http.StatusForbidden

	_, err := testClient(f).OpenPR(context.Background(), Request{
		Owner: "o", Repo: "r", Files: map[string]string{"a.txt": "b"},
		BranchPrefix: "fix", Title: "Fix", Fork: true,
	})
	if err == nil || !strings.Contains(err.Error(), "checking existing fork") || !strings.Contains(err.Error(), "403 Forbidden") {
		t.Fatalf("error = %v", err)
	}
	if f.forkPOSTCount != 0 || f.createdTree != nil {
		t.Fatalf("unexpected write after lookup error: post=%d tree=%v", f.forkPOSTCount, f.createdTree)
	}
}

func TestOpenPR_ForkFlow(t *testing.T) {
	// Shrink the fork-readiness poll so the test doesn't wait on real intervals.
	oldInterval := forkPollInterval
	forkPollInterval = time.Millisecond
	t.Cleanup(func() { forkPollInterval = oldInterval })

	f := newFakeGitHub(t, "main")
	url, err := testClient(f).OpenPR(context.Background(), Request{
		Owner: "o", Repo: "r",
		Files:        map[string]string{"templates/x.yaml": "fixed"},
		BranchPrefix: "fix",
		Title:        "Fix the thing",
		Body:         "body",
		Draft:        true,
		Fork:         true,
		AuthorName:   "Jane Maintainer",
		AuthorEmail:  "jane@example.com",
		SignOff:      true,
	})
	if err != nil {
		t.Fatalf("OpenPR fork: %v", err)
	}
	if url != "https://github.com/o/r/pull/7" {
		t.Errorf("url = %q", url)
	}
	if !f.forkCreated || f.forkPOSTCount != 1 {
		t.Errorf("fork creation = %v, calls=%d", f.forkCreated, f.forkPOSTCount)
	}
	if f.forkGETCount < 2 {
		t.Errorf("fork lookup calls = %d, want preflight and readiness checks", f.forkGETCount)
	}
	// The branch is pushed to the fork (forker/r), not upstream.
	if f.treeOwnerRepo != "forker/r" {
		t.Errorf("tree pushed to %q, want forker/r", f.treeOwnerRepo)
	}
	// The PR head is cross-fork (forker:branch) against the upstream base.
	if !strings.HasPrefix(f.prHead, "forker:fix-") {
		t.Errorf("PR head = %q, want forker:fix-*", f.prHead)
	}
	if f.prBase != "main" {
		t.Errorf("PR base = %q, want main (upstream default)", f.prBase)
	}
	if !f.prDraft {
		t.Errorf("fork PR should be a draft")
	}
	if !strings.Contains(f.commitMessage, "Signed-off-by: Jane Maintainer <jane@example.com>") {
		t.Errorf("commit message missing sign-off: %q", f.commitMessage)
	}
}

func TestCreatePRMissingIdentityIsOutcomeUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	client := NewClient(srv.Client(), "token")
	client.base = srv.URL
	if _, _, err := client.createPR(t.Context(), "o", "r", "title", "body", "head", "main", true); !errors.Is(err, ErrWriteOutcomeUnknown) {
		t.Fatalf("missing identity error = %v", err)
	}
}

func TestOpenPRCreateStatusOutcome(t *testing.T) {
	for _, tc := range []struct {
		status      int
		wantUnknown bool
	}{
		{status: http.StatusUnprocessableEntity},
		{status: http.StatusBadGateway, wantUnknown: true},
		{status: http.StatusGatewayTimeout, wantUnknown: true},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			f := newFakeGitHub(t, "main")
			f.postFailPath = "/pulls"
			f.postFailCode = tc.status
			url, err := testClient(f).OpenPR(t.Context(), Request{
				Owner: "o", Repo: "r", Files: map[string]string{"a.txt": "fixed"},
				BranchPrefix: "fix", Title: "Fix",
			})
			if url != "" || err == nil {
				t.Fatalf("create url=%q err=%v", url, err)
			}
			if errors.Is(err, ErrWriteOutcomeUnknown) != tc.wantUnknown {
				t.Errorf("unknown=%t, want %t: %v", errors.Is(err, ErrWriteOutcomeUnknown), tc.wantUnknown, err)
			}
			if !isResponseStatus(err, tc.status) {
				t.Errorf("status error was not preserved: %v", err)
			}
			for _, context := range []string{"opening pull request in o/r", "POST", "/repos/o/r/pulls", http.StatusText(tc.status)} {
				if !strings.Contains(err.Error(), context) {
					t.Errorf("error %q omitted %q", err, context)
				}
			}
		})
	}
}

// A failure on a release branch has to resolve that branch's head, not the
// default branch's, or its commit compares as diverged.
func TestResolveBaseResolvesRequestedBranch(t *testing.T) {
	f := newFakeGitHub(t, "main")
	f.branchHeads = map[string]string{"release-1.25": "releasesha"}

	base, err := testClient(f).ResolveBase(t.Context(), "o", "r", "release-1.25")
	if err != nil {
		t.Fatalf("ResolveBase: %v", err)
	}
	if base.Branch != "release-1.25" || base.HeadSHA != "releasesha" || base.TreeSHA != "basetree" {
		t.Fatalf("base = %+v", base)
	}
	if f.repoGETCount != 0 {
		t.Errorf("repo metadata reads = %d, want no default-branch lookup", f.repoGETCount)
	}
}

func TestResolveBaseDefaultsToDefaultBranch(t *testing.T) {
	f := newFakeGitHub(t, "main")

	base, err := testClient(f).ResolveBase(t.Context(), "o", "r", "")
	if err != nil {
		t.Fatalf("ResolveBase: %v", err)
	}
	if base.Branch != "main" || base.HeadSHA != "basesha" {
		t.Fatalf("base = %+v", base)
	}
	if f.repoGETCount != 1 {
		t.Errorf("repo metadata reads = %d, want 1", f.repoGETCount)
	}
}

func TestResolveBaseReportsMissingBranch(t *testing.T) {
	f := newFakeGitHub(t, "main")
	if _, err := testClient(f).ResolveBase(t.Context(), "o", "r", "deleted"); !errors.Is(err, ErrBranchNotFound) {
		t.Fatalf("missing branch error=%v", err)
	}
}

func TestResolveCommitTreeUsesPinnedRevision(t *testing.T) {
	client := lifecycleClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/git/commits/pinned" {
			t.Errorf("commit URL=%s", r.URL)
		}
		writeJSON(w, http.StatusOK, map[string]any{"tree": map[string]string{"sha": "pinned-tree"}})
	})
	tree, err := client.ResolveCommitTree(t.Context(), "o", "r", "pinned")
	if err != nil || tree != "pinned-tree" {
		t.Fatalf("tree=%q err=%v", tree, err)
	}
}

// The branch comes from build metadata, so an unsafe name must be rejected
// before it reaches an API path.
func TestResolveBaseRejectsUnsafeBranch(t *testing.T) {
	for _, branch := range []string{
		"../../other", "release/../main", "./main", "foo/./main", "main/.", ".hidden", "main branch",
		"feature?x", "-dash", "a.lock", "nested/a.lock", "trailing.", "with\\backslash", "with:colon",
		"with~tilde", "with^caret", "with*star", "double//slash", "/leading", "trailing/", "main@{1}", "with\x7fdel",
	} {
		t.Run(branch, func(t *testing.T) {
			f := newFakeGitHub(t, "main")
			if _, err := testClient(f).ResolveBase(t.Context(), "o", "r", branch); err == nil {
				t.Fatalf("branch %q was accepted", branch)
			}
			if len(f.refRequests) != 0 || f.repoGETCount != 0 {
				t.Errorf("unsafe branch reached GitHub: refs=%v repo_reads=%d", f.refRequests, f.repoGETCount)
			}
		})
	}
}

func TestResolveBaseEscapesBranchSegments(t *testing.T) {
	// release/1.25 keeps its separator; feature#1 must be escaped or the
	// unescaped # truncates the API path into a fragment.
	for _, branch := range []string{"release/1.25", "feature#1", "fix%20thing"} {
		t.Run(branch, func(t *testing.T) {
			f := newFakeGitHub(t, "main")
			f.branchHeads = map[string]string{branch: "branchsha"}

			base, err := testClient(f).ResolveBase(t.Context(), "o", "r", branch)
			if err != nil {
				t.Fatalf("ResolveBase: %v", err)
			}
			if base.Branch != branch || base.HeadSHA != "branchsha" {
				t.Fatalf("base = %+v", base)
			}
		})
	}
}
