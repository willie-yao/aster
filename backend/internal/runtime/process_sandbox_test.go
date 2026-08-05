package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDirectProcessSandboxCancellationKillsChildren(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "child-survived")
	ready := filepath.Join(dir, "child-started")
	ctx, cancel := context.WithCancel(context.Background())
	sandbox := directProcessSandbox{}
	cmd, err := sandbox.Command(ctx, SandboxSpec{
		Command: []string{"/bin/sh", "-c", fmt.Sprintf(
			`(sleep 0.5; printf survived > %q) & printf ready > %q; wait`, marker, ready,
		)},
		Environment: []string{"PATH=/usr/bin:/bin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		} else if !os.IsNotExist(err) {
			t.Fatalf("stat ready marker: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("child process did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	_ = cmd.Wait()
	time.Sleep(600 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("child process survived context cancellation")
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat marker: %v", err)
	}
}

func TestDirectProcessSandboxRejectsEmptyCommand(t *testing.T) {
	if _, err := (directProcessSandbox{}).Command(context.Background(), SandboxSpec{}); err == nil {
		t.Fatal("expected empty command to fail")
	}
}
