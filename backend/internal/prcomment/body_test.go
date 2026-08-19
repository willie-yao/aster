package prcomment

import (
	"strings"
	"testing"
)

func TestBodyIsSelfContained(t *testing.T) {
	body := Body("contributor", "https://example.github.io/dash", 6566)

	for _, want := range []string{
		"Hi @contributor. Thanks for your PR.",
		"https://example.github.io/dash/pull-requests/6566",
		"<details>",
		"</details>",
		"willie-yao/aster",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body is missing %q:\n%s", want, body)
		}
	}
}

// TestBodyStatesItDoesNotGate keeps the disclaimer in the comment. A bot that
// posts on every pull request must be explicit that it is not blocking merge,
// or it reads as a required check.
func TestBodyStatesItDoesNotGate(t *testing.T) {
	body := Body("someone", "https://example.test", 1)
	for _, want := range []string{"don't run tests", "don't gate this pull request"} {
		if !strings.Contains(body, want) {
			t.Errorf("body no longer says it %q:\n%s", want, body)
		}
	}
}

func TestBodyIsDeterministic(t *testing.T) {
	first := Body("a", "https://example.test", 7)
	second := Body("a", "https://example.test", 7)
	if first != second {
		t.Fatal("body is not deterministic, so a dry run would not show what a live run posts")
	}
}

func TestBodyOmitsGreetingWithoutAuthor(t *testing.T) {
	body := Body("  ", "https://example.test", 7)
	if strings.Contains(body, "Hi @") {
		t.Fatalf("body greets a missing author:\n%s", body)
	}
	if !strings.Contains(body, "/pull-requests/7") {
		t.Fatalf("body dropped the link:\n%s", body)
	}
}

func TestTriagePageURLNormalizesRoot(t *testing.T) {
	cases := []struct {
		name string
		site string
		want string
	}{
		{name: "plain", site: "https://example.test/dash", want: "https://example.test/dash/pull-requests/12"},
		{name: "trailing slash", site: "https://example.test/dash/", want: "https://example.test/dash/pull-requests/12"},
		{name: "surrounding space", site: "  https://example.test/dash  ", want: "https://example.test/dash/pull-requests/12"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := TriagePageURL(tc.site, 12); got != tc.want {
				t.Fatalf("TriagePageURL = %q, want %q", got, tc.want)
			}
		})
	}
}
