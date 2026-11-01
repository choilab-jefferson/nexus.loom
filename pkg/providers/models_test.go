package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/choilab-jefferson/nexus.loom/pkg/compact"
)

func TestUnknownModelMapsToModelNotFoundNoRetry(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		mk     func(url string) Provider
	}{
		{"gemini", 404, `{"error":{"message":"models/nope is not found for API version v1beta"}}`, func(u string) Provider { return NewGeminiProvider(u, "k", "nope", false) }},
		{"openai", 404, `{"error":{"message":"The model nope does not exist"}}`, func(u string) Provider { return NewOpenAIProvider(u, "k", "nope", false) }},
		{"anthropic", 404, `{"error":{"type":"not_found_error","message":"model: nope"}}`, func(u string) Provider { return NewAnthropicProvider(u, "k", "nope", false) }},
		{"ollama", 404, `{"error":"model 'nope' not found, try pulling it first"}`, func(u string) Provider { return NewOllamaProvider(u, "nope", false) }},
		{"gemini", 400, `{"error":{"message":"model nope is not supported"}}`, func(u string) Provider { return NewGeminiProvider(u, "k", "nope", false) }},
	}
	for _, c := range cases {
		calls := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.WriteHeader(c.status)
			_, _ = w.Write([]byte(c.body))
		}))
		_, err := c.mk(srv.URL).Chat(context.Background(), compact.PrefixHeader{}, []compact.Message{{Role: "user", Content: "hi"}}, 16)
		srv.Close()
		if !IsModelNotFound(err) || calls != 1 {
			t.Fatalf("%s/%d: err=%v calls=%d", c.name, c.status, err, calls)
		}
		want := `loom: model "nope" not found or not served by provider ` + c.name + `; run "loom models --provider ` + c.name + `" to list available ids`
		if err.Error() != want || strings.Contains(err.Error(), "\n") {
			t.Fatalf("msg=%q", err.Error())
		}
	}
}

func TestOther400IsNotModelNotFound(t *testing.T) {
	if isModelNotFound(400, []byte(`{"error":"max_tokens too large"}`)) || isModelNotFound(500, []byte("model not found")) {
		t.Fatal("misclassified")
	}
}

func TestListModelsAt(t *testing.T) {
	cases := []struct {
		kind, path, body string
		want             string
	}{
		{"gemini", "/v1beta/models", `{"models":[{"name":"models/gemini-b"},{"name":"models/gemini-a"}]}`, "gemini-a,gemini-b"},
		{"openai", "/v1/models", `{"data":[{"id":"gpt-x"},{"id":"gpt-a"}]}`, "gpt-a,gpt-x"},
		{"anthropic", "/v1/models", `{"data":[{"id":"claude-1"}]}`, "claude-1"},
		{"ollama", "/api/tags", `{"models":[{"name":"qwen3:4b"}]}`, "qwen3:4b"},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || r.URL.Path != c.path {
				t.Errorf("%s: %s %s", c.kind, r.Method, r.URL.Path)
			}
			_, _ = w.Write([]byte(c.body))
		}))
		ids, err := ListModelsAt(context.Background(), c.kind, srv.URL, "key")
		srv.Close()
		if err != nil || strings.Join(ids, ",") != c.want {
			t.Fatalf("%s: %v %v", c.kind, ids, err)
		}
	}
}

func TestListModelsErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }))
	defer srv.Close()
	if _, err := ListModelsAt(context.Background(), "openai", srv.URL, "key"); err == nil {
		t.Fatal("want http error")
	}
	if _, err := ListModelsAt(context.Background(), "openai", srv.URL, ""); err == nil {
		t.Fatal("want missing key error")
	}
	if _, err := ListModels(context.Background(), "typesafe"); err == nil {
		t.Fatal("want unsupported")
	}
}
