package project

import "testing"

func TestValidateAICacheGeneration(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		wantErr     bool
	}{
		{name: "empty"},
		{name: "valid", value: "1"},
		{name: "invalid characters", value: "secret value", wantErr: true},
		{name: "whitespace", value: " 2 ", wantErr: true},
		{name: "too long", value: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789abc", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateAICacheGeneration(tc.value)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error=%v, want error=%t", err, tc.wantErr)
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
