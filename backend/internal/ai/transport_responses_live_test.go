package ai

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestLiveResponsesTransport(t *testing.T) {
	if os.Getenv("RUN_AI_TRANSPORT_LIVE") != "1" {
		t.Skip("set RUN_AI_TRANSPORT_LIVE=1")
	}
	c := NewClientWithOptions(Options{API: APIResponses, Endpoint: os.Getenv("AI_ENDPOINT"), Model: os.Getenv("AI_MODEL"), Token: os.Getenv("AI_TOKEN")})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	got, err := c.Complete(ctx, "Reply concisely.", "Reply with exactly RESPONSES_TRANSPORT_OK")
	if err != nil {
		t.Fatal(err)
	}
	if got != "RESPONSES_TRANSPORT_OK" {
		t.Fatalf("response=%q", got)
	}
}
