package main

import "testing"

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
