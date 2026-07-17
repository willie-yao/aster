// Command orka-copilot-proxy is an OpenAI-compatible reverse proxy that lets
// Orka talk to GitHub Copilot's chat-completions API. It solves two
// Copilot-specific incompatibilities so an Orka `type: openai` Provider pointed
// at this service works with the caller's copilot_chat PAT:
//
//  1. Header: Copilot requires a Copilot-Integration-Id header that Orka's
//     Provider cannot set. The proxy injects it on every request.
//
//  2. Tool calls: Copilot's NON-streaming /chat/completions returns
//     finish_reason=tool_calls but a null tool_calls array for Claude models
//     (the actual calls only arrive over the streaming SSE). Orka's ai worker
//     uses the non-streaming path, so it never sees the tool calls and stops.
//     For requests that carry a tools array, the proxy forces stream:true
//     upstream, aggregates the SSE deltas (content + tool_calls + usage), and
//     returns a single normal non-streaming ChatCompletion the worker can parse.
//
// The bearer is passed through from Orka (from the Provider secretRef); the proxy
// holds no secret of its own. Requests without tools, and any non-chat path
// (e.g. the /responses probe), are proxied through unchanged.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	upstream := env("UPSTREAM", "https://api.githubcopilot.com")
	integrationID := env("COPILOT_INTEGRATION_ID", "copilot-developer-cli")
	addr := env("ADDR", ":8080")

	target, err := url.Parse(upstream)
	if err != nil {
		log.Fatalf("parse UPSTREAM %q: %v", upstream, err)
	}

	// Pass-through reverse proxy for requests that don't need de-streaming.
	passthrough := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
			req.URL.Path = normalizePath(req.URL.Path)
			req.Header.Set("Copilot-Integration-Id", integrationID)
		},
		ModifyResponse: func(resp *http.Response) error {
			log.Printf("↗ %s -> %d", resp.Request.URL.Path, resp.StatusCode)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("✖ proxy error for %s: %v", r.URL.Path, err)
			http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		},
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	transport.IdleConnTimeout = 90 * time.Second
	h := &handler{
		target:        target,
		integrationID: integrationID,
		passthrough:   passthrough,
		client:        &http.Client{Transport: transport},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	mux.Handle("/", h)

	log.Printf("orka-copilot-proxy on %s -> %s (integration-id=%s)", addr, upstream, integrationID)
	server := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
	log.Fatal(server.ListenAndServe())
}

func normalizePath(p string) string {
	// Both "/chat/completions" and "/v1/chat/completions" reach Copilot's
	// "/chat/completions".
	p = strings.TrimPrefix(p, "/v1")
	if p == "" {
		return "/"
	}
	return p
}

type handler struct {
	target        *url.URL
	integrationID string
	passthrough   *httputil.ReverseProxy
	client        *http.Client
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := normalizePath(r.URL.Path)
	if r.Method != http.MethodPost || !strings.HasSuffix(path, "/chat/completions") {
		h.passthrough.ServeHTTP(w, r)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		// Not JSON we understand; pass through untouched.
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		h.passthrough.ServeHTTP(w, r)
		return
	}

	_, hasTools := req["tools"]
	streamRequested, _ := req["stream"].(bool)
	// Only de-stream non-streaming tool requests, the case Copilot breaks.
	if !hasTools || streamRequested {
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		h.passthrough.ServeHTTP(w, r)
		return
	}

	h.destream(w, r, req)
}

// destream forces stream:true upstream, aggregates the SSE, and writes a single
// non-streaming ChatCompletion response.
func (h *handler) destream(w http.ResponseWriter, r *http.Request, req map[string]any) {
	req["stream"] = true
	req["stream_options"] = map[string]any{"include_usage": true}
	newBody, _ := json.Marshal(req)

	up := *h.target
	up.Path = normalizePath(r.URL.Path)
	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, up.String(), bytes.NewReader(newBody))
	if err != nil {
		http.Error(w, "build upstream request: "+err.Error(), http.StatusBadGateway)
		return
	}
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("Accept", "text/event-stream")
	upReq.Header.Set("Copilot-Integration-Id", h.integrationID)
	if auth := r.Header.Get("Authorization"); auth != "" {
		upReq.Header.Set("Authorization", auth)
	}

	resp, err := h.client.Do(upReq)
	if err != nil {
		http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		// Surface upstream errors verbatim so Orka's fallback logic can act.
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		log.Printf("↗ %s (destream) -> %d", up.Path, resp.StatusCode)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(raw)
		return
	}

	agg, err := aggregateSSE(resp.Body)
	if err != nil {
		http.Error(w, "aggregate stream: "+err.Error(), http.StatusBadGateway)
		return
	}
	log.Printf("↗ %s (destream) -> 200 finish=%s tool_calls=%d", up.Path, agg.finishReason, len(agg.toolCalls))

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(agg.toChatCompletion())
}

type aggregated struct {
	id           string
	model        string
	created      int64
	content      strings.Builder
	finishReason string
	toolCalls    []map[string]any
	usage        map[string]any
}

// aggregateSSE reads an OpenAI-style chat-completions SSE stream and merges the
// deltas into a single non-streaming response. Tool calls are keyed by their SSE
// index, which is provider-specific: OpenAI uses dense 0-based indices, while
// Copilot's Claude route uses 1-based indices with the text preamble at 0. A map
// keyed by index (rather than a dense slice) handles both without phantom
// entries.
func aggregateSSE(body io.Reader) (*aggregated, error) {
	agg := &aggregated{}
	type tcState struct {
		id, typ, name string
		args          strings.Builder
	}
	byIndex := map[int]*tcState{}
	var order []int

	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk struct {
			ID      string `json:"id"`
			Model   string `json:"model"`
			Created int64  `json:"created"`
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage map[string]any `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue // tolerate keep-alive or non-JSON lines
		}
		if chunk.ID != "" {
			agg.id = chunk.ID
		}
		if chunk.Model != "" {
			agg.model = chunk.Model
		}
		if chunk.Created != 0 {
			agg.created = chunk.Created
		}
		if chunk.Usage != nil {
			agg.usage = chunk.Usage
		}
		for _, ch := range chunk.Choices {
			agg.content.WriteString(ch.Delta.Content)
			if ch.FinishReason != "" {
				agg.finishReason = ch.FinishReason
			}
			for _, tc := range ch.Delta.ToolCalls {
				st, ok := byIndex[tc.Index]
				if !ok {
					st = &tcState{typ: "function"}
					byIndex[tc.Index] = st
					order = append(order, tc.Index)
				}
				if tc.ID != "" {
					st.id = tc.ID
				}
				if tc.Type != "" {
					st.typ = tc.Type
				}
				if tc.Function.Name != "" {
					st.name = tc.Function.Name
				}
				st.args.WriteString(tc.Function.Arguments)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	sort.Ints(order)
	for _, idx := range order {
		st := byIndex[idx]
		if st.name == "" {
			continue // drop indices that never named a function
		}
		agg.toolCalls = append(agg.toolCalls, map[string]any{
			"id":   st.id,
			"type": st.typ,
			"function": map[string]any{
				"name":      st.name,
				"arguments": st.args.String(),
			},
		})
	}
	return agg, nil
}

func (a *aggregated) toChatCompletion() map[string]any {
	message := map[string]any{
		"role":    "assistant",
		"content": a.content.String(),
	}
	if len(a.toolCalls) > 0 {
		message["tool_calls"] = a.toolCalls
	}
	finish := a.finishReason
	if finish == "" {
		finish = "stop"
	}
	out := map[string]any{
		"id":      a.id,
		"object":  "chat.completion",
		"created": a.created,
		"model":   a.model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": finish,
		}},
	}
	if a.usage != nil {
		out["usage"] = a.usage
	}
	return out
}
