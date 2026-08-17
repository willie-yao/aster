package prowbuild

import "testing"

func TestPullHeadRevision(t *testing.T) {
	const (
		base = "3333333333333333333333333333333333333333"
		head = "1111111111111111111111111111111111111111"
		repo = "example/project"
	)
	composite := map[string]string{repo: "main:" + base + ",42:" + head}

	cases := []struct {
		name       string
		refs       map[string]string
		repo       string
		pullNumber string
		want       string
	}{
		{name: "composite presubmit ref", refs: composite, repo: repo, pullNumber: "42", want: head},
		{name: "case insensitive repository", refs: composite, repo: "Example/Project", pullNumber: "42", want: head},
		{name: "merge commit suffix", repo: repo, pullNumber: "42",
			refs: map[string]string{repo: "main:" + base + ",42:" + head + "444444444444444444444444"},
			want: head + "444444444444444444444444"},
		{name: "uppercase normalized", repo: repo, pullNumber: "42",
			refs: map[string]string{repo: "main:" + base + ",42:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},
			want: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{name: "other pull number", refs: composite, repo: repo, pullNumber: "43"},
		{name: "other repository", refs: composite, repo: "example/other", pullNumber: "42"},
		{name: "periodic ref has no pull segment", repo: repo, pullNumber: "42",
			refs: map[string]string{repo: "main:" + base}},
		{name: "malformed revision", repo: repo, pullNumber: "42",
			refs: map[string]string{repo: "main:" + base + ",42:notasha"}},
		{name: "empty repo", refs: composite, pullNumber: "42"},
		{name: "empty pull number", refs: composite, repo: repo},
		{name: "no refs", repo: repo, pullNumber: "42"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := PullHeadRevision(tc.refs, tc.repo, tc.pullNumber)
			if tc.want == "" {
				if ok {
					t.Fatalf("PullHeadRevision = %q, want no revision", got)
				}
				return
			}
			if !ok || got != tc.want {
				t.Fatalf("PullHeadRevision = %q ok=%t, want %q", got, ok, tc.want)
			}
		})
	}
}
