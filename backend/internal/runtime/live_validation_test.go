package runtime

import (
	"context"
	"os"
	"sort"
	"testing"
	"time"
)

// TestLive_OpencodeFullPath exercises the real NewLocalAgent() path end to end
// against a live OpenAI-compatible endpoint. It is gated on LIVE_OPENCODE plus
// the endpoint/model/token so it never runs in CI. Point it at any endpoint:
//
//	LIVE_OPENCODE=1 \
//	LIVE_ENDPOINT=https://models.github.ai/inference/chat/completions \
//	LIVE_MODEL=openai/gpt-4o-mini LIVE_TOKEN=$GITHUB_TOKEN \
//	go test ./internal/runtime -run TestLive_OpencodeFullPath -v
//
// Validated 2026-07-15 on opencode 1.15.5: the agent created the file in the
// materialized clone and gitChanges captured it. This surfaced the two fixes in
// opencodeCmd/writeOpencodeConfig (the --dir pin and the model output limit).
func TestLive_OpencodeFullPath(t *testing.T) {
	endpoint, model, token := os.Getenv("LIVE_ENDPOINT"), os.Getenv("LIVE_MODEL"), os.Getenv("LIVE_TOKEN")
	if os.Getenv("LIVE_OPENCODE") == "" || endpoint == "" || model == "" || token == "" {
		t.Skip("set LIVE_OPENCODE=1 + LIVE_ENDPOINT + LIVE_MODEL + LIVE_TOKEN to run the live opencode check")
	}
	repo := initRepo(t)
	res, err := NewLocalAgent().Generate(context.Background(), GenerateSpec{
		Repo:        RepoRef{Owner: "o", Name: "n", Ref: "main", CloneURL: repo},
		Instruction: "Create a new file named greeting.txt whose entire contents are exactly the word HELLO followed by a newline. Do not change any other file.",
		Model:       model,
		Endpoint:    endpoint,
		Token:       token,
		Timeout:     4 * time.Minute,
	})
	if err != nil {
		t.Fatalf("Generate: %v\noutput:\n%s", err, res.Output)
	}
	var changed []string
	for k := range res.Files {
		changed = append(changed, k)
	}
	sort.Strings(changed)
	t.Logf("changed files: %v; greeting.txt=%q", changed, res.Files["greeting.txt"])
	if _, ok := res.Files["greeting.txt"]; !ok {
		t.Errorf("expected greeting.txt via the real NewLocalAgent path; got %v\noutput:\n%s", changed, res.Output)
	}
}
