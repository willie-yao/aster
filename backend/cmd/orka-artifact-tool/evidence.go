package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
)

const (
	evidenceTokenVersion = "v2"
	maxEvidencePathBytes = 4096
)

type evidenceAttestor struct {
	key []byte
}

func newEvidenceAttestor(secret string) *evidenceAttestor {
	sum := sha256.Sum256([]byte("prow-ai-artifact-evidence\x00" + secret))
	return &evidenceAttestor{key: append([]byte(nil), sum[:]...)}
}

func (a *evidenceAttestor) issue(scope, path string) string {
	return a.issueBytes(scope, path, 1)
}

func (a *evidenceAttestor) issueBytes(scope, path string, bytesFetched int) string {
	path = normalizeEvidencePath(path)
	if a == nil || len(a.key) == 0 || path == "" || len(path) > maxEvidencePathBytes || bytesFetched < 0 {
		return ""
	}
	encodedPath := base64.RawURLEncoding.EncodeToString([]byte(path))
	mac := a.mac(scope, path, bytesFetched)
	return strings.Join([]string{evidenceTokenVersion, encodedPath, strconv.Itoa(bytesFetched), base64.RawURLEncoding.EncodeToString(mac)}, ".")
}

func (a *evidenceAttestor) verify(scope, token string) (string, bool) {
	path, _, ok := a.verifyBytes(scope, token)
	return path, ok
}

func (a *evidenceAttestor) verifyBytes(scope, token string) (string, int, bool) {
	if a == nil || len(a.key) == 0 {
		return "", 0, false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 4 || parts[0] != evidenceTokenVersion {
		return "", 0, false
	}
	pathBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(pathBytes) == 0 || len(pathBytes) > maxEvidencePathBytes {
		return "", 0, false
	}
	path := string(pathBytes)
	if normalizeEvidencePath(path) != path {
		return "", 0, false
	}
	bytesFetched, err := strconv.Atoi(parts[2])
	if err != nil || bytesFetched < 0 {
		return "", 0, false
	}
	provided, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil || !hmac.Equal(provided, a.mac(scope, path, bytesFetched)) {
		return "", 0, false
	}
	return path, bytesFetched, true
}

func (a *evidenceAttestor) mac(scope, path string, bytesFetched int) []byte {
	mac := hmac.New(sha256.New, a.key)
	_, _ = mac.Write([]byte(evidenceTokenVersion))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(scope))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(path))
	_, _ = fmt.Fprintf(mac, "\x00%d", bytesFetched)
	return mac.Sum(nil)
}

func attachEvidenceToken(attestor *evidenceAttestor, scope, toolName string, bytesFetched int, payload map[string]interface{}) {
	if payload == nil || !isEvidenceTool(toolName) {
		return
	}
	if _, failed := payload["error"]; failed {
		return
	}
	path, _ := payload["path"].(string)
	if token := attestor.issueBytes(scope, path, bytesFetched); token != "" {
		payload["evidence_token"] = token
	}
}

func isEvidenceTool(name string) bool {
	switch strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), "-", "_") {
	case "read_artifact", "tail_artifact", "grep_artifact":
		return true
	}
	return false
}
