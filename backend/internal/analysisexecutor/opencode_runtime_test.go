package analysisexecutor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyOpenCodeRuntime(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "opencode")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '1.18.2\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(t.TempDir(), "opencode-runtime.json")
	writeOpenCodeRuntimeManifestForTest(t, manifestPath, bin, func(*openCodeRuntimeManifest) {})
	if err := verifyOpenCodeRuntime(context.Background(), bin, manifestPath); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyOpenCodeRuntimeFailsClosed(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "opencode")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '1.18.2\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*openCodeRuntimeManifest)
	}{
		{name: "upstream version", mutate: func(value *openCodeRuntimeManifest) { value.UpstreamVersion = "1.18.1" }},
		{name: "upstream revision", mutate: func(value *openCodeRuntimeManifest) { value.UpstreamRevision = strings.Repeat("a", 40) }},
		{name: "source archive", mutate: func(value *openCodeRuntimeManifest) { value.SourceArchiveSHA256 = strings.Repeat("a", 64) }},
		{name: "models snapshot", mutate: func(value *openCodeRuntimeManifest) { value.ModelsDevSHA256 = strings.Repeat("a", 64) }},
		{name: "web builder", mutate: func(value *openCodeRuntimeManifest) { value.WebBuilderImage = "unexpected" }},
		{name: "web node", mutate: func(value *openCodeRuntimeManifest) { value.WebBuilderNode = "unexpected" }},
		{name: "web bun", mutate: func(value *openCodeRuntimeManifest) { value.WebBuilderBunImage = "unexpected" }},
		{name: "web bun digest", mutate: func(value *openCodeRuntimeManifest) { value.WebBuilderBunSHA256 = strings.Repeat("a", 64) }},
		{name: "web UI digest", mutate: func(value *openCodeRuntimeManifest) { value.WebUISHA256 = "unexpected" }},
		{name: "builder", mutate: func(value *openCodeRuntimeManifest) { value.BuilderImage = "unexpected" }},
		{name: "builder bun digest", mutate: func(value *openCodeRuntimeManifest) { value.BuilderBunSHA256 = strings.Repeat("a", 64) }},
		{name: "bun version", mutate: func(value *openCodeRuntimeManifest) { value.BunVersion = "unexpected" }},
		{name: "embedded web UI", mutate: func(value *openCodeRuntimeManifest) { value.EmbeddedWebUI = false }},
		{name: "patch version", mutate: func(value *openCodeRuntimeManifest) { value.PatchVersion = "unexpected" }},
		{name: "patch digest", mutate: func(value *openCodeRuntimeManifest) { value.PatchSHA256 = strings.Repeat("a", 64) }},
		{name: "build patch version", mutate: func(value *openCodeRuntimeManifest) { value.BuildPatchVersion = "unexpected" }},
		{name: "build patch digest", mutate: func(value *openCodeRuntimeManifest) { value.BuildPatchSHA256 = strings.Repeat("a", 64) }},
		{name: "binary digest", mutate: func(value *openCodeRuntimeManifest) { value.BinarySHA256 = strings.Repeat("a", 64) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manifestPath := filepath.Join(t.TempDir(), "opencode-runtime.json")
			writeOpenCodeRuntimeManifestForTest(t, manifestPath, bin, tc.mutate)
			if err := verifyOpenCodeRuntime(context.Background(), bin, manifestPath); err == nil {
				t.Fatal("mismatched OpenCode runtime was accepted")
			}
		})
	}
}

func TestVerifyOpenCodeRuntimeRejectsWrongVersionOutput(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "opencode")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '1.18.1\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(t.TempDir(), "opencode-runtime.json")
	writeOpenCodeRuntimeManifestForTest(t, manifestPath, bin, func(*openCodeRuntimeManifest) {})
	if err := verifyOpenCodeRuntime(context.Background(), bin, manifestPath); err == nil {
		t.Fatal("wrong OpenCode version output was accepted")
	}
}

func writeOpenCodeRuntimeManifestForTest(t *testing.T, path, bin string, mutate func(*openCodeRuntimeManifest)) {
	t.Helper()
	binary, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(binary)
	manifest := openCodeRuntimeManifest{
		Version: openCodeRuntimeManifestVersion, UpstreamVersion: openCodeUpstreamVersion,
		UpstreamRevision: openCodeUpstreamRevision, SourceArchiveSHA256: openCodeSourceArchiveSHA256,
		ModelsDevSHA256: openCodeModelsDevSHA256, WebBuilderImage: openCodeWebBuilderImage, WebBuilderNode: openCodeWebBuilderNodeVersion,
		WebBuilderBunImage: openCodeWebBuilderBunImage, WebBuilderBunSHA256: openCodeWebBuilderBunSHA256, WebUISHA256: strings.Repeat("e", 64),
		BuilderImage: openCodeBuilderImage, BuilderBunSHA256: openCodeBuilderBunSHA256,
		BunVersion: openCodeBunVersion, EmbeddedWebUI: true,
		PatchVersion: openCodePatchVersion, PatchSHA256: openCodePatchSHA256, BuildPatchVersion: openCodeBuildPatchVersion,
		BuildPatchSHA256: openCodeBuildPatchSHA256, BinarySHA256: hex.EncodeToString(digest[:]),
	}
	mutate(&manifest)
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
