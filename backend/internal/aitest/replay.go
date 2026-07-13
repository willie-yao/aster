// Package aitest provides test-support helpers for exercising the agentic AI
// pipeline without a live model: a record/replay chat-completions server that
// serves deterministic responses keyed by a stable request fingerprint.
package aitest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/textutil"
)

// volatileToolFields are envelope keys the agentic loop stamps onto every tool
// result that vary between a record run and a later replay run (wall-clock and
// running budget counters). They are stripped before fingerprinting so the same
// logical conversation replays deterministically across runs.
var volatileToolFields = []string{"elapsed_seconds", "remaining_model_bytes", "remaining_gcs_bytes"}

// Fingerprint hashes the salient parts of an OpenAI-style chat-completion
// request body: the model and the message sequence (roles, contents, names,
// tool-call IDs, and tool-call function name+arguments). Tool schemas and
// parallel_tool_calls are excluded because they are stable per run and do not
// change the model's reply for a given conversation state. Volatile fields in
// tool-result envelopes are normalized out so a multi-turn agentic conversation
// produces the same fingerprint on record and replay. The same conversation
// state therefore always maps to the same recorded response.
func Fingerprint(body []byte) (string, error) {
	var req struct {
		Model    string `json:"model"`
		Messages []struct {
			Role       string  `json:"role"`
			Content    *string `json:"content"`
			Name       string  `json:"name"`
			ToolCallID string  `json:"tool_call_id"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return "", fmt.Errorf("aitest: fingerprint decode: %w", err)
	}
	for i := range req.Messages {
		if req.Messages[i].Role == "tool" && req.Messages[i].Content != nil {
			normalized := normalizeToolContent(*req.Messages[i].Content)
			req.Messages[i].Content = &normalized
		}
	}
	canon, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:]), nil
}

// normalizeToolContent drops volatile envelope fields from a tool-result JSON
// body, preserving all other fields byte-for-byte. Non-JSON or non-object
// content is returned unchanged.
func normalizeToolContent(content string) string {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(content), &obj); err != nil {
		return content
	}
	for _, k := range volatileToolFields {
		delete(obj, k)
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return content
	}
	return string(out)
}

// ReplayServer is an httptest server that answers chat-completion requests from
// recorded fixtures. In replay mode (the default) a request with no recorded
// response fails the test. In record mode it proxies the request to a real
// upstream endpoint, saves the response under the fixtures dir, and returns it.
type ReplayServer struct {
	*httptest.Server
	t   *testing.T
	dir string

	mu      sync.Mutex
	seeded  map[string][]byte
	served  int
	misses  int
	recLock map[string]*sync.Mutex

	writeMu  sync.Mutex
	record   bool
	upstream string
	token    string
	client   *http.Client
}

// NewReplayServer serves recorded responses from dir (may be empty). A request
// with no recorded or seeded response records a test failure and returns 500.
func NewReplayServer(t *testing.T, dir string) *ReplayServer {
	t.Helper()
	rs := &ReplayServer{t: t, dir: dir, seeded: map[string][]byte{}, recLock: map[string]*sync.Mutex{}}
	rs.Server = httptest.NewServer(http.HandlerFunc(rs.handle))
	t.Cleanup(rs.Close)
	return rs
}

// NewRecordingServer proxies misses to upstream (an OpenAI-compatible
// chat-completions URL) with a Bearer token, writes each response under dir as
// <fingerprint>.json, and returns it. Use it once to capture fixtures, then
// switch the test to NewReplayServer.
func NewRecordingServer(t *testing.T, dir, upstream, token string) *ReplayServer {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("aitest: mkdir fixtures: %v", err)
	}
	rs := &ReplayServer{
		t: t, dir: dir, seeded: map[string][]byte{}, recLock: map[string]*sync.Mutex{},
		record: true, upstream: upstream, token: token,
		client: &http.Client{Timeout: 5 * time.Minute},
	}
	rs.Server = httptest.NewServer(http.HandlerFunc(rs.handle))
	t.Cleanup(rs.Close)
	return rs
}

// Seed registers an in-memory response for the conversation state described by
// requestBody, so a scripted scenario can supply responses without writing
// fixture files. The request body need only contain the salient fields used by
// Fingerprint.
func (rs *ReplayServer) Seed(requestBody, responseBody []byte) {
	fp, err := Fingerprint(requestBody)
	if err != nil {
		rs.t.Fatalf("aitest: seed fingerprint: %v", err)
	}
	rs.mu.Lock()
	rs.seeded[fp] = append([]byte(nil), responseBody...)
	rs.mu.Unlock()
}

// Stats returns how many requests were served from fixtures/seeds and how many
// missed. Useful for asserting cache behavior (a cached fetch serves 0).
func (rs *ReplayServer) Stats() (served, misses int) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.served, rs.misses
}

func (rs *ReplayServer) lookup(fp string) ([]byte, bool) {
	rs.mu.Lock()
	if data, ok := rs.seeded[fp]; ok {
		rs.mu.Unlock()
		return data, true
	}
	rs.mu.Unlock()
	if rs.dir == "" {
		return nil, false
	}
	data, err := os.ReadFile(filepath.Join(rs.dir, fp+".json"))
	if err != nil {
		return nil, false
	}
	return data, true
}

func (rs *ReplayServer) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	fp, err := Fingerprint(body)
	if err != nil {
		rs.t.Errorf("aitest: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if data, ok := rs.lookup(fp); ok {
		rs.mu.Lock()
		rs.served++
		rs.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
		return
	}
	rs.mu.Lock()
	rs.misses++
	rs.mu.Unlock()
	if rs.record {
		rs.recordOnce(w, fp, body)
		return
	}
	// Replay miss: surface the unmatched request so fixtures can be updated.
	rs.t.Errorf("aitest: no recorded response for fingerprint %s; request head: %s", fp, textutil.Truncate(string(body), 400))
	http.Error(w, "aitest: no recorded response", http.StatusInternalServerError)
}

// fpLock returns a per-fingerprint mutex so record mode serializes concurrent
// misses for the same conversation state: only the first calls upstream and
// writes the fixture, and later callers serve that same recorded response. This
// keeps a concurrently-recorded fixture set internally consistent.
func (rs *ReplayServer) fpLock(fp string) *sync.Mutex {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	lk, ok := rs.recLock[fp]
	if !ok {
		lk = &sync.Mutex{}
		rs.recLock[fp] = lk
	}
	return lk
}

// recordOnce serializes recording per fingerprint, re-checking for a fixture
// another goroutine may have just written before proxying upstream.
func (rs *ReplayServer) recordOnce(w http.ResponseWriter, fp string, body []byte) {
	lk := rs.fpLock(fp)
	lk.Lock()
	defer lk.Unlock()
	if data, ok := rs.lookup(fp); ok {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
		return
	}
	rs.proxyAndRecord(w, fp, body)
}

// proxyAndRecord forwards a miss to the real upstream, atomically writes an OK
// response under dir as <fingerprint>.json, and returns it. Errors are reported
// via t.Errorf (goroutine-safe) and a 500 rather than t.Fatalf, which must not
// be called from a non-test goroutine.
func (rs *ReplayServer) proxyAndRecord(w http.ResponseWriter, fp string, body []byte) {
	req, err := http.NewRequest(http.MethodPost, rs.upstream, bytes.NewReader(body))
	if err != nil {
		rs.t.Errorf("aitest: build upstream request: %v", err)
		http.Error(w, "aitest: build upstream request", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if rs.token != "" {
		req.Header.Set("Authorization", "Bearer "+rs.token)
	}
	req.Header.Set("Copilot-Integration-Id", "copilot-developer-cli")
	resp, err := rs.client.Do(req)
	if err != nil {
		rs.t.Errorf("aitest: upstream call: %v", err)
		http.Error(w, "aitest: upstream call", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK {
		if err := rs.writeFixture(fp, respBody); err != nil {
			rs.t.Errorf("aitest: write fixture: %v", err)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBody)
}

// writeFixture writes the fixture atomically (temp file then rename) under a
// per-server lock so concurrent records cannot leave a partially written file.
func (rs *ReplayServer) writeFixture(fp string, data []byte) error {
	rs.writeMu.Lock()
	defer rs.writeMu.Unlock()
	final := filepath.Join(rs.dir, fp+".json")
	tmp, err := os.CreateTemp(rs.dir, fp+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, final)
}
