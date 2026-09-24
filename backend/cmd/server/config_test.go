package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/project"
)

func TestInteractiveFeaturesFromEnv(t *testing.T) {
	setDefaults := func(t *testing.T) {
		t.Helper()
		t.Setenv("ANALYSIS_CHAT_ENABLED", "")
		t.Setenv("ANALYSIS_SOURCE_INVESTIGATION_ENABLED", "")
		t.Setenv("PULL_REQUEST_ESCALATION_ENABLED", "")
		t.Setenv("ACTIONS_ENABLED", "")
	}
	t.Run("legacy actions default", func(t *testing.T) {
		setDefaults(t)
		features, err := interactiveFeaturesFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if !features.Actions || features.AnalysisChat {
			t.Fatalf("features = %+v", features)
		}
	})
	t.Run("chat defaults writes off", func(t *testing.T) {
		setDefaults(t)
		t.Setenv("ANALYSIS_CHAT_ENABLED", "true")
		features, err := interactiveFeaturesFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if features.Actions || !features.AnalysisChat {
			t.Fatalf("features = %+v", features)
		}
	})
	t.Run("chat and actions", func(t *testing.T) {
		setDefaults(t)
		t.Setenv("ANALYSIS_CHAT_ENABLED", "1")
		t.Setenv("ACTIONS_ENABLED", "1")
		features, err := interactiveFeaturesFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if !features.Actions || !features.AnalysisChat {
			t.Fatalf("features = %+v", features)
		}
	})
	t.Run("escalation defaults writes off", func(t *testing.T) {
		setDefaults(t)
		t.Setenv("PULL_REQUEST_ESCALATION_ENABLED", "true")
		features, err := interactiveFeaturesFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if features.Actions || !features.PullRequestEscalation {
			t.Fatalf("features = %+v", features)
		}
	})
	t.Run("escalation and actions", func(t *testing.T) {
		setDefaults(t)
		t.Setenv("PULL_REQUEST_ESCALATION_ENABLED", "true")
		t.Setenv("ACTIONS_ENABLED", "true")
		features, err := interactiveFeaturesFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if !features.Actions || !features.PullRequestEscalation {
			t.Fatalf("features = %+v", features)
		}
	})
	t.Run("invalid", func(t *testing.T) {
		setDefaults(t)
		t.Setenv("ANALYSIS_CHAT_ENABLED", "sometimes")
		if _, err := interactiveFeaturesFromEnv(); err == nil {
			t.Fatal("invalid feature flag was accepted")
		}
	})
}

func TestAnalysisChatServiceOptionsFromEnv(t *testing.T) {
	for _, name := range []string{
		"ANALYSIS_CHAT_STATE_DIR",
		"ANALYSIS_CHAT_SESSION_TTL",
		"ANALYSIS_CHAT_HISTORY_RETENTION",
		"ANALYSIS_CHAT_MAX_SESSIONS",
		"ANALYSIS_CHAT_MAX_SESSIONS_PER_OWNER",
		"ANALYSIS_CHAT_MAX_ACTIVE_TURNS_PER_OWNER",
		"ANALYSIS_CHAT_REQUESTS_PER_MINUTE",
	} {
		t.Setenv(name, "")
	}
	opts, err := analysisChatServiceOptionsFromEnv("/data", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if opts.StateDir != filepath.Join("/data", ".analysis-chat") || opts.SessionTTL != 2*time.Hour || opts.HistoryRetention != 180*24*time.Hour ||
		opts.MaxSessions != 128 || opts.MaxSessionsPerOwner != 8 || opts.TurnLeaseTTL != 90*time.Second ||
		opts.TurnTimeout != time.Minute || opts.MaxActiveTurnsPerOwner != 2 || opts.MaxRequestsPerOwnerPerMinute != 10 {
		t.Fatalf("default options = %+v", opts)
	}

	t.Setenv("ANALYSIS_CHAT_STATE_DIR", "/state/chat")
	t.Setenv("ANALYSIS_CHAT_SESSION_TTL", "45m")
	t.Setenv("ANALYSIS_CHAT_HISTORY_RETENTION", "720h")
	t.Setenv("ANALYSIS_CHAT_MAX_SESSIONS", "24")
	t.Setenv("ANALYSIS_CHAT_MAX_SESSIONS_PER_OWNER", "3")
	t.Setenv("ANALYSIS_CHAT_MAX_ACTIVE_TURNS_PER_OWNER", "4")
	t.Setenv("ANALYSIS_CHAT_REQUESTS_PER_MINUTE", "20")
	opts, err = analysisChatServiceOptionsFromEnv("/data", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if opts.StateDir != "/state/chat" || opts.SessionTTL != 45*time.Minute || opts.HistoryRetention != 720*time.Hour ||
		opts.MaxSessions != 24 || opts.MaxSessionsPerOwner != 3 || opts.TurnLeaseTTL != time.Minute ||
		opts.TurnTimeout != 30*time.Second || opts.MaxActiveTurnsPerOwner != 4 || opts.MaxRequestsPerOwnerPerMinute != 20 {
		t.Fatalf("configured options = %+v", opts)
	}
}

func TestAnalysisChatServiceOptionsRejectInvalidEnv(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		value string
	}{
		{name: "ANALYSIS_CHAT_SESSION_TTL", value: "zero"},
		{name: "ANALYSIS_CHAT_MAX_SESSIONS", value: "0"},
		{name: "ANALYSIS_CHAT_MAX_SESSIONS_PER_OWNER", value: "many"},
		{name: "ANALYSIS_CHAT_MAX_ACTIVE_TURNS_PER_OWNER", value: "0"},
		{name: "ANALYSIS_CHAT_REQUESTS_PER_MINUTE", value: "none"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			for _, name := range []string{
				"ANALYSIS_CHAT_SESSION_TTL",
				"ANALYSIS_CHAT_MAX_SESSIONS",
				"ANALYSIS_CHAT_MAX_SESSIONS_PER_OWNER",
				"ANALYSIS_CHAT_MAX_ACTIVE_TURNS_PER_OWNER",
				"ANALYSIS_CHAT_REQUESTS_PER_MINUTE",
			} {
				t.Setenv(name, "")
			}
			t.Setenv(testCase.name, testCase.value)
			if _, err := analysisChatServiceOptionsFromEnv("/data", time.Minute); err == nil {
				t.Fatal("invalid analysis chat setting was accepted")
			}
		})
	}
	t.Run("owner exceeds total", func(t *testing.T) {
		t.Setenv("ANALYSIS_CHAT_SESSION_TTL", "")
		t.Setenv("ANALYSIS_CHAT_MAX_SESSIONS", "2")
		t.Setenv("ANALYSIS_CHAT_MAX_SESSIONS_PER_OWNER", "3")
		t.Setenv("ANALYSIS_CHAT_MAX_ACTIVE_TURNS_PER_OWNER", "4")
		t.Setenv("ANALYSIS_CHAT_REQUESTS_PER_MINUTE", "20")
		if _, err := analysisChatServiceOptionsFromEnv("/data", time.Minute); err == nil {
			t.Fatal("owner limit above total was accepted")
		}
	})
}

func TestAnalysisChatTimeoutFromEnv(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		t.Setenv("ANALYSIS_CHAT_TIMEOUT", "")
		got, err := analysisChatTimeoutFromEnv()
		if err != nil || got != 10*time.Minute {
			t.Fatalf("timeout=%v err=%v", got, err)
		}
	})
	t.Run("slow provider", func(t *testing.T) {
		t.Setenv("ANALYSIS_CHAT_TIMEOUT", "15m")
		got, err := analysisChatTimeoutFromEnv()
		if err != nil || got != 15*time.Minute {
			t.Fatalf("timeout=%v err=%v", got, err)
		}
	})
	for _, value := range []string{"0s", "31m", "not-a-duration"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("ANALYSIS_CHAT_TIMEOUT", value)
			if _, err := analysisChatTimeoutFromEnv(); err == nil {
				t.Fatalf("invalid timeout %q was accepted", value)
			}
		})
	}
}

func TestExactJUnitChatFixEnabledRequiresOptInAgentSandbox(t *testing.T) {
	for _, testCase := range []struct {
		name string
		cfg  project.FixPRs
		want bool
	}{
		{name: "enabled agent sandbox", cfg: project.FixPRs{Enabled: true, AgentRuntime: &project.FixAgentRuntime{Type: "agent-sandbox"}}, want: true},
		{name: "disabled", cfg: project.FixPRs{AgentRuntime: &project.FixAgentRuntime{Type: "agent-sandbox"}}},
		{name: "unsupported runtime", cfg: project.FixPRs{Enabled: true, AgentRuntime: &project.FixAgentRuntime{Type: "opencode"}}},
		{name: "missing runtime", cfg: project.FixPRs{Enabled: true}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := exactJUnitChatFixEnabled(testCase.cfg); got != testCase.want {
				t.Fatalf("enabled = %t, want %t", got, testCase.want)
			}
		})
	}
}

func TestFixActionsEnabledRequiresAgentSandbox(t *testing.T) {
	for _, testCase := range []struct {
		name string
		cfg  project.FixPRs
		want bool
	}{
		{name: "agent sandbox", cfg: project.FixPRs{Enabled: true, AgentRuntime: &project.FixAgentRuntime{Type: "agent-sandbox"}}, want: true},
		{name: "default runtime", cfg: project.FixPRs{Enabled: true, AgentRuntime: &project.FixAgentRuntime{}}, want: true},
		{name: "unsupported runtime", cfg: project.FixPRs{Enabled: true, AgentRuntime: &project.FixAgentRuntime{Type: "opencode"}}},
		{name: "disabled", cfg: project.FixPRs{AgentRuntime: &project.FixAgentRuntime{Type: "agent-sandbox"}}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := fixActionsEnabled(testCase.cfg); got != testCase.want {
				t.Fatalf("enabled=%t want=%t", got, testCase.want)
			}
		})
	}
}

func TestActionTimeoutsFromEnv(t *testing.T) {
	for _, testCase := range []struct {
		name, agentTimeout string
		fixEnabled         bool
		explicit           *string
		wantAction         time.Duration
		wantGeneration     time.Duration
		wantError          string
	}{
		{name: "Fix disabled", wantGeneration: 10 * time.Minute},
		{name: "default agent", fixEnabled: true, wantGeneration: 15 * time.Minute},
		{name: "short agent", fixEnabled: true, agentTimeout: "1m", wantGeneration: 10 * time.Minute},
		{name: "long agent", fixEnabled: true, agentTimeout: "30m", wantGeneration: 35 * time.Minute},
		{name: "equal to minimum", fixEnabled: true, explicit: new("15m"), wantAction: 15 * time.Minute, wantGeneration: 15 * time.Minute},
		{name: "short agent explicit minimum", fixEnabled: true, agentTimeout: "1m", explicit: new("6m"), wantAction: 6 * time.Minute, wantGeneration: 6 * time.Minute},
		{name: "above minimum", fixEnabled: true, agentTimeout: "30m", explicit: new("40m"), wantAction: 40 * time.Minute, wantGeneration: 40 * time.Minute},
		{name: "disabled accepts short override", explicit: new("1m"), wantAction: time.Minute, wantGeneration: time.Minute},
		{name: "below minimum", fixEnabled: true, explicit: new("14m"), wantError: "at least 15m0s"},
		{name: "below long minimum", fixEnabled: true, agentTimeout: "30m", explicit: new("30m"), wantError: "at least 35m0s"},
		{name: "malformed", fixEnabled: true, explicit: new("tomorrow"), wantError: "invalid ACTION_TIMEOUT"},
		{name: "empty", fixEnabled: true, explicit: new(""), wantError: "invalid ACTION_TIMEOUT"},
		{name: "zero", fixEnabled: true, explicit: new("0s"), wantError: "positive duration"},
		{name: "negative", fixEnabled: true, explicit: new("-1m"), wantError: "positive duration"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("ACTION_TIMEOUT", "")
			if testCase.explicit == nil {
				if err := os.Unsetenv("ACTION_TIMEOUT"); err != nil {
					t.Fatal(err)
				}
			} else {
				t.Setenv("ACTION_TIMEOUT", *testCase.explicit)
			}
			cfg := &project.Config{AI: &project.AI{FixPRs: &project.FixPRs{
				Enabled: testCase.fixEnabled,
				AgentRuntime: &project.FixAgentRuntime{
					Type: "agent-sandbox", Timeout: testCase.agentTimeout,
				},
			}}}
			action, generation, err := actionTimeoutsFromEnv(cfg)
			if testCase.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), testCase.wantError) {
					t.Fatalf("action=%s generation=%s error=%v; want %q", action, generation, err, testCase.wantError)
				}
				return
			}
			if err != nil || action != testCase.wantAction || generation != testCase.wantGeneration {
				t.Fatalf("action=%s generation=%s error=%v; want %s, %s", action, generation, err, testCase.wantAction, testCase.wantGeneration)
			}
		})
	}
}

func TestAnalysisChatHistoryRetentionRejectsInvalidDuration(t *testing.T) {
	for _, value := range []string{"0s", "-1h", "180d", "invalid"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("ANALYSIS_CHAT_HISTORY_RETENTION", value)
			if _, err := analysisChatServiceOptionsFromEnv(t.TempDir(), time.Minute); err == nil {
				t.Fatal("invalid history retention accepted")
			}
		})
	}
}
