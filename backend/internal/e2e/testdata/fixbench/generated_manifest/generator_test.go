package manifest

import (
	"os"
	"testing"
)

func TestRenderCronJobMatchesGolden(t *testing.T) {
	want, err := os.ReadFile("testdata/golden.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if got := RenderCronJob("fixture"); got != string(want) {
		t.Fatalf("generated manifest does not match golden:\n%s", got)
	}
}
