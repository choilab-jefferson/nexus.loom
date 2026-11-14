package providers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/choilab-jefferson/nexus.loom/pkg/compact"
	"github.com/choilab-jefferson/nexus.loom/pkg/tools"
)

// captureServer records the last request body and answers with a fixed JSON reply.
func captureServer(t *testing.T, reply string, body *map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func onlyReadFilePrefix(t *testing.T) compact.PrefixHeader {
	t.Helper()
	s, err := tools.FilterSchemas([]string{"read_file"})
	if err != nil {
		t.Fatal(err)
	}
	return compact.PrefixHeader{SystemPrompt: "sys", ToolSchemasJSON: tools.SchemasJSON(s)}
}

func toolNamesIn(t *testing.T, v any, nameOf func(map[string]any) string) []string {
	t.Helper()
	list, _ := v.([]any)
	var out []string
	for _, e := range list {
		m, _ := e.(map[string]any)
		out = append(out, nameOf(m))
	}
	return out
}

func fnName(m map[string]any) string {
	f, _ := m["function"].(map[string]any)
	n, _ := f["name"].(string)
	return n
}

func plainName(m map[string]any) string { n, _ := m["name"].(string); return n }

func wantOnlyReadFile(t *testing.T, provider string, names []string) {
	t.Helper()
	if len(names) != 1 || names[0] != "read_file" {
		t.Fatalf("%s rendered tools %v, want only [read_file]", provider, names)
	}
}

var userMsg = []compact.Message{{Role: "user", Content: "hi"}}

func TestProvidersRenderOnlyPrefixTools(t *testing.T) {
	prefix := onlyReadFilePrefix(t)

	var ob map[string]any
	osrv := captureServer(t, `{"done_reason":"stop","message":{"role":"assistant","content":"ok"}}`, &ob)
	if _, err := NewOllamaProvider(osrv.URL, "m", false).Chat(context.Background(), prefix, userMsg, 64); err != nil {
		t.Fatal(err)
	}
	wantOnlyReadFile(t, "ollama", toolNamesIn(t, ob["tools"], fnName))

	var oa map[string]any
	asrv := captureServer(t, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`, &oa)
	if _, err := NewOpenAIProvider(asrv.URL, "k", "gpt-4o-mini", false).Chat(context.Background(), prefix, userMsg, 64); err != nil {
		t.Fatal(err)
	}
	wantOnlyReadFile(t, "openai", toolNamesIn(t, oa["tools"], fnName))

	var an map[string]any
	nsrv := captureServer(t, `{"stop_reason":"end_turn","content":[{"type":"text","text":"ok"}]}`, &an)
	if _, err := NewAnthropicProvider(nsrv.URL, "k", "m", false).Chat(context.Background(), prefix, userMsg, 64); err != nil {
		t.Fatal(err)
	}
	wantOnlyReadFile(t, "anthropic", toolNamesIn(t, an["tools"], plainName))

	var gm map[string]any
	gsrv := captureServer(t, `{"candidates":[{"finishReason":"STOP","content":{"role":"model","parts":[{"text":"ok"}]}}]}`, &gm)
	if _, err := NewGeminiProvider(gsrv.URL, "k", "gemini-x", false).Chat(context.Background(), prefix, userMsg, 64); err != nil {
		t.Fatal(err)
	}
	tl, _ := gm["tools"].([]any)
	if len(tl) != 1 {
		t.Fatalf("gemini tools block = %v", gm["tools"])
	}
	decl, _ := tl[0].(map[string]any)
	wantOnlyReadFile(t, "gemini", toolNamesIn(t, decl["functionDeclarations"], plainName))
}
