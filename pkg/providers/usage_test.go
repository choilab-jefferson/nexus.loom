package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/choilab-jefferson/nexus.loom/pkg/compact"
)

func TestOutputTokensParsedPerProvider(t *testing.T) {
	cases := map[string]struct {
		body string
		mk   func(url string) Provider
	}{
		"ollama": {`{"message":{"content":"x"},"done_reason":"stop","prompt_eval_count":3,"eval_count":11}`,
			func(u string) Provider { return NewOllamaProvider(u, "m", false) }},
		"openai": {`{"choices":[{"finish_reason":"stop","message":{"content":"x"}}],"usage":{"prompt_tokens":3,"completion_tokens":11}}`,
			func(u string) Provider { return NewOpenAIProvider(u, "k", "m", false) }},
		"anthropic": {`{"stop_reason":"end_turn","content":[{"type":"text","text":"x"}],"usage":{"input_tokens":3,"output_tokens":11}}`,
			func(u string) Provider { return NewAnthropicProvider(u, "k", "m", false) }},
		"gemini": {`{"candidates":[{"finishReason":"STOP","content":{"parts":[{"text":"x"}]}}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":11}}`,
			func(u string) Provider { return NewGeminiProvider(u, "k", "m", false) }},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()
			resp, err := c.mk(srv.URL).Chat(context.Background(), compact.PrefixHeader{SystemPrompt: "s"}, []compact.Message{{Role: "user", Content: "hi"}}, 64)
			if err != nil {
				t.Fatal(err)
			}
			if resp.OutputTokens != 11 {
				t.Fatalf("OutputTokens = %d, want 11", resp.OutputTokens)
			}
		})
	}
}
