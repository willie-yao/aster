package runtime

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
		t.Fatalf("unavailable critique = %v, %d, %v", got, n, err)
	}
}

func TestCritiqueKeepsAvailableReviewer(t *testing.T) {
	zero, two := 0, 2
	for _, tc := range []struct {
		name       string
		configured *int
		retries    int
	}{
		{name: "default"},
		{name: "zero regenerations", configured: &zero},
		{name: "two regenerations", configured: &two, retries: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := critiqueClient{}
			got, n, err := Critique(client, tc.configured)
			if err != nil || got != client || n != tc.retries {
				t.Fatalf("critique = %v, %d, %v, want available reviewer with %d retries", got, n, err, tc.retries)
			}
		})
	}
}
