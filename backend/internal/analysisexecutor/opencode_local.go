package analysisexecutor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/willie-yao/aster/backend/internal/agentanalysis"
)

type openCodeLocalAPIError struct {
	Class string
	Phase string
	Cause error
}

func (e *openCodeLocalAPIError) Error() string {
	if e == nil {
		return "OpenCode local API request failed"
	}
	return "OpenCode local API request failed: " + e.Class
}

func (e *openCodeLocalAPIError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func newOpenCodeLocalAPIError(err error) error {
	if err == nil {
		return nil
	}
	return &openCodeLocalAPIError{Class: classifyOpenCodeLocalAPIError(err), Cause: err}
}

func annotateOpenCodeLocalPhase(err error, phase string) error {
	var local *openCodeLocalAPIError
	if errors.As(err, &local) && local.Phase == "" {
		local.Phase = phase
	}
	return err
}

func classifyOpenCodeLocalAPIError(err error) string {
	switch {
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "local_connection_closed"
	case errors.Is(err, context.DeadlineExceeded):
		return "local_deadline"
	case errors.Is(err, context.Canceled):
		return "local_cancelled"
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return "local_timeout"
		}
		return "local_network"
	}
	return "local_transport"
}

func recoverableOpenCodeLocalEOF(err error) (*openCodeLocalAPIError, bool) {
	var local *openCodeLocalAPIError
	if !errors.As(err, &local) || local.Class != "local_connection_closed" {
		return nil, false
	}
	return local, true
}

func recordOpenCodeLocalTransport(telemetry *agentanalysis.WorkspaceOpenCodeTelemetry, err error, recovered bool) {
	if telemetry == nil {
		return
	}
	var local *openCodeLocalAPIError
	if !errors.As(err, &local) {
		return
	}
	telemetry.LocalTransportFailure = local.Class
	telemetry.LocalTransportPhase = local.Phase
	telemetry.LocalTransportRecovered = recovered
	if !recovered {
		telemetry.FailureCode = local.Class
	}
}

type openCodeProcessTracker struct {
	done                    chan struct{}
	mu                      sync.RWMutex
	err                     error
	memoryBaseline          cgroupMemoryEvents
	memoryBaselineAvailable bool
}

func trackOpenCodeProcess(cmd *exec.Cmd, baseline cgroupMemoryEvents, baselineAvailable bool) *openCodeProcessTracker {
	tracker := &openCodeProcessTracker{done: make(chan struct{}), memoryBaseline: baseline, memoryBaselineAvailable: baselineAvailable}
	go func() {
		err := cmd.Wait()
		tracker.mu.Lock()
		tracker.err = err
		tracker.mu.Unlock()
		close(tracker.done)
	}()
	return tracker
}

func (p *openCodeProcessTracker) snapshot() (state string, exitCode int, exitKnown bool, signal string) {
	if p == nil {
		return "outcome_unavailable", 0, false, ""
	}
	select {
	case <-p.done:
		p.mu.RLock()
		err := p.err
		p.mu.RUnlock()
		if err == nil {
			return "exited_zero", 0, true, ""
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code := exitErr.ExitCode()
			if code >= 0 {
				return "exited_nonzero", code, true, ""
			}
			return "signaled", 0, false, processSignalName(exitErr.ProcessState)
		}
		return "outcome_unavailable", 0, false, ""
	default:
		return "running", 0, false, ""
	}
}

type openCodeStreamTracker struct {
	reader *os.File
	done   chan struct{}
	mu     sync.RWMutex
	err    error
}

func trackOpenCodeStream(reader *os.File, writer io.Writer) *openCodeStreamTracker {
	tracker := &openCodeStreamTracker{reader: reader, done: make(chan struct{})}
	go func() {
		_, err := io.Copy(writer, reader)
		_ = reader.Close()
		tracker.mu.Lock()
		tracker.err = err
		tracker.mu.Unlock()
		close(tracker.done)
	}()
	return tracker
}

func stopTrackedOpenCodeProcess(terminate func(), tracker *openCodeProcessTracker, streams ...*openCodeStreamTracker) bool {
	if terminate != nil {
		terminate()
	}
	if tracker != nil {
		select {
		case <-tracker.done:
		case <-time.After(time.Second):
			closeOpenCodeStreams(streams)
			waitOpenCodeStreams(streams)
			return false
		}
	}
	deadline := time.Now().Add(time.Second)
	for _, stream := range streams {
		if stream == nil {
			continue
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			closeOpenCodeStreams(streams)
			waitOpenCodeStreams(streams)
			return false
		}
		select {
		case <-stream.done:
			stream.mu.RLock()
			err := stream.err
			stream.mu.RUnlock()
			if err != nil {
				closeOpenCodeStreams(streams)
				waitOpenCodeStreams(streams)
				return false
			}
		case <-time.After(remaining):
			closeOpenCodeStreams(streams)
			waitOpenCodeStreams(streams)
			return false
		}
	}
	return true
}

func closeOpenCodeStreams(streams []*openCodeStreamTracker) {
	for _, stream := range streams {
		if stream != nil && stream.reader != nil {
			_ = stream.reader.Close()
		}
	}
}

func waitOpenCodeStreams(streams []*openCodeStreamTracker) {
	for _, stream := range streams {
		if stream == nil {
			continue
		}
		select {
		case <-stream.done:
		case <-time.After(time.Second):
		}
	}
}

func diagnoseOpenCodeLocalFailure(tracker *openCodeProcessTracker, telemetry *agentanalysis.WorkspaceOpenCodeTelemetry) {
	if telemetry == nil || telemetry.LocalTransportFailure == "" {
		return
	}
	state, _, _, signal := tracker.snapshot()
	telemetry.ServerProcessState = state
	telemetry.ServerSignal = signal
	telemetry.CgroupOOMStatus = agentanalysis.WorkspaceCgroupOOMUnavailable
	if memory, ok := tracker.memoryDelta("/sys/fs/cgroup/memory.events"); ok {
		telemetry.CgroupOOMStatus = agentanalysis.WorkspaceCgroupOOMNotObserved
		if memory.OOMKill > 0 {
			telemetry.CgroupOOMStatus = agentanalysis.WorkspaceCgroupOOMObserved
		}
	}
	if !telemetry.LocalTransportRecovered && state == "signaled" && signal == "sigkill" && telemetry.CgroupOOMStatus == agentanalysis.WorkspaceCgroupOOMObserved {
		telemetry.FailureCode = "opencode_cgroup_oom"
	}
}

type cgroupMemoryEvents struct{ OOM, OOMKill, OOMGroupKill int }

func readCgroupMemoryEvents(path string) (cgroupMemoryEvents, bool) {
	data, err := os.ReadFile(path)
	if err != nil || len(data) > 4096 {
		return cgroupMemoryEvents{}, false
	}
	values := map[string]int{}
	seen := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		value, err := strconv.Atoi(fields[1])
		if err != nil || value < 0 {
			return cgroupMemoryEvents{}, false
		}
		values[fields[0]] = value
		seen[fields[0]] = true
	}
	if !seen["oom"] || !seen["oom_kill"] {
		return cgroupMemoryEvents{}, false
	}
	return cgroupMemoryEvents{OOM: values["oom"], OOMKill: values["oom_kill"], OOMGroupKill: values["oom_group_kill"]}, true
}

func (p *openCodeProcessTracker) memoryDelta(path string) (cgroupMemoryEvents, bool) {
	if p == nil || !p.memoryBaselineAvailable {
		return cgroupMemoryEvents{}, false
	}
	current, ok := readCgroupMemoryEvents(path)
	if !ok || current.OOM < p.memoryBaseline.OOM || current.OOMKill < p.memoryBaseline.OOMKill || current.OOMGroupKill < p.memoryBaseline.OOMGroupKill {
		return cgroupMemoryEvents{}, false
	}
	return cgroupMemoryEvents{
		OOM:          current.OOM - p.memoryBaseline.OOM,
		OOMKill:      current.OOMKill - p.memoryBaseline.OOMKill,
		OOMGroupKill: current.OOMGroupKill - p.memoryBaseline.OOMGroupKill,
	}, true
}

func persistedOpenCodePhase(err error, telemetryErr error, currentRequests, priorRequests int) bool {
	if _, ok := recoverableOpenCodeLocalEOF(err); !ok || telemetryErr != nil {
		return false
	}
	return currentRequests > priorRequests
}

func recoverOpenCodeStructuredOutput(raw []byte) ([]byte, error) {
	if len(raw) == 0 || len(raw) > maxOpenCodeTelemetryBytes {
		return nil, fmt.Errorf("OpenCode persisted finalization is unavailable")
	}
	var messages []openCodeMessage
	if err := json.Unmarshal(raw, &messages); err != nil || len(messages) > maxOpenCodeTelemetryEvents {
		return nil, fmt.Errorf("OpenCode persisted finalization is malformed")
	}
	for index := len(messages) - 1; index >= 0; index-- {
		structured := bytes.TrimSpace(messages[index].Info.Structured)
		if messages[index].Info.Role != "assistant" || len(structured) == 0 || bytes.Equal(structured, []byte("null")) {
			continue
		}
		var value any
		decoder := json.NewDecoder(bytes.NewReader(structured))
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("OpenCode persisted structured output is malformed")
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			return nil, fmt.Errorf("OpenCode persisted structured output has trailing data")
		}
		return append([]byte(nil), structured...), nil
	}
	return nil, fmt.Errorf("OpenCode persisted structured output is missing")
}
