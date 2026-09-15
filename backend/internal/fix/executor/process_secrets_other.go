//go:build !linux

package executor

func lockProcessSecrets() error { return nil }
