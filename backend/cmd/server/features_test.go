package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/project"
	"github.com/willie-yao/aster/backend/internal/server"
)

func TestEnableActionsConfiguresGenerationTimeouts(t *testing.T) {
	for _, testCase := range []struct {
		name, agentTimeout string
		fixEnabled         bool
		explicit           string
		wantHandler        time.Duration
		wantFixPreview     time.Duration
		wantError          bool
	}{
		{name: "default agent", fixEnabled: true, wantFixPreview: 15 * time.Minute},
		{name: "long agent", fixEnabled: true, agentTimeout: "30m", wantFixPreview: 35 * time.Minute},
		{name: "Fix disabled", wantFixPreview: 0},
		{name: "explicit override", fixEnabled: true, explicit: "40m", agentTimeout: "30m", wantHandler: 40 * time.Minute, wantFixPreview: 40 * time.Minute},
		{name: "invalid override", fixEnabled: true, explicit: "14m", wantError: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("ACTION_TIMEOUT", "")
			if testCase.explicit == "" {
				if err := os.Unsetenv("ACTION_TIMEOUT"); err != nil {
					t.Fatal(err)
				}
			} else {
				t.Setenv("ACTION_TIMEOUT", testCase.explicit)
			}
			cfg := &project.Config{AI: &project.AI{FixPRs: &project.FixPRs{
				Enabled: testCase.fixEnabled, AgentRuntime: &project.FixAgentRuntime{
					Type: "agent-sandbox", Timeout: testCase.agentTimeout,
				},
			}}}
			opts := &server.Options{}
			ctx, cancel := context.WithCancel(context.Background())
			service, err := enableActions(ctx, opts, cfg, t.TempDir(), nil)
			defer func() {
				cancel()
				if service != nil {
					if err := service.Wait(context.Background()); err != nil {
						t.Error(err)
					}
				}
			}()
			if testCase.wantError {
				if err == nil || service != nil || opts.Actions != nil {
					t.Fatalf("invalid startup: service=%v options=%+v error=%v", service, opts, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if opts.ActionTimeout != testCase.wantHandler || opts.FixPreviewTimeout != testCase.wantFixPreview ||
				opts.DisableFixActions == testCase.fixEnabled || opts.Actions != service {
				t.Fatalf("options=%+v, want handler=%s Fix preview=%s enabled=%t", opts, testCase.wantHandler, testCase.wantFixPreview, testCase.fixEnabled)
			}
		})
	}
}

func TestFormatPricingRate(t *testing.T) {
	for _, test := range []struct {
		value string
		want  string
	}{
		{value: "3.000000", want: "3.00"},
		{value: "0.305", want: "0.31"},
		{value: "0.304", want: "0.30"},
	} {
		if got := formatPricingRate(test.value); got != test.want {
			t.Errorf("formatPricingRate(%q) = %q, want %q", test.value, got, test.want)
		}
	}
}
