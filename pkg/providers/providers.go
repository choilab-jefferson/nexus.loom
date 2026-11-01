package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/choilab-jefferson/nexus.loom/pkg/compact"
	"github.com/choilab-jefferson/nexus.loom/pkg/policy"
	"github.com/choilab-jefferson/nexus.loom/pkg/tools"
)

// DefaultMaxOutputTokens is the per-response output cap used when neither the
// task spec, --max-output-tokens nor LOOM_MAX_OUTPUT_TOKENS sets one.
const DefaultMaxOutputTokens = 8192

// StopReason represents the canonical Loom v0.1 Invariant I1 stop reason kinds.
type StopReason string

const (
	StopEndTurn   StopReason = "end_turn"
	StopToolUse   StopReason = "tool_use"
	StopMaxTokens StopReason = "max_tokens"
	StopError     StopReason = "error"
	StopUnknown   StopReason = "unknown"
)

// StopPaused is Anthropic pause_turn: the model yielded mid-turn; not a completion.
const StopPaused StopReason = "paused"

// RawStopKind maps a vendor raw stop string to a canonical kind without
// considering tool calls. Anthropic: refusal, pause_turn,
// model_context_window_exceeded. Gemini: MAX_TOKENS, MALFORMED_FUNCTION_CALL,
// RECITATION, OTHER, BLOCKLIST, PROHIBITED_CONTENT, SPII.
func RawStopKind(raw string) StopReason {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "stop", "end_turn", "eos", "stop_sequence", "completed", "finish":
		return StopEndTurn
	case "tool_calls", "tool_use", "function_call":
		return StopToolUse
	case "length", "max_tokens", "model_length", "model_context_window_exceeded":
		return StopMaxTokens
	case "pause_turn":
		return StopPaused
	case "error", "content_filter", "safety", "refusal",
		"malformed_function_call", "recitation", "other", "blocklist",
		"prohibited_content", "spii":
		return StopError
	default:
		return StopUnknown
	}
}

// IsTruncatedOrAborted reports whether a raw stop string means the response
// must not be acted on (tool calls in it may be partial or unsafe).
func IsTruncatedOrAborted(raw string) bool {
	switch RawStopKind(raw) {
	case StopMaxTokens, StopError, StopPaused:
		return true
	}
	return false
}

// NormalizeStopReason enforces Invariant I1: if len(toolCalls) > 0, StopToolUse wins
// regardless of the provider's raw stop string.
func NormalizeStopReason(raw string, toolCalls []compact.ToolCall) StopReason {
	if len(toolCalls) > 0 {
		return StopToolUse
	}
	return RawStopKind(raw)
}

// Response is the normalized model response returned by a Provider.
type Response struct {
	Content         string             `json:"content"`
	ToolCalls       []compact.ToolCall `json:"tool_calls,omitempty"`
	StopReason      StopReason         `json:"stop_reason"`
	RawStopReason   string             `json:"raw_stop_reason"`
	PromptTokens    int                `json:"prompt_tokens"`
	PromptEvalCount int                `json:"prompt_eval_count"`
	OutputTokens    int                `json:"output_tokens"`
	// Cache tokens (0 when the provider does not report them). Anthropic reports
	// them separately from PromptTokens; OpenAI/Gemini cached tokens are a subset
	// of PromptTokens.
	CacheCreationTokens int    `json:"cache_creation_tokens,omitempty"`
	CacheReadTokens     int    `json:"cache_read_tokens,omitempty"`
	Model               string `json:"model"`
	Provider            string `json:"provider"`
}

// Provider is the single-turn model inference interface for Loom v0.1.
type Provider interface {
	Chat(ctx context.Context, prefix compact.PrefixHeader, msgs []compact.Message, maxTokens int) (Response, error)
}

// ReadNexusTokenStore reads key=value pairs from $NEXUS_CONFIG_HOME/tokens.env
// (default ~/.config/nexus/tokens.env).
func ReadNexusTokenStore() map[string]string {
	return policy.ReadNexusTokenStore()
}

// ResolveAPIKey checks every candidate in os.Getenv() (in the order given: put
// standard names first, NEXUS_* aliases after), then the optional Nexus token
// file ~/.config/nexus/tokens.env, which is silently skipped when absent.
func ResolveAPIKey(envAndStoreKeys ...string) string {
	return policy.ResolveAPIKey(envAndStoreKeys...)
}

var (
	reGoogleKey = regexp.MustCompile(`AIza[0-9A-Za-z_\-]{20,}`)
	reSkKey     = regexp.MustCompile(`sk-[0-9A-Za-z_\-]{20,}`)
)

// RedactSecret scrubs secret (if non-empty) and common API key patterns from error strings.
func RedactSecret(msg, secret string) string {
	if secret != "" && len(secret) >= 6 {
		msg = strings.ReplaceAll(msg, secret, "<REDACTED>")
	}
	msg = reGoogleKey.ReplaceAllString(msg, "<REDACTED>")
	msg = reSkKey.ReplaceAllString(msg, "<REDACTED>")
	return msg
}

// IsLocalEndpoint verifies that rawURL resolves to a loopback or RFC1918 private host for PHI mode.
func IsLocalEndpoint(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate()
}

// OllamaProvider calls a local Ollama instance (`POST /api/chat`) with fixed num_ctx=32768 and keep_alive=-1.
type OllamaProvider struct {
	BaseURL string
	Model   string
	PHIMode bool
	Client  *http.Client
}

func NewOllamaProvider(baseURL, model string, phiMode bool) *OllamaProvider {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = os.Getenv("OLLAMA_HOST")
		if baseURL == "" {
			baseURL = "http://127.0.0.1:11434"
		}
	}
	if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		baseURL = "http://" + baseURL
	}
	if strings.TrimSpace(model) == "" {
		model = strings.TrimSpace(os.Getenv("LOOM_MODEL"))
		if model == "" {
			model = DefaultOllamaModel(runtime.GOOS)
		}
	}
	return &OllamaProvider{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Model:   model,
		PHIMode: phiMode,
		Client:  &http.Client{Timeout: httpTimeout()},
	}
}

func (p *OllamaProvider) Chat(ctx context.Context, prefix compact.PrefixHeader, msgs []compact.Message, maxTokens int) (Response, error) {
	if p.PHIMode && !IsLocalEndpoint(p.BaseURL) {
		return Response{}, &policy.ViolationError{
			Reason: fmt.Sprintf("PHI mode prohibits non-local Ollama endpoint %q", p.BaseURL),
		}
	}
	if maxTokens <= 0 {
		maxTokens = DefaultMaxOutputTokens
	}

	var wireMsgs []map[string]any
	wireMsgs = append(wireMsgs, map[string]any{
		"role":    "system",
		"content": prefix.RenderSystemBlock(),
	})
	for _, m := range msgs {
		entry := map[string]any{
			"role":    m.Role,
			"content": m.Content,
		}
		if len(m.ToolCalls) > 0 {
			var tcs []map[string]any
			for _, tc := range m.ToolCalls {
				tcs = append(tcs, map[string]any{
					"function": map[string]any{
						"name":      tc.Name,
						"arguments": tc.Arguments,
					},
				})
			}
			entry["tool_calls"] = tcs
		}
		wireMsgs = append(wireMsgs, entry)
	}

	var wireTools []map[string]any
	for _, s := range tools.BuiltinSchemas() {
		wireTools = append(wireTools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        s.Name,
				"description": s.Description,
				"parameters":  s.Parameters,
			},
		})
	}

	payload := map[string]any{
		"model":      p.Model,
		"messages":   wireMsgs,
		"tools":      wireTools,
		"stream":     false,
		"keep_alive": -1,
		"options": map[string]any{
			"num_ctx":     32768,
			"num_predict": maxTokens,
			"temperature": 0.0,
		},
	}

	body, err := doJSONPost(ctx, p.Client, p.BaseURL+"/api/chat", nil, payload, "")
	if err != nil {
		if strings.Contains(err.Error(), "connection refused") {
			return Response{}, fmt.Errorf("ollama not reachable at %s (start it with `ollama serve`, set OLLAMA_HOST, or pick another --provider such as anthropic or mock): %w", p.BaseURL, err)
		}
		return Response{}, tagModelErr(err, "ollama", p.Model)
	}

	var parsed struct {
		Model      string `json:"model"`
		DoneReason string `json:"done_reason"`
		Message    struct {
			Role      string `json:"role"`
			Content   string `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string         `json:"name"`
					Arguments map[string]any `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		PromptEvalCount int `json:"prompt_eval_count"`
		EvalCount       int `json:"eval_count"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return Response{}, fmt.Errorf("ollama response decode error: %w", err)
	}

	var tcs []compact.ToolCall
	for i, tc := range parsed.Message.ToolCalls {
		id := tc.ID
		if id == "" {
			id = fmt.Sprintf("call_%d", i+1)
		}
		tcs = append(tcs, compact.ToolCall{
			ID:        id,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}
	rawStop := parsed.DoneReason
	if rawStop == "" {
		rawStop = "stop"
	}
	return Response{
		Content:         parsed.Message.Content,
		ToolCalls:       tcs,
		StopReason:      NormalizeStopReason(rawStop, tcs),
		RawStopReason:   rawStop,
		PromptTokens:    compact.EstimateTotalTokens(prefix, msgs),
		PromptEvalCount: parsed.PromptEvalCount,
		OutputTokens:    parsed.EvalCount,
		Model:           p.Model,
		Provider:        "ollama",
	}, nil
}

// OpenAIProvider calls an OpenAI-compatible `/v1/chat/completions` endpoint.
type OpenAIProvider struct {
	BaseURL string
	APIKey  string
	Model   string
	PHIMode bool
	Client  *http.Client
}

func NewOpenAIProvider(baseURL, apiKey, model string, phiMode bool) *OpenAIProvider {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = os.Getenv("OPENAI_BASE_URL")
		if baseURL == "" {
			baseURL = "https://api.openai.com"
		}
	}
	if strings.TrimSpace(apiKey) == "" {
		apiKey = ResolveAPIKey("OPENAI_API_KEY", "NEXUS_OPENAI_API_KEY", "LOOM_API_KEY")
	}
	if strings.TrimSpace(model) == "" {
		model = envModelOr("gpt-4o-mini")
	}
	return &OpenAIProvider{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		Model:   model,
		PHIMode: phiMode,
		Client:  &http.Client{Timeout: httpTimeout()},
	}
}

func (p *OpenAIProvider) Chat(ctx context.Context, prefix compact.PrefixHeader, msgs []compact.Message, maxTokens int) (Response, error) {
	if p.PHIMode && !IsLocalEndpoint(p.BaseURL) {
		return Response{}, &policy.ViolationError{
			Reason: fmt.Sprintf("PHI mode prohibits non-local OpenAI endpoint %q", p.BaseURL),
		}
	}
	if maxTokens <= 0 {
		maxTokens = DefaultMaxOutputTokens
	}

	var wireMsgs []map[string]any
	wireMsgs = append(wireMsgs, map[string]any{
		"role":    "system",
		"content": prefix.RenderSystemBlock(),
	})
	for _, m := range msgs {
		entry := map[string]any{
			"role":    m.Role,
			"content": m.Content,
		}
		if m.Role == "tool" && m.ToolCallID != "" {
			entry["tool_call_id"] = m.ToolCallID
		}
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			var tcs []map[string]any
			for _, tc := range m.ToolCalls {
				argBytes, _ := json.Marshal(tc.Arguments)
				tcs = append(tcs, map[string]any{
					"id":   tc.ID,
					"type": "function",
					"function": map[string]any{
						"name":      tc.Name,
						"arguments": string(argBytes),
					},
				})
			}
			entry["tool_calls"] = tcs
		}
		wireMsgs = append(wireMsgs, entry)
	}

	var wireTools []map[string]any
	for _, s := range tools.BuiltinSchemas() {
		wireTools = append(wireTools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        s.Name,
				"description": s.Description,
				"parameters":  s.Parameters,
			},
		})
	}

	payload := map[string]any{
		"model":    p.Model,
		"messages": wireMsgs,
		"tools":    wireTools,
	}
	if isOpenAIReasoningModel(p.Model) {
		// Reasoning models reject max_tokens and any non-default temperature.
		payload["max_completion_tokens"] = maxTokens
	} else {
		payload["max_tokens"] = maxTokens
		payload["temperature"] = 0.0
	}
	headers := map[string]string{}
	if p.APIKey != "" {
		headers["Authorization"] = "Bearer " + p.APIKey
	}

	body, err := doJSONPost(ctx, p.Client, p.BaseURL+"/v1/chat/completions", headers, payload, p.APIKey)
	if err != nil {
		return Response{}, tagModelErr(err, "openai", p.Model)
	}

	var parsed struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens        int `json:"prompt_tokens"`
			OutTokens           int `json:"completion_tokens"`
			PromptTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return Response{}, fmt.Errorf("openai response decode error: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return Response{}, fmt.Errorf("openai response contained no choices")
	}
	choice := parsed.Choices[0]
	var tcs []compact.ToolCall
	for i, tc := range choice.Message.ToolCalls {
		var args map[string]any
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
		id := tc.ID
		if id == "" {
			id = fmt.Sprintf("call_%d", i+1)
		}
		tcs = append(tcs, compact.ToolCall{
			ID:        id,
			Name:      tc.Function.Name,
			Arguments: args,
		})
	}
	return Response{
		Content:         choice.Message.Content,
		ToolCalls:       tcs,
		StopReason:      NormalizeStopReason(choice.FinishReason, tcs),
		RawStopReason:   choice.FinishReason,
		PromptTokens:    parsed.Usage.PromptTokens,
		OutputTokens:    parsed.Usage.OutTokens,
		CacheReadTokens: parsed.Usage.PromptTokensDetails.CachedTokens,
		Model:           p.Model,
		Provider:        "openai",
	}, nil
}

// AnthropicProvider calls `POST https://api.anthropic.com/v1/messages` with ephemeral cache_control on the frozen prefix.
type AnthropicProvider struct {
	BaseURL string
	APIKey  string
	Model   string
	PHIMode bool
	Client  *http.Client
}

func NewAnthropicProvider(baseURL, apiKey, model string, phiMode bool) *AnthropicProvider {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = "https://api.anthropic.com"
	}
	if strings.TrimSpace(apiKey) == "" {
		apiKey = ResolveAPIKey("ANTHROPIC_API_KEY", "NEXUS_ANTHROPIC_API_KEY")
	}
	if strings.TrimSpace(model) == "" {
		model = envModelOr("claude-sonnet-5-5")
	}
	return &AnthropicProvider{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		Model:   model,
		PHIMode: phiMode,
		Client:  &http.Client{Timeout: httpTimeout()},
	}
}

func (p *AnthropicProvider) Chat(ctx context.Context, prefix compact.PrefixHeader, msgs []compact.Message, maxTokens int) (Response, error) {
	if p.PHIMode {
		return Response{}, &policy.ViolationError{
			Reason: "PHI mode prohibits cloud AnthropicProvider",
		}
	}
	if strings.TrimSpace(p.APIKey) == "" {
		return Response{}, fmt.Errorf("ANTHROPIC_API_KEY (or NEXUS_ANTHROPIC_API_KEY) is not set in the environment (or the optional Nexus token file ~/.config/nexus/tokens.env)")
	}
	if maxTokens <= 0 {
		maxTokens = DefaultMaxOutputTokens
	}

	systemBlocks := []map[string]any{
		{
			"type":          "text",
			"text":          prefix.RenderSystemBlock(),
			"cache_control": map[string]any{"type": "ephemeral"},
		},
	}

	var wireMsgs []map[string]any
	for _, m := range msgs {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			var blocks []map[string]any
			if strings.TrimSpace(m.Content) != "" {
				blocks = append(blocks, map[string]any{
					"type": "text",
					"text": m.Content,
				})
			}
			for _, tc := range m.ToolCalls {
				input := tc.Arguments
				if input == nil {
					input = map[string]any{}
				}
				blocks = append(blocks, map[string]any{
					"type":  "tool_use",
					"id":    tc.ID,
					"name":  tc.Name,
					"input": input,
				})
			}
			wireMsgs = append(wireMsgs, map[string]any{
				"role":    "assistant",
				"content": blocks,
			})
			continue
		}
		if m.Role == "tool" {
			trBlock := map[string]any{
				"type":        "tool_result",
				"tool_use_id": m.ToolCallID,
				"content":     m.Content,
			}
			// Merge consecutive tool results into a single user message per Anthropic wire format
			if len(wireMsgs) > 0 && wireMsgs[len(wireMsgs)-1]["role"] == "user" {
				if prevSlice, ok := wireMsgs[len(wireMsgs)-1]["content"].([]map[string]any); ok {
					wireMsgs[len(wireMsgs)-1]["content"] = append(prevSlice, trBlock)
					continue
				}
			}
			wireMsgs = append(wireMsgs, map[string]any{
				"role":    "user",
				"content": []map[string]any{trBlock},
			})
			continue
		}
		wireMsgs = append(wireMsgs, map[string]any{
			"role":    m.Role,
			"content": m.Content,
		})
	}

	var wireTools []map[string]any
	schemas := tools.BuiltinSchemas()
	for i, s := range schemas {
		t := map[string]any{
			"name":         s.Name,
			"description":  s.Description,
			"input_schema": s.Parameters,
		}
		if i == len(schemas)-1 {
			t["cache_control"] = map[string]any{"type": "ephemeral"}
		}
		wireTools = append(wireTools, t)
	}

	payload := map[string]any{
		"model":      p.Model,
		"max_tokens": maxTokens,
		"system":     systemBlocks,
		"messages":   wireMsgs,
		"tools":      wireTools,
	}
	headers := map[string]string{
		"anthropic-version": "2023-06-01",
		"x-api-key":         p.APIKey,
	}

	body, err := doJSONPost(ctx, p.Client, p.BaseURL+"/v1/messages", headers, payload, p.APIKey)
	if err != nil {
		return Response{}, tagModelErr(err, "anthropic", p.Model)
	}

	var parsed struct {
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string         `json:"type"`
			Text  string         `json:"text"`
			ID    string         `json:"id"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		} `json:"content"`
		Usage struct {
			InputTokens         int `json:"input_tokens"`
			OutTokens           int `json:"output_tokens"`
			CacheCreationTokens int `json:"cache_creation_input_tokens"`
			CacheReadTokens     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return Response{}, fmt.Errorf("anthropic response decode error: %w", err)
	}

	var textParts []string
	var tcs []compact.ToolCall
	for _, block := range parsed.Content {
		switch block.Type {
		case "text":
			textParts = append(textParts, block.Text)
		case "tool_use":
			tcs = append(tcs, compact.ToolCall{
				ID:        block.ID,
				Name:      block.Name,
				Arguments: block.Input,
			})
		}
	}
	return Response{
		Content:             strings.Join(textParts, "\n"),
		ToolCalls:           tcs,
		StopReason:          NormalizeStopReason(parsed.StopReason, tcs),
		RawStopReason:       parsed.StopReason,
		PromptTokens:        parsed.Usage.InputTokens,
		OutputTokens:        parsed.Usage.OutTokens,
		CacheCreationTokens: parsed.Usage.CacheCreationTokens,
		CacheReadTokens:     parsed.Usage.CacheReadTokens,
		Model:               p.Model,
		Provider:            "anthropic",
	}, nil
}

// GeminiProvider calls the Gemini `generateContent` endpoint with `x-goog-api-key` header and functionDeclarations.
type GeminiProvider struct {
	BaseURL string
	APIKey  string
	Model   string
	PHIMode bool
	Client  *http.Client
}

func NewGeminiProvider(baseURL, apiKey, model string, phiMode bool) *GeminiProvider {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = "https://generativelanguage.googleapis.com"
	}
	if strings.TrimSpace(apiKey) == "" {
		apiKey = ResolveAPIKey("GEMINI_API_KEY", "GOOGLE_API_KEY", "NEXUS_GEMINI_API_KEY")
	}
	if strings.TrimSpace(model) == "" {
		model = envModelOr("gemini-3.8-flash")
	}
	return &GeminiProvider{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		Model:   model,
		PHIMode: phiMode,
		Client:  &http.Client{Timeout: httpTimeout()},
	}
}

func (p *GeminiProvider) Chat(ctx context.Context, prefix compact.PrefixHeader, msgs []compact.Message, maxTokens int) (Response, error) {
	if p.PHIMode {
		return Response{}, &policy.ViolationError{
			Reason: "PHI mode prohibits cloud GeminiProvider",
		}
	}
	if strings.TrimSpace(p.APIKey) == "" {
		return Response{}, fmt.Errorf("GEMINI_API_KEY, GOOGLE_API_KEY or NEXUS_GEMINI_API_KEY is not set in the environment (or the optional Nexus token file ~/.config/nexus/tokens.env)")
	}
	cleanModel := strings.TrimPrefix(p.Model, "models/")
	endpoint := fmt.Sprintf("%s/v1beta/models/%s:generateContent", p.BaseURL, cleanModel)
	return doGeminiGenerateContent(ctx, p.Client, endpoint, p.APIKey, cleanModel, "gemini", prefix, msgs, maxTokens)
}

// GoogleAgentPlatformProvider calls Vertex AI / Google Agent Platform (`publishers/google/models/...:generateContent`).
type GoogleAgentPlatformProvider struct {
	BaseURL  string
	APIKey   string
	Project  string
	Location string
	Model    string
	PHIMode  bool
	Client   *http.Client
}

func NewGoogleAgentPlatformProvider(baseURL, apiKey, model string, phiMode bool) *GoogleAgentPlatformProvider {
	if strings.TrimSpace(apiKey) == "" {
		apiKey = ResolveAPIKey("NEXUS_GOOGLE_AGENT_API_KEY")
	}
	if strings.TrimSpace(model) == "" {
		model = envModelOr("gemini-3.8-flash")
	}
	proj, loc := resolveGAPProjectAndLocation()
	return &GoogleAgentPlatformProvider{
		BaseURL:  strings.TrimRight(baseURL, "/"),
		APIKey:   apiKey,
		Project:  proj,
		Location: loc,
		Model:    model,
		PHIMode:  phiMode,
		Client:   &http.Client{Timeout: httpTimeout()},
	}
}

func resolveGAPProjectAndLocation() (string, string) {
	proj := strings.TrimSpace(os.Getenv("NEXUS_GCP_PROJECT"))
	loc := strings.TrimSpace(os.Getenv("NEXUS_GCP_LOCATION"))

	cfgPath := strings.TrimSpace(os.Getenv("NEXUS_GAP_CREDIT_CONFIG"))
	if cfgPath == "" {
		cfgDir := strings.TrimSpace(os.Getenv("NEXUS_CONFIG_HOME"))
		if cfgDir == "" {
			if home, err := os.UserHomeDir(); err == nil && home != "" {
				cfgDir = filepath.Join(home, ".config", "nexus")
			}
		}
		if cfgDir != "" {
			cfgPath = filepath.Join(cfgDir, "google-agent-platform-credit.json")
		}
	}
	if cfgPath != "" {
		if data, err := os.ReadFile(cfgPath); err == nil {
			var parsed struct {
				Project   string `json:"project"`
				ProjectID string `json:"project_id"`
				Location  string `json:"location"`
			}
			if json.Unmarshal(data, &parsed) == nil {
				if proj == "" {
					if parsed.Project != "" {
						proj = parsed.Project
					} else {
						proj = parsed.ProjectID
					}
				}
				if loc == "" && parsed.Location != "" {
					loc = parsed.Location
				}
			}
		}
	}
	if loc == "" {
		loc = "global"
	}
	return proj, loc
}

// EndpointURL builds the Google Agent Platform generateContent URL.
func (p *GoogleAgentPlatformProvider) EndpointURL() (string, error) {
	proj := p.Project
	loc := p.Location
	if proj == "" || loc == "" {
		rProj, rLoc := resolveGAPProjectAndLocation()
		if proj == "" {
			proj = rProj
		}
		if loc == "" {
			loc = rLoc
		}
	}
	if proj == "" {
		return "", fmt.Errorf("Google Agent Platform project is not configured: set NEXUS_GCP_PROJECT (and optionally NEXUS_GCP_LOCATION), or provide a JSON file {project, location} via NEXUS_GAP_CREDIT_CONFIG or $NEXUS_CONFIG_HOME/google-agent-platform-credit.json")
	}
	cleanModel := strings.TrimPrefix(p.Model, "models/")
	if p.BaseURL != "" {
		return fmt.Sprintf("%s/v1/projects/%s/locations/%s/publishers/google/models/%s:generateContent",
			p.BaseURL, proj, loc, cleanModel), nil
	}
	var host string
	switch loc {
	case "global":
		host = "aiplatform.googleapis.com"
	case "us", "eu":
		host = fmt.Sprintf("aiplatform.%s.rep.googleapis.com", loc)
	default:
		host = fmt.Sprintf("%s-aiplatform.googleapis.com", loc)
	}
	return fmt.Sprintf("https://%s/v1/projects/%s/locations/%s/publishers/google/models/%s:generateContent",
		host, proj, loc, cleanModel), nil
}

func (p *GoogleAgentPlatformProvider) Chat(ctx context.Context, prefix compact.PrefixHeader, msgs []compact.Message, maxTokens int) (Response, error) {
	if p.PHIMode {
		return Response{}, &policy.ViolationError{
			Reason: "PHI mode prohibits cloud GoogleAgentPlatformProvider",
		}
	}
	if strings.TrimSpace(p.APIKey) == "" {
		return Response{}, fmt.Errorf("NEXUS_GOOGLE_AGENT_API_KEY is not set in the environment (or the optional Nexus token file ~/.config/nexus/tokens.env)")
	}
	endpoint, err := p.EndpointURL()
	if err != nil {
		return Response{}, err
	}
	cleanModel := strings.TrimPrefix(p.Model, "models/")
	return doGeminiGenerateContent(ctx, p.Client, endpoint, p.APIKey, cleanModel, "google-agent-platform", prefix, msgs, maxTokens)
}

func doGeminiGenerateContent(
	ctx context.Context,
	client *http.Client,
	endpoint, apiKey, model, providerName string,
	prefix compact.PrefixHeader,
	msgs []compact.Message,
	maxTokens int,
) (Response, error) {
	if maxTokens <= 0 {
		maxTokens = DefaultMaxOutputTokens
	}

	var contents []map[string]any
	for _, m := range msgs {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			var parts []map[string]any
			if strings.TrimSpace(m.Content) != "" {
				parts = append(parts, map[string]any{"text": m.Content})
			}
			for _, tc := range m.ToolCalls {
				args := tc.Arguments
				if args == nil {
					args = map[string]any{}
				}
				part := map[string]any{
					"functionCall": map[string]any{
						"name": tc.Name,
						"args": args,
					},
				}
				if tc.ThoughtSignature != "" {
					part["thoughtSignature"] = tc.ThoughtSignature
				}
				parts = append(parts, part)
			}
			contents = append(contents, map[string]any{
				"role":  "model",
				"parts": parts,
			})
			continue
		}
		if m.Role == "tool" {
			tName := m.ToolName
			if tName == "" {
				tName = m.Name
			}
			frPart := map[string]any{
				"functionResponse": map[string]any{
					"name": tName,
					"response": map[string]any{
						"result": m.Content,
					},
				},
			}
			if len(contents) > 0 && contents[len(contents)-1]["role"] == "user" {
				if prevParts, ok := contents[len(contents)-1]["parts"].([]map[string]any); ok && len(prevParts) > 0 {
					if _, isFR := prevParts[0]["functionResponse"]; isFR {
						contents[len(contents)-1]["parts"] = append(prevParts, frPart)
						continue
					}
				}
			}
			contents = append(contents, map[string]any{
				"role":  "user",
				"parts": []map[string]any{frPart},
			})
			continue
		}
		role := "user"
		if m.Role == "assistant" {
			role = "model"
		}
		contents = append(contents, map[string]any{
			"role": role,
			"parts": []map[string]any{
				{"text": m.Content},
			},
		})
	}

	var fnDecls []map[string]any
	for _, s := range tools.BuiltinSchemas() {
		fnDecls = append(fnDecls, map[string]any{
			"name":        s.Name,
			"description": s.Description,
			"parameters":  s.Parameters,
		})
	}

	genCfg := map[string]any{
		"maxOutputTokens": maxTokens,
		"temperature":     0.0,
	}
	if strings.HasPrefix(model, "gemini-4") {
		genCfg["thinkingConfig"] = map[string]any{
			"thinkingLevel": "low",
		}
	}

	payload := map[string]any{
		"systemInstruction": map[string]any{
			"parts": []map[string]any{
				{"text": prefix.RenderSystemBlock()},
			},
		},
		"contents": contents,
		"tools": []map[string]any{
			{"functionDeclarations": fnDecls},
		},
		"generationConfig": genCfg,
	}

	headers := map[string]string{
		"x-goog-api-key": apiKey,
	}
	body, err := doJSONPost(ctx, client, endpoint, headers, payload, apiKey)
	if err != nil {
		return Response{}, tagModelErr(err, providerName, model)
	}

	var parsed struct {
		Candidates []struct {
			FinishReason string `json:"finishReason"`
			Content      struct {
				Parts []struct {
					Text             string `json:"text"`
					Thought          bool   `json:"thought"`
					ThoughtSignature string `json:"thoughtSignature"`
					FunctionCall     *struct {
						Name string         `json:"name"`
						Args map[string]any `json:"args"`
					} `json:"functionCall"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
		UsageMetadata struct {
			PromptTokenCount int `json:"promptTokenCount"`
			CandTokens       int `json:"candidatesTokenCount"`
			CachedTokens     int `json:"cachedContentTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return Response{}, fmt.Errorf("%s response decode error: %w", providerName, err)
	}
	if len(parsed.Candidates) == 0 {
		return Response{}, fmt.Errorf("%s response contained no candidates", providerName)
	}

	cand := parsed.Candidates[0]
	var texts []string
	var tcs []compact.ToolCall
	for i, part := range cand.Content.Parts {
		if part.Thought {
			continue
		}
		if part.Text != "" {
			texts = append(texts, part.Text)
		}
		if part.FunctionCall != nil {
			tcs = append(tcs, compact.ToolCall{
				ID:               fmt.Sprintf("call_%d", i+1),
				Name:             part.FunctionCall.Name,
				Arguments:        part.FunctionCall.Args,
				ThoughtSignature: part.ThoughtSignature,
			})
		}
	}
	promptToks := parsed.UsageMetadata.PromptTokenCount
	if promptToks <= 0 {
		promptToks = compact.EstimateTotalTokens(prefix, msgs)
	}
	return Response{
		Content:         strings.Join(texts, "\n"),
		ToolCalls:       tcs,
		StopReason:      NormalizeStopReason(cand.FinishReason, tcs),
		RawStopReason:   cand.FinishReason,
		PromptTokens:    promptToks,
		OutputTokens:    parsed.UsageMetadata.CandTokens,
		CacheReadTokens: parsed.UsageMetadata.CachedTokens,
		Model:           model,
		Provider:        providerName,
	}, nil
}

// TypeSafeProvider calls `POST /v1/systemone` (`typesafe` / `jev`) using TYPESAFE_API_KEY.
type TypeSafeProvider struct {
	BaseURL string
	APIKey  string
	Model   string
	PHIMode bool
	Client  *http.Client
}

func NewTypeSafeProvider(baseURL, apiKey, model string, phiMode bool) *TypeSafeProvider {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = "https://api.typesafe.ai"
	}
	if strings.TrimSpace(apiKey) == "" {
		apiKey = ResolveAPIKey("TYPESAFE_API_KEY")
	}
	if strings.TrimSpace(model) == "" {
		model = envModelOr("jev-latest")
	}
	return &TypeSafeProvider{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		Model:   model,
		PHIMode: phiMode,
		Client:  &http.Client{Timeout: httpTimeout()},
	}
}

func (p *TypeSafeProvider) Chat(ctx context.Context, prefix compact.PrefixHeader, msgs []compact.Message, _ int) (Response, error) {
	if p.PHIMode {
		return Response{}, &policy.ViolationError{
			Reason: "PHI mode prohibits cloud TypeSafeProvider",
		}
	}
	if strings.TrimSpace(p.APIKey) == "" {
		return Response{}, fmt.Errorf("TYPESAFE_API_KEY is not set in the environment (or the optional Nexus token file ~/.config/nexus/tokens.env)")
	}

	stateParts := []string{prefix.RenderSystemBlock()}
	latestInstruction := ""
	for _, m := range msgs {
		stateParts = append(stateParts, fmt.Sprintf("[%s]\n%s", m.Role, m.Content))
		if m.Role == "user" && strings.TrimSpace(m.Content) != "" {
			latestInstruction = strings.TrimSpace(m.Content)
		}
	}

	questions := map[string]any{
		"q1": map[string]any{
			"type":         "noul",
			"instructions": latestInstruction,
		},
	}
	if strings.HasPrefix(latestInstruction, "{") {
		var parsedQ map[string]any
		if json.Unmarshal([]byte(latestInstruction), &parsedQ) == nil && len(parsedQ) > 0 {
			if _, hasType := parsedQ["type"]; hasType {
				questions = map[string]any{"q1": parsedQ}
			} else {
				questions = parsedQ
			}
		}
	}

	payload := map[string]any{
		"state": map[string]any{
			"context": strings.Join(stateParts, "\n\n"),
		},
		"model":     p.Model,
		"questions": questions,
	}
	headers := map[string]string{
		"Authorization": "Bearer " + p.APIKey,
	}

	body, err := doJSONPost(ctx, p.Client, p.BaseURL+"/v1/systemone", headers, payload, p.APIKey)
	if err != nil {
		return Response{}, err
	}

	var parsed struct {
		Answers any `json:"answers"`
		Usage   struct {
			InputTokens int `json:"input_tokens"`
			OutTokens   int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return Response{}, fmt.Errorf("typesafe response decode error: %w", err)
	}
	ansBytes, _ := json.Marshal(parsed.Answers)
	promptToks := parsed.Usage.InputTokens
	if promptToks <= 0 {
		promptToks = compact.EstimateTotalTokens(prefix, msgs)
	}
	return Response{
		Content:       string(ansBytes),
		StopReason:    StopEndTurn,
		RawStopReason: "stop",
		PromptTokens:  promptToks,
		OutputTokens:  parsed.Usage.OutTokens,
		Model:         p.Model,
		Provider:      "typesafe",
	}, nil
}

// ScriptedMockProvider implements the deterministic replay transport for unit and CI gate tests.
type ScriptedMockProvider struct {
	mu           sync.Mutex
	Responses    []Response
	RecordedHash []string
	RecordedMsgs [][]compact.Message
	idx          int
}

func NewScriptedMockProvider(responses ...Response) *ScriptedMockProvider {
	return &ScriptedMockProvider{
		Responses: responses,
	}
}

func (m *ScriptedMockProvider) Chat(_ context.Context, prefix compact.PrefixHeader, msgs []compact.Message, _ int) (Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.RecordedHash = append(m.RecordedHash, prefix.PrefixHash())
	cloned := make([]compact.Message, len(msgs))
	copy(cloned, msgs)
	m.RecordedMsgs = append(m.RecordedMsgs, cloned)

	if m.idx >= len(m.Responses) {
		return Response{
			Content:       "done",
			StopReason:    StopEndTurn,
			RawStopReason: "stop",
			PromptTokens:  compact.EstimateTotalTokens(prefix, msgs),
			Model:         "mock-model",
			Provider:      "mock",
		}, nil
	}
	resp := m.Responses[m.idx]
	m.idx++
	if resp.StopReason == "" {
		resp.StopReason = NormalizeStopReason(resp.RawStopReason, resp.ToolCalls)
	}
	if resp.PromptTokens == 0 {
		resp.PromptTokens = compact.EstimateTotalTokens(prefix, msgs)
	}
	if resp.Provider == "" {
		resp.Provider = "mock"
	}
	if resp.Model == "" {
		resp.Model = "mock-model"
	}
	return resp, nil
}

// Resilience knobs (vars so tests can shrink them).
var (
	maxResponseBytes int64 = 32 << 20
	maxRetries             = 3
	retryBaseDelay         = 500 * time.Millisecond
	maxRetryDelay          = 60 * time.Second
)

// httpTimeout returns the per-request HTTP timeout: LOOM_HTTP_TIMEOUT
// (Go duration like "90s", or bare seconds), default 120s.
func httpTimeout() time.Duration {
	v := strings.TrimSpace(os.Getenv("LOOM_HTTP_TIMEOUT"))
	if v == "" {
		return 120 * time.Second
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return 120 * time.Second
}

// isOpenAIReasoningModel reports o-series and gpt-5 models, which take
// max_completion_tokens and only the default temperature.
func isOpenAIReasoningModel(model string) bool {
	m := strings.ToLower(model)
	if strings.HasPrefix(m, "gpt-5") {
		return true
	}
	return len(m) >= 2 && m[0] == 'o' && m[1] >= '0' && m[1] <= '9'
}

func retryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code >= 500
}

// retryDelay honors Retry-After (seconds or HTTP date), else exponential backoff.
func retryDelay(h http.Header, attempt int) time.Duration {
	d := retryBaseDelay << attempt
	if ra := strings.TrimSpace(h.Get("Retry-After")); ra != "" {
		if n, err := strconv.Atoi(ra); err == nil && n >= 0 {
			d = time.Duration(n) * time.Second
		} else if t, err := http.ParseTime(ra); err == nil {
			d = time.Until(t)
		}
	}
	if d < 0 {
		d = 0
	}
	if d > maxRetryDelay {
		d = maxRetryDelay
	}
	return d
}

func doJSONPost(ctx context.Context, client *http.Client, endpoint string, headers map[string]string, payload any, secret string) ([]byte, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("%s", RedactSecret(err.Error(), secret))
		}
		req.Header.Set("Content-Type", "application/json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("%s", RedactSecret(err.Error(), secret))
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("%s", RedactSecret(err.Error(), secret))
		}
		if int64(len(body)) > maxResponseBytes {
			return nil, fmt.Errorf("response from %s exceeds %d bytes", endpoint, maxResponseBytes)
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return body, nil
		}
		if retryableStatus(resp.StatusCode) && attempt < maxRetries {
			t := time.NewTimer(retryDelay(resp.Header, attempt))
			select {
			case <-ctx.Done():
				t.Stop()
				return nil, ctx.Err()
			case <-t.C:
			}
			continue
		}
		msg := fmt.Sprintf("HTTP %d from %s: %s", resp.StatusCode, endpoint, strings.TrimSpace(string(body)))
		msg = RedactSecret(msg, secret)
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, &AuthError{Status: resp.StatusCode, Msg: msg}
		}
		if isModelNotFound(resp.StatusCode, body) {
			return nil, &ModelNotFoundError{Status: resp.StatusCode, Detail: msg}
		}
		return nil, fmt.Errorf("%s", msg)
	}
}

// AuthError is an operator-side credential failure (HTTP 401/403); the model
// cannot fix it, so the engine maps it to exit 2 rather than a task failure.
type AuthError struct {
	Status int
	Msg    string
}

func (e *AuthError) Error() string { return e.Msg }

// IsAuthError reports whether err is or wraps an *AuthError.
func IsAuthError(err error) bool {
	var a *AuthError
	return errors.As(err, &a)
}

// DefaultOllamaModel returns the default local model tag for goos. The MLX
// tag only exists on Apple platforms; elsewhere a portable tag is used.
func DefaultOllamaModel(goos string) string {
	if goos == "darwin" {
		return "qwen3.5:4b-mlx"
	}
	return "qwen3:4b"
}

// envModelOr returns LOOM_MODEL when set, else def.
func envModelOr(def string) string {
	if m := strings.TrimSpace(os.Getenv("LOOM_MODEL")); m != "" {
		return m
	}
	return def
}

// OllamaReachable reports whether an Ollama server answers on OLLAMA_HOST
// (default 127.0.0.1:11434) within a short timeout.
func OllamaReachable() bool {
	base := strings.TrimSpace(os.Getenv("OLLAMA_HOST"))
	if base == "" {
		base = "http://127.0.0.1:11434"
	}
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "http://" + base
	}
	c := &http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := c.Get(strings.TrimRight(base, "/") + "/api/tags")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// AutoProviderKeys is the auto-detect order: first provider with a key wins.
var AutoProviderKeys = []struct {
	Provider string
	Keys     []string
}{
	{"anthropic", []string{"ANTHROPIC_API_KEY", "NEXUS_ANTHROPIC_API_KEY"}},
	{"gemini", []string{"GEMINI_API_KEY", "GOOGLE_API_KEY", "NEXUS_GEMINI_API_KEY"}},
	{"openai", []string{"OPENAI_API_KEY", "NEXUS_OPENAI_API_KEY", "LOOM_API_KEY"}},
}

// PHIPermitted reports whether a provider may run under PHI mode, reusing the
// same endpoint check (IsLocalEndpoint) the providers enforce in Chat. Cloud-only
// providers (anthropic, gemini, ...) are never permitted.
func PHIPermitted(provider string) bool {
	switch provider {
	case "ollama":
		return IsLocalEndpoint(NewOllamaProvider("", "", true).BaseURL)
	case "openai":
		return IsLocalEndpoint(NewOpenAIProvider("", "", "", true).BaseURL)
	}
	return false
}

// ResolveProviderName maps "", "auto" or LOOM_PROVIDER to a concrete provider.
// An explicit non-auto name is returned unchanged. ollamaUp is injectable.
func ResolveProviderName(name string, ollamaUp func() bool) (string, error) {
	return ResolveProviderNamePHI(name, ollamaUp, false)
}

// ResolveProviderNamePHI is ResolveProviderName; when phi is true, auto
// selection skips providers that PHIPermitted rejects.
func ResolveProviderNamePHI(name string, ollamaUp func() bool, phi bool) (string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" || name == "auto" {
		name = strings.ToLower(strings.TrimSpace(os.Getenv("LOOM_PROVIDER")))
	}
	if name != "" && name != "auto" {
		return name, nil
	}
	for _, e := range AutoProviderKeys {
		if ResolveAPIKey(e.Keys...) != "" && (!phi || PHIPermitted(e.Provider)) {
			return e.Provider, nil
		}
	}
	if ollamaUp != nil && ollamaUp() && (!phi || PHIPermitted("ollama")) {
		return "ollama", nil
	}
	return "", fmt.Errorf("no model provider configured: set one of ANTHROPIC_API_KEY, GEMINI_API_KEY (or GOOGLE_API_KEY), OPENAI_API_KEY in the environment " +
		"(or the optional Nexus token file ~/.config/nexus/tokens.env), start Ollama, or pass --provider/LOOM_PROVIDER")
}
