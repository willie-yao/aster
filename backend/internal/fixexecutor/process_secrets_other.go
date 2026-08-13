//go:build !linux

package fixexecutor

func lockProcessSecrets() error { return nil }
