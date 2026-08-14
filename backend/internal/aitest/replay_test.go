package aitest

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/willie-yao/aster/backend/internal/ai"
)

func TestFingerprint_StableAndDistinct(t *testing.T) {
	a := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"x":1}]}`)
	// Same conversation, different tool schema -> same fingerprint.
	b := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"y":2}],"parallel_tool_calls":false}`)
	// Different message -> different fingerprint.
	c := []byte(`{"model":"m","messages":[{"role":"user","content":"bye"}]}`)
	fa, _ := Fingerprint(a)
	fb, _ := Fingerprint(b)
	fc, _ := Fingerprint(c)
	if fa != fb {
		t.Errorf("tool-schema/flags should not affect fingerprint: %s vs %s", fa, fb)
	}
	if fa == fc {
		t.Errorf("different message must change fingerprint")
	}
}

// TestReplayServer_RecordThenReplay records a response from a fake upstream and
// then serves the identical request from the fixture without hitting upstream,
// proving the record/replay seam works with the real ai.Client.
func TestReplayServer_RecordThenReplay(t *testing.T) {
	var upstreamHits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamHits, 1)
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"hello from upstream"}}]}`)
	}))
	defer upstream.Close()

	dir := t.TempDir()

	rec := NewRecordingServer(t, dir, upstream.URL, "tok")
	c1 := ai.NewClientWithOptions(ai.Options{Endpoint: rec.URL, Model: "m", CacheDir: t.TempDir()})
	out, err := c1.Complete(context.Background(), "sys prompt", "user prompt")
	if err != nil {
		t.Fatalf("record Complete: %v", err)
	}
	if out != "hello from upstream" {
		t.Fatalf("record out = %q", out)
	}
	if got := atomic.LoadInt32(&upstreamHits); got != 1 {
		t.Fatalf("upstream hits during record = %d, want 1", got)
	}

	rep := NewReplayServer(t, dir)
	c2 := ai.NewClientWithOptions(ai.Options{Endpoint: rep.URL, Model: "m", CacheDir: t.TempDir()})
	out2, err := c2.Complete(context.Background(), "sys prompt", "user prompt")
	if err != nil {
		t.Fatalf("replay Complete: %v", err)
	}
	if out2 != "hello from upstream" {
		t.Errorf("replay out = %q, want recorded value", out2)
	}
	if got := atomic.LoadInt32(&upstreamHits); got != 1 {
		t.Errorf("upstream hit during replay (hits=%d); should serve from fixture", got)
	}
	if served, _ := rep.Stats(); served != 1 {
		t.Errorf("replay served = %d, want 1", served)
	}
}

func TestFingerprint_NormalizesVolatileToolFields(t *testing.T) {
	// Two second-turn requests identical except for the volatile envelope fields
	// the agentic loop stamps on tool results must share a fingerprint, so a
	// multi-turn conversation replays deterministically across runs.
	mk := func(elapsed, remModel int) []byte {
		return []byte(`{"model":"m","messages":[` +
			`{"role":"user","content":"go"},` +
			`{"role":"assistant","content":null,"tool_calls":[{"id":"c1","function":{"name":"read_artifact","arguments":"{}"}}]},` +
			`{"role":"tool","tool_call_id":"c1","content":"{\"path\":\"build-log.txt\",\"elapsed_seconds\":` +
			strconv.Itoa(elapsed) + `,\"remaining_model_bytes\":` + strconv.Itoa(remModel) + `}"}` +
			`]}`)
	}
	fa, err := Fingerprint(mk(3, 999000))
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	fb, err := Fingerprint(mk(41, 412000))
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if fa != fb {
		t.Errorf("volatile tool-result fields must not affect fingerprint: %s vs %s", fa, fb)
	}
}

func TestReplayServer_Seed(t *testing.T) {
	rs := NewReplayServer(t, "")
	rs.Seed(
		[]byte(`{"model":"m","messages":[{"role":"system","content":""},{"role":"user","content":"ping"}]}`),
		[]byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"pong"}}]}`),
	)
	c := ai.NewClientWithOptions(ai.Options{Endpoint: rs.URL, Model: "m", CacheDir: t.TempDir()})
	out, err := c.Complete(context.Background(), "", "ping")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if out != "pong" {
		t.Errorf("out = %q, want pong", out)
	}
}
