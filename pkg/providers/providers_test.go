package providers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/choilab-jefferson/nexus.loom/pkg/compact"
	"github.com/choilab-jefferson/nexus.loom/pkg/policy"
)

func TestNormalizeStopReasonInvariantI1(t *testing.T) {
	tcs := []compact.ToolCall{{ID: "1", Name: "read_file", Arguments: map[string]any{"path": "a.go"}}}
	if got := NormalizeStopReason("stop", tcs); got != StopToolUse {
		t.Fatalf("expected StopToolUse when tool_calls present despite raw='stop', got %s", got)
	}
	if got := NormalizeStopReason("length", nil); got != StopMaxTokens {
		t.Fatalf("expected StopMaxTokens on 'length', got %s", got)
	}
	if got := NormalizeStopReason("weird_vendor_reason", nil); got != StopUnknown {
		t.Fatalf("expected StopUnknown on unrecognized reason, got %s", got)
	}
}

func TestReadNexusTokenStoreAndRedactSecret(t *testing.T) {
	// Hermetic: ambient env (e.g. IKB_MCP_TOKEN in a nexus session) beats the file.
	for _, k := range []string{"IKB_MCP_TOKEN", "TYPESAFE_API_KEY", "NEXUS_ANTHROPIC_API_KEY", "NEXUS_GEMINI_API_KEY"} {
		t.Setenv(k, "")
	}
	tmp := t.TempDir()
	t.Setenv("NEXUS_CONFIG_HOME", tmp)

	tokensContent := `# Nexus token store
NEXUS_ANTHROPIC_API_KEY="sk-ant-api03-1234567890abcdefghijklmnop"
export NEXUS_GEMINI_API_KEY='AIzaSyA1234567890abcdefghijklmnop'
NEXUS_GOOGLE_AGENT_API_KEY=AIzaSyGAP1234567890abcdefghijklm
TYPESAFE_API_KEY=ts-secret-token-999
IKB_MCP_TOKEN=ikb-token-xyz
`
	if err := os.WriteFile(filepath.Join(tmp, "tokens.env"), []byte(tokensContent), 0o600); err != nil {
		t.Fatal(err)
	}

	store := ReadNexusTokenStore()
	if store["NEXUS_ANTHROPIC_API_KEY"] != "sk-ant-api03-1234567890abcdefghijklmnop" {
		t.Fatalf("unexpected NEXUS_ANTHROPIC_API_KEY: %q", store["NEXUS_ANTHROPIC_API_KEY"])
	}
	if store["NEXUS_GEMINI_API_KEY"] != "AIzaSyA1234567890abcdefghijklmnop" {
		t.Fatalf("unexpected NEXUS_GEMINI_API_KEY: %q", store["NEXUS_GEMINI_API_KEY"])
	}
	if got := ResolveAPIKey("TYPESAFE_API_KEY"); got != "ts-secret-token-999" {
		t.Fatalf("expected ts-secret-token-999, got %q", got)
	}
	if got := ResolveAPIKey("IKB_MCP_TOKEN"); got != "ikb-token-xyz" {
		t.Fatalf("expected ikb-token-xyz, got %q", got)
	}

	rawErr := "failed request with key AIzaSyA1234567890abcdefghijklmnop and sk-ant-api03-1234567890abcdefghijklmnop"
	redacted := RedactSecret(rawErr, "ts-secret-token-999")
	if strings.Contains(redacted, "AIzaSy") || strings.Contains(redacted, "sk-ant") {
		t.Fatalf("expected secrets to be redacted, got %q", redacted)
	}
}

func TestPHIModeRefusesCloudProviders(t *testing.T) {
	t.Setenv("NEXUS_CONFIG_HOME", t.TempDir())
	ctx := context.Background()
	prefix := compact.PrefixHeader{SystemPrompt: "test"}
	msgs := []compact.Message{{Role: "user", Content: "hi"}}

	anth := NewAnthropicProvider("", "key", "claude-sonnet-4-5", true)
	if _, err := anth.Chat(ctx, prefix, msgs, 128); !policy.IsViolation(err) {
		t.Errorf("expected ViolationError from AnthropicProvider in PHIMode, got %v", err)
	}

	gem := NewGeminiProvider("", "key", "gemini-2.5-flash", true)
	if _, err := gem.Chat(ctx, prefix, msgs, 128); !policy.IsViolation(err) {
		t.Errorf("expected ViolationError from GeminiProvider in PHIMode, got %v", err)
	}

	gap := NewGoogleAgentPlatformProvider("", "key", "gemini-3.5-flash", true)
	if _, err := gap.Chat(ctx, prefix, msgs, 128); !policy.IsViolation(err) {
		t.Errorf("expected ViolationError from GoogleAgentPlatformProvider in PHIMode, got %v", err)
	}

	ts := NewTypeSafeProvider("", "key", "jev-latest", true)
	if _, err := ts.Chat(ctx, prefix, msgs, 128); !policy.IsViolation(err) {
		t.Errorf("expected ViolationError from TypeSafeProvider in PHIMode, got %v", err)
	}

	oai := NewOpenAIProvider("https://api.openai.com", "key", "gpt-4o-mini", true)
	if _, err := oai.Chat(ctx, prefix, msgs, 128); !policy.IsViolation(err) {
		t.Errorf("expected ViolationError from remote OpenAIProvider in PHIMode, got %v", err)
	}

	ollamaRemote := NewOllamaProvider("https://remote-llm.example.com", "qwen2.5-coder:32b", true)
	if _, err := ollamaRemote.Chat(ctx, prefix, msgs, 128); !policy.IsViolation(err) {
		t.Errorf("expected ViolationError from remote OllamaProvider in PHIMode, got %v", err)
	}
}

func TestGoogleAgentPlatformProviderURLAndHeader(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("NEXUS_CONFIG_HOME", tmp)
	if err := os.WriteFile(filepath.Join(tmp, "tokens.env"), []byte("NEXUS_GOOGLE_AGENT_API_KEY=gap-test-key-123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "google-agent-platform-credit.json"), []byte(`{"project":"nexus-gcp-proj","location":"global"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	gap := NewGoogleAgentPlatformProvider("", "", "gemini-3.5-flash", false)
	u, err := gap.EndpointURL()
	if err != nil {
		t.Fatalf("EndpointURL failed: %v", err)
	}
	wantURL := "https://aiplatform.googleapis.com/v1/projects/nexus-gcp-proj/locations/global/publishers/google/models/gemini-3.5-flash:generateContent"
	if u != wantURL {
		t.Fatalf("expected %s, got %s", wantURL, u)
	}

	var gotHeaderKey, gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaderKey = r.Header.Get("x-goog-api-key")
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"candidates": [{
				"finishReason": "STOP",
				"content": {
					"parts": [
						{"text": "thinking...", "thought": true},
						{"functionCall": {"name": "read_file", "args": {"path": "main.go"}}, "thoughtSignature": "sig-abc"}
					]
				}
			}],
			"usageMetadata": {"promptTokenCount": 77}
		}`))
	}))
	defer srv.Close()

	gapLocal := NewGoogleAgentPlatformProvider(srv.URL, "", "gemini-4-argon", false)
	resp, err := gapLocal.Chat(context.Background(), compact.PrefixHeader{SystemPrompt: "sys"}, []compact.Message{
		{Role: "user", Content: "check main.go"},
	}, 512)
	if err != nil {
		t.Fatalf("GoogleAgentPlatformProvider Chat failed: %v", err)
	}
	if gotHeaderKey != "gap-test-key-123" {
		t.Errorf("expected x-goog-api-key=gap-test-key-123, got %q", gotHeaderKey)
	}
	if !strings.HasSuffix(gotPath, "/publishers/google/models/gemini-4-argon:generateContent") {
		t.Errorf("unexpected request path: %s", gotPath)
	}
	// Thought part must be skipped in Content
	if strings.Contains(resp.Content, "thinking...") {
		t.Errorf("expected thought=true part to be skipped, got %q", resp.Content)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ThoughtSignature != "sig-abc" {
		t.Fatalf("expected tool call with ThoughtSignature=sig-abc, got %+v", resp.ToolCalls)
	}
	if resp.PromptTokens != 77 {
		t.Errorf("expected PromptTokens=77 from UsageMetadata, got %d", resp.PromptTokens)
	}
}

func TestTypeSafeProvider(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("NEXUS_CONFIG_HOME", tmp)
	if err := os.WriteFile(filepath.Join(tmp, "tokens.env"), []byte("TYPESAFE_API_KEY=ts-key-456\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":{"q1":"verified"},"usage":{"input_tokens":19}}`))
	}))
	defer srv.Close()

	prov := NewTypeSafeProvider(srv.URL, "", "jev-latest", false)
	resp, err := prov.Chat(context.Background(), compact.PrefixHeader{SystemPrompt: "sys"}, []compact.Message{
		{Role: "user", Content: "Verify invariant"},
	}, 256)
	if err != nil {
		t.Fatalf("TypeSafeProvider Chat failed: %v", err)
	}
	if gotAuth != "Bearer ts-key-456" {
		t.Errorf("expected Bearer ts-key-456, got %q", gotAuth)
	}
	stateObj, ok := gotBody["state"].(map[string]any)
	if !ok || !strings.Contains(stateObj["context"].(string), "Verify invariant") {
		t.Fatalf("expected state to be JSON object with context, got %#v", gotBody["state"])
	}
	questionsObj, ok := gotBody["questions"].(map[string]any)
	if !ok {
		t.Fatalf("expected questions map, got %#v", gotBody["questions"])
	}
	q1, ok := questionsObj["q1"].(map[string]any)
	if !ok || q1["type"] != "noul" || q1["instructions"] != "Verify invariant" {
		t.Fatalf("expected q1 question object with type=noul and instructions, got %#v", questionsObj["q1"])
	}
	if !strings.Contains(resp.Content, `"q1":"verified"`) || resp.PromptTokens != 19 {
		t.Errorf("unexpected TypeSafeProvider response: %+v", resp)
	}
}

func TestOllamaProviderLocalHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model": "qwen2.5-coder:32b",
			"done_reason": "stop",
			"prompt_eval_count": 42,
			"message": {
				"role": "assistant",
				"content": "",
				"tool_calls": [
					{"id": "call_1", "function": {"name": "list_dir", "arguments": {"path": "."}}}
				]
			}
		}`))
	}))
	defer srv.Close()

	prov := NewOllamaProvider(srv.URL, "qwen2.5-coder:32b", true)
	resp, err := prov.Chat(context.Background(), compact.PrefixHeader{SystemPrompt: "sys"}, []compact.Message{
		{Role: "user", Content: "list files"},
	}, 512)
	if err != nil {
		t.Fatalf("OllamaProvider local chat failed: %v", err)
	}
	if resp.StopReason != StopToolUse {
		t.Fatalf("expected StopToolUse, got %s", resp.StopReason)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "list_dir" {
		t.Fatalf("unexpected tool calls: %+v", resp.ToolCalls)
	}
}

func TestDoJSONPostAuthStatuses(t *testing.T) {
	for _, code := range []int{401, 403, 429, 500} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "nope", code)
		}))
		_, err := doJSONPost(context.Background(), srv.Client(), srv.URL, nil, map[string]any{}, "")
		srv.Close()
		if err == nil {
			t.Fatalf("code %d: expected error", code)
		}
		want := code == 401 || code == 403
		if IsAuthError(err) != want {
			t.Errorf("code %d: IsAuthError=%v want %v", code, IsAuthError(err), want)
		}
	}
}
