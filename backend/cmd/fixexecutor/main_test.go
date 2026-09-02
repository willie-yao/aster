package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/willie-yao/aster/backend/internal/modelprovider"
	engineruntime "github.com/willie-yao/aster/backend/internal/runtime"
)

func validExecutionRequest() engineruntime.ExecutionRequest {
	sha := strings.Repeat("a", 40)
	return engineruntime.ExecutionRequest{
		Version: engineruntime.ExecutionContractVersion, RepositoryURL: "https://github.com/octocat/Hello-World.git",
		CommitSHA: sha, ExpectedBaseSHA: sha, Prompt: "Update README.", TimeoutSeconds: 60,
		MaxSteps: 2, MaxFiles: 1, OutputLimitBytes: 64 << 10,
		ModelProvider: modelprovider.Normalize(modelprovider.Config{
			CredentialMode: modelprovider.CredentialModeGateway,
			API:            modelprovider.APIChatCompletions, Endpoint: "https://gateway.example.internal/v1/chat/completions", Model: "fixture-model",
			Auth: modelprovider.Auth{Type: modelprovider.AuthTypeNone},
		}),
		CommandPolicy: engineruntime.CommandPolicy{Commands: []engineruntime.ExecutionCommand{{
			Argv: []string{"git", "diff", "--cached", "--check"}, TimeoutSeconds: 30,
		}}},
	}
}

func encodeRequestData(t *testing.T, data []byte) string {
	t.Helper()
	return base64.StdEncoding.EncodeToString(data)
}

func marshalRequest(t *testing.T, request engineruntime.ExecutionRequest) []byte {
	t.Helper()
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestVersionString(t *testing.T) {
	previousVersion, previousCommit, previousImageTag := version, commit, imageTag
	t.Cleanup(func() {
		version, commit, imageTag = previousVersion, previousCommit, previousImageTag
	})
	version = "v0.9.0"
	commit = "0123456789abcdef0123456789abcdef01234567"
	imageTag = "sha-0123456"
	want := "fixexecutor version=v0.9.0 commit=0123456789abcdef0123456789abcdef01234567 image_tag=sha-0123456"
	if got := versionString(); got != want {
		t.Fatalf("versionString() = %q, want %q", got, want)
	}
}

func TestReadRequestRequiresOneStrictJSONDocument(t *testing.T) {
	valid := validExecutionRequest()
	validData := marshalRequest(t, valid)
	unknown := append(append([]byte(nil), validData[:len(validData)-1]...), []byte(`,"unknown":true}`)...)
	invalidContract := valid
	invalidContract.Version = 0

	for _, testCase := range []struct {
		name    string
		encoded string
		wantErr string
	}{
		{name: "missing", wantErr: requestEnv + " is required"},
		{name: "invalid base64", encoded: "not-base64", wantErr: "decode execution request"},
		{name: "unknown field", encoded: encodeRequestData(t, unknown), wantErr: "unknown field"},
		{name: "trailing document", encoded: encodeRequestData(t, append(append([]byte(nil), validData...), []byte(`{}`)...)), wantErr: "trailing data"},
		{name: "trailing garbage", encoded: encodeRequestData(t, append(append([]byte(nil), validData...), []byte(`garbage`)...)), wantErr: "trailing data"},
		{name: "contract failure", encoded: encodeRequestData(t, marshalRequest(t, invalidContract)), wantErr: "version 0"},
		{name: "valid with whitespace", encoded: " \n" + encodeRequestData(t, append(append([]byte(" \n"), validData...), []byte("\n\t")...)) + "\t "},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv(requestEnv, testCase.encoded)
			got, err := readRequest()
			if testCase.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
					t.Fatalf("readRequest error = %v, want %q", err, testCase.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Version != valid.Version || got.CommitSHA != valid.CommitSHA || got.Prompt != valid.Prompt {
				t.Fatalf("request = %+v", got)
			}
		})
	}
}
