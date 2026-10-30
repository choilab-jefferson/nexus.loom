package providers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/choilab-jefferson/nexus.loom/pkg/compact"
)

var (
	wirePrefix = compact.PrefixHeader{SystemPrompt: "sys"}
	wireMsgs   = []compact.Message{{Role: "user", Content: "hi"}}
)

func fastRetries(t *testing.T) {
	t.Helper()
	old := retryBaseDelay
	retryBaseDelay = time.Millisecond
	t.Cleanup(func() { retryBaseDelay = old })
}

func TestRetry429HonorsRetryAfterThen200(t *testing.T) {
	fastRetries(t)
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	start := time.Now()
	body, err := doJSONPost(context.Background(), srv.Client(), srv.URL, nil, map[string]any{}, "")
	if err != nil {
		t.Fatalf("expected success after retry: %v", err)
	}
	if string(body) != `{"ok":true}` || atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("body=%q calls=%d", body, calls)
	}
	if el := time.Since(start); el < 900*time.Millisecond {
		t.Fatalf("Retry-After not honored, elapsed %v", el)
	}
}

func TestRetry5xxExhaustsAndNoRetryOn4xx(t *testing.T) {
	fastRetries(t)
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	if _, err := doJSONPost(context.Background(), srv.Client(), srv.URL, nil, map[string]any{}, ""); err == nil {
		t.Fatal("expected error")
	}
	if got := atomic.LoadInt32(&calls); got != int32(maxRetries+1) {
		t.Fatalf("expected %d attempts, got %d", maxRetries+1, got)
	}

	atomic.StoreInt32(&calls, 0)
	srv4 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv4.Close()
	_, _ = doJSONPost(context.Background(), srv4.Client(), srv4.URL, nil, map[string]any{}, "")
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("400 must not be retried, calls=%d", calls)
	}
}

func TestRetryStopsOnContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := doJSONPost(ctx, srv.Client(), srv.URL, nil, map[string]any{}, ""); err == nil {
		t.Fatal("expected ctx error")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("backoff ignored context")
	}
}

func TestOversizedBodyRejected(t *testing.T) {
	old := maxResponseBytes
	maxResponseBytes = 1024
	defer func() { maxResponseBytes = old }()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 4096)))
	}))
	defer srv.Close()
	_, err := doJSONPost(context.Background(), srv.Client(), srv.URL, nil, map[string]any{}, "")
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected size error, got %v", err)
	}
}

func TestHTTPTimeoutEnv(t *testing.T) {
	t.Setenv("LOOM_HTTP_TIMEOUT", "")
	if httpTimeout() != 120*time.Second {
		t.Fatal("default")
	}
	t.Setenv("LOOM_HTTP_TIMEOUT", "7s")
	if httpTimeout() != 7*time.Second {
		t.Fatal("duration")
	}
	t.Setenv("LOOM_HTTP_TIMEOUT", "9")
	if httpTimeout() != 9*time.Second {
		t.Fatal("seconds")
	}
	t.Setenv("LOOM_HTTP_TIMEOUT", "junk")
	if httpTimeout() != 120*time.Second {
		t.Fatal("fallback")
	}
	if NewOpenAIProvider("http://127.0.0.1:1", "k", "m", false).Client.Timeout != 120*time.Second {
		t.Fatal("provider should use fallback timeout")
	}
	t.Setenv("LOOM_HTTP_TIMEOUT", "3s")
	if NewAnthropicProvider("", "k", "m", false).Client.Timeout != 3*time.Second {
		t.Fatal("provider should pick up env timeout")
	}
}

// capture serves a canned response and records the request.
func capture(t *testing.T, resp string, path *string, hdr *http.Header, body *map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*path = r.URL.Path
		*hdr = r.Header.Clone()
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, body)
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestOpenAIWire(t *testing.T) {
	var path string
	var hdr http.Header
	var body map[string]any
	srv := capture(t, `{"choices":[{"finish_reason":"tool_calls","message":{"content":"ok","tool_calls":[{"id":"c1","function":{"name":"read_file","arguments":"{\"path\":\"a.go\"}"}}]}}],"usage":{"prompt_tokens":11}}`, &path, &hdr, &body)

	resp, err := NewOpenAIProvider(srv.URL, "sk-x", "gpt-4o", false).Chat(context.Background(), wirePrefix, wireMsgs, 300)
	if err != nil {
		t.Fatal(err)
	}
	if path != "/v1/chat/completions" || hdr.Get("Authorization") != "Bearer sk-x" {
		t.Fatalf("path=%s auth=%s", path, hdr.Get("Authorization"))
	}
	if body["max_tokens"] != float64(300) || body["temperature"] != float64(0) || body["max_completion_tokens"] != nil {
		t.Fatalf("classic shape wrong: %v", body)
	}
	if body["model"] != "gpt-4o" || body["messages"].([]any)[0].(map[string]any)["role"] != "system" {
		t.Fatalf("bad body: %v", body)
	}
	if resp.Content != "ok" || resp.PromptTokens != 11 || resp.StopReason != StopToolUse ||
		len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Arguments["path"] != "a.go" {
		t.Fatalf("bad parse: %+v", resp)
	}
}

func TestOpenAIReasoningModelShape(t *testing.T) {
	for _, m := range []string{"o3-mini", "o1", "gpt-5", "gpt-5-mini"} {
		var path string
		var hdr http.Header
		var body map[string]any
		srv := capture(t, `{"choices":[{"finish_reason":"stop","message":{"content":"x"}}]}`, &path, &hdr, &body)
		if _, err := NewOpenAIProvider(srv.URL, "k", m, false).Chat(context.Background(), wirePrefix, wireMsgs, 64); err != nil {
			t.Fatal(err)
		}
		if body["max_completion_tokens"] != float64(64) {
			t.Errorf("%s: want max_completion_tokens, got %v", m, body)
		}
		if _, ok := body["max_tokens"]; ok {
			t.Errorf("%s: max_tokens must be absent", m)
		}
		if _, ok := body["temperature"]; ok {
			t.Errorf("%s: temperature must be absent", m)
		}
	}
	for _, m := range []string{"gpt-4o", "gpt-4.1", "llama3", "omni-x"} {
		if isOpenAIReasoningModel(m) {
			t.Errorf("%s misclassified as reasoning", m)
		}
	}
}

func TestAnthropicWire(t *testing.T) {
	var path string
	var hdr http.Header
	var body map[string]any
	srv := capture(t, `{"stop_reason":"tool_use","content":[{"type":"text","text":"hello"},{"type":"tool_use","id":"tu1","name":"read_file","input":{"path":"b.go"}}],"usage":{"input_tokens":21}}`, &path, &hdr, &body)

	resp, err := NewAnthropicProvider(srv.URL, "ak", "claude-x", false).Chat(context.Background(), wirePrefix, wireMsgs, 256)
	if err != nil {
		t.Fatal(err)
	}
	if path != "/v1/messages" || hdr.Get("x-api-key") != "ak" || hdr.Get("anthropic-version") != "2023-06-01" {
		t.Fatalf("path=%s hdr=%v", path, hdr)
	}
	if body["max_tokens"] != float64(256) || body["model"] != "claude-x" {
		t.Fatalf("bad body: %v", body)
	}
	sys := body["system"].([]any)[0].(map[string]any)
	if sys["cache_control"].(map[string]any)["type"] != "ephemeral" {
		t.Fatalf("missing cache_control: %v", sys)
	}
	if resp.Content != "hello" || resp.PromptTokens != 21 || resp.StopReason != StopToolUse ||
		len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "tu1" || resp.ToolCalls[0].Arguments["path"] != "b.go" {
		t.Fatalf("bad parse: %+v", resp)
	}
}

func TestGeminiWire(t *testing.T) {
	var path string
	var hdr http.Header
	var body map[string]any
	srv := capture(t, `{"candidates":[{"finishReason":"STOP","content":{"parts":[{"text":"hi"},{"functionCall":{"name":"read_file","args":{"path":"c.go"}}}]}}],"usageMetadata":{"promptTokenCount":5}}`, &path, &hdr, &body)

	resp, err := NewGeminiProvider(srv.URL, "gk", "models/gemini-3.5-flash", false).Chat(context.Background(), wirePrefix, wireMsgs, 128)
	if err != nil {
		t.Fatal(err)
	}
	if path != "/v1beta/models/gemini-3.5-flash:generateContent" || hdr.Get("x-goog-api-key") != "gk" {
		t.Fatalf("path=%s hdr=%v", path, hdr)
	}
	cfg := body["generationConfig"].(map[string]any)
	if cfg["maxOutputTokens"] != float64(128) || cfg["temperature"] != float64(0) {
		t.Fatalf("bad generationConfig: %v", cfg)
	}
	if body["systemInstruction"] == nil || body["contents"].([]any)[0].(map[string]any)["role"] != "user" {
		t.Fatalf("bad body: %v", body)
	}
	if resp.Content != "hi" || resp.PromptTokens != 5 || resp.StopReason != StopToolUse ||
		len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Arguments["path"] != "c.go" {
		t.Fatalf("bad parse: %+v", resp)
	}
}
