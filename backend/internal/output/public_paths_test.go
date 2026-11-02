package output

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/willie-yao/aster/backend/internal/models"
)

func TestIsPublicDataPath(t *testing.T) {
	data, err := os.ReadFile("testdata/public_paths.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name   string
		Path   string
		Public bool
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			if got := IsPublicDataPath(tc.Path); got != tc.Public {
				t.Fatalf("IsPublicDataPath(%q) = %t, want %t", tc.Path, got, tc.Public)
			}
		})
	}
	if IsPublicDataPath(string([]byte{0xff})) {
		t.Fatal("invalid UTF-8 path is public")
	}
}

func TestIsPublicDataPathProducerFilenames(t *testing.T) {
	for _, id := range []string{"a", "ab", "abc", "periodic-x", "example/project/pull-e2e", "unicode-☃"} {
		t.Run(id, func(t *testing.T) {
			path := "jobs/" + models.JobDataFilename(id)
			if !IsPublicDataPath(path) {
				t.Fatalf("producer path %q is not public", path)
			}
		})
	}
	for _, number := range []int{1, 123, 1000000} {
		path := "pull-requests/" + models.PullRequestDataFilename(number)
		if !IsPublicDataPath(path) {
			t.Fatalf("producer path %q is not public", path)
		}
	}
}
