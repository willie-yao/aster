package project

import (
	"encoding/json"
	"testing"
)

func TestResolveAICacheGeneration(t *testing.T) {
	for _, tc := range []struct {
		name, config, env, want string
		wantErr                 bool
	}{
		{name: "empty"},
		{name: "config", config: "1", want: "1"},
		{name: "env override", config: "1", env: "2", want: "2"},
		{name: "blank env uses config", config: "1", env: "  ", want: "1"},
		{name: "invalid characters", config: "secret value", wantErr: true},
		{name: "config whitespace", config: " 2 ", wantErr: true},
		{name: "env whitespace", config: "1", env: " 2 ", wantErr: true},
		{name: "too long", config: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789abc", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveAICacheGeneration(tc.config, tc.env)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("generation=%q error=%v, want %q error=%t", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestAICacheGenerationFingerprint(t *testing.T) {
	if got := AICacheGenerationFingerprint(""); got != "" {
		t.Fatalf("empty fingerprint = %q", got)
	}
	one := AICacheGenerationFingerprint("1")
	if one == "" || one != AICacheGenerationFingerprint("1") || one == AICacheGenerationFingerprint("2") {
		t.Fatalf("fingerprints: one=%q two=%q", one, AICacheGenerationFingerprint("2"))
	}
}

func TestAICacheGenerationIsNotPublicJSON(t *testing.T) {
	data, err := json.Marshal(AI{CacheGeneration: "private-generation"})
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"agentic":{"critique":{}}}` {
		t.Fatalf("public AI JSON = %s", data)
	}
}
