package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/willie-yao/aster/backend/internal/analysischat"
	"github.com/willie-yao/aster/backend/internal/project"
)

func exactJUnitChatFixEnabled(fixConfig project.FixPRs) bool {
	return fixConfig.Enabled && fixConfig.AgentRuntime != nil && fixConfig.AgentRuntime.Type == "agent-sandbox"
}

func fixActionsEnabled(fixConfig project.FixPRs) bool {
	if !fixConfig.Enabled || fixConfig.AgentRuntime == nil {
		return false
	}
	return fixConfig.AgentRuntime.Type == "" || fixConfig.AgentRuntime.Type == "agent-sandbox"
}

const fixActionTimeoutHeadroom = 5 * time.Minute

func actionTimeoutsFromEnv(cfg *project.Config) (actionTimeout, generationTimeout time.Duration, err error) {
	const defaultGenerationTimeout = 10 * time.Minute
	generationTimeout = defaultGenerationTimeout
	fix := cfg.EffectiveFixPRs()
	minimum := time.Duration(0)
	if fixActionsEnabled(fix) {
		minimum = fix.AgentRuntime.ParsedTimeout() + fixActionTimeoutHeadroom
		if generationTimeout < minimum {
			generationTimeout = minimum
		}
	}
	if value, present := os.LookupEnv("ACTION_TIMEOUT"); present {
		actionTimeout, err = time.ParseDuration(value)
		if err != nil {
			return 0, 0, fmt.Errorf("invalid ACTION_TIMEOUT %q: %w", value, err)
		}
		if actionTimeout <= 0 {
			return 0, 0, fmt.Errorf("ACTION_TIMEOUT must be a positive duration")
		}
		if minimum > 0 && actionTimeout < minimum {
			return 0, 0, fmt.Errorf("ACTION_TIMEOUT must be at least %s when Fix is enabled (agent timeout plus %s)", minimum, fixActionTimeoutHeadroom)
		}
		generationTimeout = actionTimeout
	}
	return actionTimeout, generationTimeout, nil
}

type interactiveFeatures struct {
	Actions               bool
	AnalysisChat          bool
	PullRequestEscalation bool
}

func interactiveFeaturesFromEnv() (interactiveFeatures, error) {
	chat, err := optionalBoolEnv("ANALYSIS_CHAT_ENABLED", false)
	if err != nil {
		return interactiveFeatures{}, err
	}
	pullRequestEscalation, err := optionalBoolEnv("PULL_REQUEST_ESCALATION_ENABLED", false)
	if err != nil {
		return interactiveFeatures{}, err
	}
	actions, err := optionalBoolEnv("ACTIONS_ENABLED", !chat && !pullRequestEscalation)
	if err != nil {
		return interactiveFeatures{}, err
	}
	return interactiveFeatures{
		Actions: actions, AnalysisChat: chat,
		PullRequestEscalation: pullRequestEscalation,
	}, nil
}

func optionalBoolEnv(name string, fallback bool) (bool, error) {
	value, ok := os.LookupEnv(name)
	if !ok || strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(strings.TrimSpace(value))
	if err != nil {
		return false, fmt.Errorf("invalid %s %q: %w", name, value, err)
	}
	return parsed, nil
}

func analysisChatTimeoutFromEnv() (time.Duration, error) {
	const maxTimeout = 30 * time.Minute
	timeout := analysischat.DefaultTurnTimeout
	if value := os.Getenv("ANALYSIS_CHAT_TIMEOUT"); value != "" {
		parsed, err := time.ParseDuration(value)
		if err != nil {
			return 0, fmt.Errorf("invalid ANALYSIS_CHAT_TIMEOUT %q: %w", value, err)
		}
		timeout = parsed
	}
	if timeout <= 0 || timeout > maxTimeout {
		return 0, fmt.Errorf("ANALYSIS_CHAT_TIMEOUT must be greater than zero and at most %s", maxTimeout)
	}
	return timeout, nil
}

func analysisChatServiceOptionsFromEnv(dataDir string, timeout time.Duration) (analysischat.Options, error) {
	opts := analysischat.Options{
		StateDir:                     strings.TrimSpace(os.Getenv("ANALYSIS_CHAT_STATE_DIR")),
		SessionTTL:                   2 * time.Hour,
		HistoryRetention:             analysischat.DefaultHistoryRetention,
		MaxSessions:                  128,
		MaxSessionsPerOwner:          8,
		TurnLeaseTTL:                 timeout + 30*time.Second,
		TurnTimeout:                  timeout,
		MaxActiveTurnsPerOwner:       2,
		MaxRequestsPerOwnerPerMinute: 10,
	}
	if opts.StateDir == "" {
		opts.StateDir = filepath.Join(dataDir, ".analysis-chat")
	}
	if value := os.Getenv("ANALYSIS_CHAT_SESSION_TTL"); value != "" {
		ttl, err := time.ParseDuration(value)
		if err != nil {
			return analysischat.Options{}, fmt.Errorf("invalid ANALYSIS_CHAT_SESSION_TTL %q: %w", value, err)
		}
		if ttl <= 0 {
			return analysischat.Options{}, fmt.Errorf("ANALYSIS_CHAT_SESSION_TTL must be greater than zero")
		}
		opts.SessionTTL = ttl
	}
	if value := os.Getenv("ANALYSIS_CHAT_HISTORY_RETENTION"); value != "" {
		retention, err := time.ParseDuration(value)
		if err != nil || retention <= 0 {
			return analysischat.Options{}, fmt.Errorf("ANALYSIS_CHAT_HISTORY_RETENTION must be a positive duration")
		}
		opts.HistoryRetention = retention
	}
	var err error
	opts.MaxSessions, err = positiveIntEnv("ANALYSIS_CHAT_MAX_SESSIONS", opts.MaxSessions)
	if err != nil {
		return analysischat.Options{}, err
	}
	opts.MaxSessionsPerOwner, err = positiveIntEnv("ANALYSIS_CHAT_MAX_SESSIONS_PER_OWNER", opts.MaxSessionsPerOwner)
	if err != nil {
		return analysischat.Options{}, err
	}
	opts.MaxActiveTurnsPerOwner, err = positiveIntEnv("ANALYSIS_CHAT_MAX_ACTIVE_TURNS_PER_OWNER", opts.MaxActiveTurnsPerOwner)
	if err != nil {
		return analysischat.Options{}, err
	}
	opts.MaxRequestsPerOwnerPerMinute, err = positiveIntEnv("ANALYSIS_CHAT_REQUESTS_PER_MINUTE", opts.MaxRequestsPerOwnerPerMinute)
	if err != nil {
		return analysischat.Options{}, err
	}
	if opts.MaxSessionsPerOwner > opts.MaxSessions {
		return analysischat.Options{}, fmt.Errorf("ANALYSIS_CHAT_MAX_SESSIONS_PER_OWNER cannot exceed ANALYSIS_CHAT_MAX_SESSIONS")
	}
	return opts, nil
}

func positiveIntEnv(name string, fallback int) (int, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return parsed, nil
}

// githubReadTokenFromEnv mirrors the fetcher's token preference order.
func githubReadTokenFromEnv() string {
	for _, name := range []string{"GITHUB_READ_TOKEN", "BOT_TOKEN", "GITHUB_TOKEN"} {
		if token := os.Getenv(name); token != "" {
			return token
		}
	}
	return ""
}
