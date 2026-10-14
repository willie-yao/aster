package pr

import (
	"context"
	"strings"
	"testing"

	"github.com/willie-yao/aster/backend/internal/ghpr"
	"github.com/willie-yao/aster/backend/internal/models"
)

const bodyTestProwURL = "https://prow.example/view/gs/builds/123"

type testPRFiller struct {
	replacement string
}

func (f testPRFiller) FillBody(_ context.Context, description string) string {
	if f.replacement != "" {
		return f.replacement
	}
	return "## Repository template\n\n" + description
}

func fixBodyGenerators() map[string]func(context.Context, *Manager, string) (*GeneratedFix, error) {
	return map[string]func(context.Context, *Manager, string) (*GeneratedFix, error){
		"analysis": func(ctx context.Context, manager *Manager, prowURL string) (*GeneratedFix, error) {
			failure := validAnalysisFailure()
			failure.ProwURL = prowURL
			failure.SourceBranch = "release-1.24"
			return manager.GenerateAnalysisPreview(ctx, failure, "")
		},
		"build": func(ctx context.Context, manager *Manager, prowURL string) (*GeneratedFix, error) {
			return manager.GenerateBuildPreview(ctx, BuildFailure{
				ID: "build::id", JobID: "periodic-capz", JobName: "periodic-capz", BuildID: "123", ProwURL: prowURL,
			}, "")
		},
		"pattern": func(ctx context.Context, manager *Manager, prowURL string) (*GeneratedFix, error) {
			pattern := systemicPattern("etcd")
			pattern.SharedBuilds = []string{"123", "124", "125", "126", "127", "128"}
			return manager.GeneratePreview(ctx, pattern, "",
				models.BuildInfo{BuildID: "123", ProwURL: prowURL},
				models.BuildInfo{BuildID: "unrelated", ProwURL: "https://prow.example/unrelated"},
				models.BuildInfo{BuildID: "128", ProwURL: "https://prow.example/128"},
			)
		},
	}
}

func TestFixPRBodyRecordedBuildLinks(t *testing.T) {
	for kind, generate := range fixBodyGenerators() {
		for _, prowURL := range []string{"", bodyTestProwURL} {
			t.Run(kind+"/url="+prowURL, func(t *testing.T) {
				client := &fakePR{base: ghpr.Base{Branch: "main", HeadSHA: exactAnalysisRevision, TreeSHA: "tree"}}
				manager := newManager(t, client, goodAgent(), Options{PRFiller: testPRFiller{}})
				fix, err := generate(t.Context(), manager, prowURL)
				if err != nil {
					t.Fatal(err)
				}
				want := "build `123`"
				if prowURL != "" {
					want = "[build 123](<" + prowURL + ">)"
				}
				if !strings.Contains(fix.Description, want) || strings.Count(fix.Body, want) != 2 {
					t.Fatalf("missing build source %q: %s", want, fix.Body)
				}
				if prowURL == "" && strings.Contains(fix.Body, "[build 123](") {
					t.Fatalf("invented Prow link: %s", fix.Body)
				}
				if kind == "pattern" {
					if !strings.Contains(fix.Body, "build `127` and 1 more") ||
						strings.Contains(fix.Body, "https://prow.example/128") ||
						strings.Contains(fix.Body, "https://prow.example/unrelated") {
						t.Fatalf("pattern sources were not bounded to listed builds: %s", fix.Body)
					}
				} else if !strings.Contains(fix.Body, "of `periodic-capz`") {
					t.Fatalf("missing job name: %s", fix.Body)
				}
				if !strings.Contains(fix.Body, "AI coding agent in Aster") || strings.Contains(fix.Body, "[Aster](") {
					t.Fatalf("missing unlinked Aster disclosure: %s", fix.Body)
				}
				assertNoFixCallouts(t, fix.Body)
			})
		}
	}
}

func TestFixPRBodyEngineOwnedTextSurvivesTemplateFilling(t *testing.T) {
	for kind, generate := range fixBodyGenerators() {
		for _, status := range []VerifyStatus{VerifyPassed, VerifyFailed, VerifySkipped} {
			t.Run(kind+"/"+string(status), func(t *testing.T) {
				client := &fakePR{base: ghpr.Base{Branch: "main", HeadSHA: exactAnalysisRevision, TreeSHA: "tree"}}
				agent := goodAgent()
				manager := newManager(t, client, agent, Options{
					DashboardURL: "https://aster.example",
					PRFiller:     testPRFiller{replacement: "## Repository template\n\nA minimal patch."},
				})
				if status != VerifySkipped {
					agent.res.BaseSHA = exactAnalysisRevision
					agent.res.CommandResults = sandboxCommandResults()
					if status == VerifyFailed {
						agent.res.CommandResults[0].ExitCode = 1
					}
					manager.opts.Agent.RequireCommandResults = true
					manager.opts.Agent.CommandPolicy.Commands = sandboxVerificationCommands()
				}
				fix, err := generate(t.Context(), manager, bodyTestProwURL)
				if err != nil {
					t.Fatal(err)
				}
				if fix.Preview.Verify.Status != status || !strings.Contains(fix.Body, "## Repository template") {
					t.Fatalf("template or verification changed: %s", fix.Body)
				}
				if strings.Contains(fix.Description, bodyTestProwURL) {
					t.Fatalf("replacement template retained the source link: %s", fix.Description)
				}
				source := "[build 123](<" + bodyTestProwURL + ">) of `periodic-capz`"
				if kind == "pattern" {
					source = "[build 123](<" + bodyTestProwURL + ">), build `124`, build `125`, build `126`, build `127` and 1 more"
				}
				if status == VerifySkipped {
					if strings.Contains(fix.Body, "**Automated verification:**") {
						t.Fatalf("skipped verification added a verdict: %s", fix.Body)
					}
				} else if !strings.Contains(fix.Body, "**Automated verification:** "+string(status)+":") {
					t.Fatalf("template dropped verification: %s", fix.Body)
				}
				if status == VerifyFailed && !strings.Contains(fix.Body, "treat it as a lead, not a fix") {
					t.Fatalf("failed verification lost qualification: %s", fix.Body)
				}
				if kind == "analysis" {
					if !strings.Contains(fix.Description, "comment `/cherry-pick release-1.24`") ||
						!strings.Contains(fix.Body, "comment `/cherry-pick release-1.24`") {
						t.Fatalf("template dropped backport suggestion: %s", fix.Body)
					}
					for line := range strings.SplitSeq(fix.Body, "\n") {
						if strings.HasPrefix(strings.TrimSpace(line), "/cherry-pick") {
							t.Fatalf("backport suggestion became a Prow command: %q", line)
						}
					}
				}
				for _, want := range []string{
					"AI coding agent in [Aster](<https://aster.example>)",
					"from an automated analysis of " + source + ".",
					"A maintainer reviewed the preview before this draft PR was opened; verify it before merging.",
					markerFor(fix.key),
				} {
					if !strings.Contains(fix.Body, want) {
						t.Fatalf("template dropped footer text %q: %s", want, fix.Body)
					}
				}
				fix.SetWarnings([]string{"The selected finding is unverified."})
				fix.SetWarnings([]string{"Source hints are missing."})
				if strings.Count(fix.Body, "**Investigation warnings:**") != 1 ||
					!strings.Contains(fix.Body, "The selected finding is unverified.") ||
					!strings.Contains(fix.Body, "Source hints are missing.") {
					t.Fatalf("investigation warnings were lost or duplicated: %s", fix.Body)
				}
				assertNoFixCallouts(t, fix.Body)
			})
		}
	}
}

func TestFixPRBodyFooterWithoutProwURLSurvivesTemplateFilling(t *testing.T) {
	for kind, generate := range fixBodyGenerators() {
		t.Run(kind, func(t *testing.T) {
			client := &fakePR{base: ghpr.Base{Branch: "main", HeadSHA: exactAnalysisRevision, TreeSHA: "tree"}}
			manager := newManager(t, client, goodAgent(), Options{
				PRFiller: testPRFiller{replacement: "## Repository template\n\nA minimal patch."},
			})
			fix, err := generate(t.Context(), manager, "")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(fix.Body, "from an automated analysis of build `123`") ||
				strings.Contains(fix.Body, "https://prow.example") {
				t.Fatalf("footer dropped build identity or invented a link: %s", fix.Body)
			}
		})
	}
}

func assertNoFixCallouts(t *testing.T, body string) {
	t.Helper()
	for _, callout := range []string{"> [!NOTE]", "> [!WARNING]", "> [!CAUTION]"} {
		if strings.Contains(body, callout) {
			t.Fatalf("Fix PR retained a callout %q: %s", callout, body)
		}
	}
}
