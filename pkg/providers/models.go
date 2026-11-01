package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// ModelNotFoundError means the provider answered that the requested model id
// does not exist or is not served for this key. The engine maps it to exit 2
// (config error) with no retry. Provider and Model are filled by tagModelErr.
type ModelNotFoundError struct {
	Provider string
	Model    string
	Status   int
	Detail   string
}

func (e *ModelNotFoundError) Error() string {
	return fmt.Sprintf("loom: model %q not found or not served by provider %s; run \"loom models --provider %s\" to list available ids", e.Model, e.Provider, e.Provider)
}

// IsModelNotFound reports whether err is or wraps a *ModelNotFoundError.
func IsModelNotFound(err error) bool {
	var m *ModelNotFoundError
	return errors.As(err, &m)
}

// tagModelErr fills provider and model on a ModelNotFoundError; other errors pass through.
func tagModelErr(err error, provider, model string) error {
	var m *ModelNotFoundError
	if errors.As(err, &m) {
		m.Provider, m.Model = provider, model
	}
	return err
}

// isModelNotFound classifies a non-2xx response from the provider's own body.
// No allowlist of ids: 404 naming a model, or 400 saying the model is unknown.
func isModelNotFound(status int, body []byte) bool {
	b := strings.ToLower(string(body))
	switch status {
	case http.StatusNotFound:
		return strings.Contains(b, "model") || strings.Contains(b, "not found")
	case http.StatusBadRequest:
		if !strings.Contains(b, "model") {
			return false
		}
		for _, k := range []string{"not found", "not supported", "unsupported", "does not exist", "unknown model", "invalid model", "not served", "is not available"} {
			if strings.Contains(b, k) {
				return true
			}
		}
	}
	return false
}

// ListModels returns the model ids available to the configured key for the
// named provider. Network access happens only here (`loom models`).
func ListModels(ctx context.Context, name string) ([]string, error) {
	switch name {
	case "ollama":
		p := NewOllamaProvider("", "", false)
		return ListModelsAt(ctx, "ollama", p.BaseURL, "")
	case "openai":
		p := NewOpenAIProvider("", "", "", false)
		return ListModelsAt(ctx, "openai", p.BaseURL, p.APIKey)
	case "anthropic", "anthropic-api":
		p := NewAnthropicProvider("", "", "", false)
		return ListModelsAt(ctx, "anthropic", p.BaseURL, p.APIKey)
	case "gemini", "gemini-api":
		p := NewGeminiProvider("", "", "", false)
		return ListModelsAt(ctx, "gemini", p.BaseURL, p.APIKey)
	}
	return nil, fmt.Errorf("models listing is not supported for provider %q (supported: ollama, openai, anthropic, gemini)", name)
}

// ListModelsAt lists models from baseURL for kind ollama|openai|anthropic|gemini.
func ListModelsAt(ctx context.Context, kind, baseURL, apiKey string) ([]string, error) {
	baseURL = strings.TrimRight(baseURL, "/")
	headers := map[string]string{}
	var url string
	switch kind {
	case "ollama":
		url = baseURL + "/api/tags"
	case "openai":
		url = baseURL + "/v1/models"
		if apiKey != "" {
			headers["Authorization"] = "Bearer " + apiKey
		}
	case "anthropic":
		url = baseURL + "/v1/models?limit=1000"
		headers["anthropic-version"] = "2023-06-01"
		headers["x-api-key"] = apiKey
	case "gemini":
		url = baseURL + "/v1beta/models?pageSize=1000"
		headers["x-goog-api-key"] = apiKey
	default:
		return nil, fmt.Errorf("unsupported provider %q", kind)
	}
	if kind != "ollama" && strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("no API key configured for provider %s", kind)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("%s", RedactSecret(err.Error(), apiKey))
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: httpTimeout()}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s", RedactSecret(err.Error(), apiKey))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%s", RedactSecret(err.Error(), apiKey))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s", RedactSecret(fmt.Sprintf("HTTP %d from %s: %s", resp.StatusCode, url, strings.TrimSpace(string(body))), apiKey))
	}
	return parseModelList(kind, body)
}

func parseModelList(kind string, body []byte) ([]string, error) {
	var parsed struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("bad %s models response: %w", kind, err)
	}
	var ids []string
	switch kind {
	case "ollama":
		for _, m := range parsed.Models {
			ids = append(ids, m.Name)
		}
	case "gemini":
		for _, m := range parsed.Models {
			ids = append(ids, strings.TrimPrefix(m.Name, "models/"))
		}
	default:
		for _, m := range parsed.Data {
			ids = append(ids, m.ID)
		}
	}
	sort.Strings(ids)
	return ids, nil
}
