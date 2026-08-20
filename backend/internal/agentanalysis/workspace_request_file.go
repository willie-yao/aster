package agentanalysis

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const (
	WorkspaceExecutionRequestRoot           = "/analysis-request"
	WorkspaceExecutionRequestFile           = "request.json"
	WorkspaceExecutionRequestChunkEnvPrefix = "PROW_AI_ANALYSIS_EXECUTION_REQUEST_B64_CHUNK_"
	WorkspaceExecutionRequestChunkCount     = 16
	workspaceExecutionRequestChunkBytes     = 64 << 10
)

// WorkspaceExecutionRequestPath returns the fixed request file path.
func WorkspaceExecutionRequestPath(root string) string {
	return filepath.Join(root, WorkspaceExecutionRequestFile)
}

// WorkspaceExecutionRequestChunkEnv returns one fixed request chunk name.
func WorkspaceExecutionRequestChunkEnv(index int) string {
	return fmt.Sprintf("%s%02d", WorkspaceExecutionRequestChunkEnvPrefix, index)
}

// EncodeWorkspaceExecutionRequestChunks splits one bounded request for the credential-free stager.
func EncodeWorkspaceExecutionRequestChunks(data []byte) ([]string, error) {
	if len(data) < 1 || len(data) > maxWorkspaceRequestBytes || !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return nil, fmt.Errorf("workspace execution request is empty, oversized, or invalid")
	}
	encoded := base64.StdEncoding.EncodeToString(data)
	if len(encoded) > WorkspaceExecutionRequestChunkCount*workspaceExecutionRequestChunkBytes {
		return nil, fmt.Errorf("workspace execution request exceeds chunk capacity")
	}
	chunks := make([]string, WorkspaceExecutionRequestChunkCount)
	for index := 0; len(encoded) > 0; index++ {
		limit := min(len(encoded), workspaceExecutionRequestChunkBytes)
		chunks[index] = encoded[:limit]
		encoded = encoded[limit:]
	}
	return chunks, nil
}

// DecodeWorkspaceExecutionRequestChunks reconstructs one exact bounded request.
func DecodeWorkspaceExecutionRequestChunks(lookup func(string) (string, bool)) ([]byte, error) {
	if lookup == nil {
		return nil, fmt.Errorf("workspace execution request chunk lookup is required")
	}
	var encoded strings.Builder
	empty := false
	for index := 0; index < WorkspaceExecutionRequestChunkCount; index++ {
		name := WorkspaceExecutionRequestChunkEnv(index)
		value, ok := lookup(name)
		if !ok {
			return nil, fmt.Errorf("%s is required", name)
		}
		if value == "" {
			empty = true
			continue
		}
		if empty || len(value) > workspaceExecutionRequestChunkBytes {
			return nil, fmt.Errorf("workspace execution request chunks are sparse or oversized")
		}
		encoded.WriteString(value)
	}
	if encoded.Len() == 0 || encoded.Len() > WorkspaceExecutionRequestChunkCount*workspaceExecutionRequestChunkBytes {
		return nil, fmt.Errorf("workspace execution request chunks are empty or oversized")
	}
	data, err := base64.StdEncoding.Strict().DecodeString(encoded.String())
	if err != nil {
		return nil, fmt.Errorf("decode workspace execution request chunks: %w", err)
	}
	if len(data) < 1 || len(data) > maxWorkspaceRequestBytes || !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return nil, fmt.Errorf("workspace execution request is empty, oversized, or invalid")
	}
	return data, nil
}

// WriteWorkspaceExecutionRequestFile writes one validated request to a fresh bounded volume.
func WriteWorkspaceExecutionRequestFile(root string, request WorkspaceExecutionRequest) error {
	if err := ValidateWorkspaceExecutionRequest(request); err != nil {
		return err
	}
	data, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("encode workspace execution request: %w", err)
	}
	if len(data) > maxWorkspaceRequestBytes {
		return fmt.Errorf("workspace execution request is oversized")
	}
	root = filepath.Clean(root)
	if !filepath.IsAbs(root) {
		return fmt.Errorf("workspace execution request root must be absolute")
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("workspace execution request root is not a safe directory")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("workspace execution request root must be empty")
	}
	path := WorkspaceExecutionRequestPath(root)
	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o400)
	if err != nil {
		return fmt.Errorf("create workspace execution request file: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	written := 0
	for written < len(data) {
		count, writeErr := file.Write(data[written:])
		if writeErr != nil {
			_ = file.Close()
			return fmt.Errorf("write workspace execution request file: %w", writeErr)
		}
		if count == 0 {
			_ = file.Close()
			return fmt.Errorf("write workspace execution request file: short write")
		}
		written += count
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close workspace execution request file: %w", err)
	}
	return nil
}

// ReadWorkspaceExecutionRequestFile reads and validates one fixed request file.
func ReadWorkspaceExecutionRequestFile(root string) (WorkspaceExecutionRequest, error) {
	root = filepath.Clean(root)
	if !filepath.IsAbs(root) {
		return WorkspaceExecutionRequest{}, fmt.Errorf("workspace execution request root must be absolute")
	}
	path := WorkspaceExecutionRequestPath(root)
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return WorkspaceExecutionRequest{}, fmt.Errorf("workspace execution request file is missing or unsafe")
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > maxWorkspaceRequestBytes {
		return WorkspaceExecutionRequest{}, fmt.Errorf("workspace execution request file is unsafe or oversized")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxWorkspaceRequestBytes+1))
	if err != nil {
		return WorkspaceExecutionRequest{}, fmt.Errorf("read workspace execution request file: %w", err)
	}
	if len(data) < 1 || len(data) > maxWorkspaceRequestBytes {
		return WorkspaceExecutionRequest{}, fmt.Errorf("workspace execution request file is empty or oversized")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var request WorkspaceExecutionRequest
	if err := decoder.Decode(&request); err != nil {
		return request, fmt.Errorf("parse workspace execution request: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return request, fmt.Errorf("workspace execution request contains trailing data")
	}
	if err := ValidateWorkspaceExecutionRequest(request); err != nil {
		return request, err
	}
	return request, nil
}
