package fixruntime

import (
	"context"
	"testing"
)

type critiqueClient struct{}

func (critiqueClient) Complete(context.Context, string, string) (string, error) { return "", nil }

func TestCritiqueFailsClosedWithoutReviewer(t *testing.T) {
	retries := 1
	if _, _, err := Critique(nil, &retries); err == nil {
		t.Fatal("configured critique without reviewer was accepted")
	}
	retries = 0
	if got, n, err := Critique(nil, &retries); err != nil || got != nil || n != 0 {
		t.Fatalf("disabled critique = %v, %d, %v", got, n, err)
	}
	client := critiqueClient{}
	if got, n, err := Critique(client, nil); err != nil || got != nil || n != 0 {
		t.Fatalf("default nil config = %v, %d, %v", got, n, err)
	}
	retries = 2
	if got, n, err := Critique(client, &retries); err != nil || got == nil || n != 2 {
		t.Fatalf("configured critique = %v, %d, %v", got, n, err)
	}
}
