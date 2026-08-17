package jobconfig

import "testing"

func TestBranchSelectorMatches(t *testing.T) {
	cases := []struct {
		name         string
		branches     []string
		skipBranches []string
		branch       string
		want         bool
	}{
		{name: "no selectors admits everything", branch: "main", want: true},
		{name: "branches allows a match", branches: []string{"^main$"}, branch: "main", want: true},
		{name: "branches rejects a non-match", branches: []string{"^release-.*$"}, branch: "main"},
		{name: "skip wins over branches", branches: []string{"^main$"}, skipBranches: []string{"^main$"}, branch: "main"},
		{name: "skip alone rejects", skipBranches: []string{"^release-.*$"}, branch: "release-1.0"},
		{name: "skip alone admits others", skipBranches: []string{"^release-.*$"}, branch: "main", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := BranchSelectorMatches("job", tc.branches, tc.skipBranches, tc.branch)
			if err != nil {
				t.Fatalf("BranchSelectorMatches: %v", err)
			}
			if got != tc.want {
				t.Fatalf("BranchSelectorMatches = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestBranchSelectorMatchesReportsInvalidPatterns(t *testing.T) {
	if _, err := BranchSelectorMatches("job", []string{"["}, nil, "main"); err == nil {
		t.Error("want an error for an invalid branches selector")
	}
	if _, err := BranchSelectorMatches("job", nil, []string{"["}, "main"); err == nil {
		t.Error("want an error for an invalid skip_branches selector")
	}
}

func TestJobDefinitionAppliesToBranch(t *testing.T) {
	job := JobDefinition{Name: "pull-release", Branches: []string{"^release-.*$"}}
	if ok, err := job.AppliesToBranch("release-1.0"); err != nil || !ok {
		t.Fatalf("AppliesToBranch(release-1.0) = %t, %v", ok, err)
	}
	if ok, err := job.AppliesToBranch("main"); err != nil || ok {
		t.Fatalf("AppliesToBranch(main) = %t, %v", ok, err)
	}
}
