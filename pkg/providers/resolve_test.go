package providers

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

var allKeyVars = []string{
	"ANTHROPIC_API_KEY", "NEXUS_ANTHROPIC_API_KEY",
	"GEMINI_API_KEY", "GOOGLE_API_KEY", "NEXUS_GEMINI_API_KEY",
	"OPENAI_API_KEY", "NEXUS_OPENAI_API_KEY", "LOOM_API_KEY",
	"LOOM_PROVIDER", "LOOM_MODEL",
}

// hermetic isolates config dir and clears every provider env var.
func hermetic(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("NEXUS_CONFIG_HOME", dir)
	for _, k := range allKeyVars {
		t.Setenv(k, "")
	}
	return dir
}

func writeTokens(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "tokens.env"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func up() bool   { return true }
func down() bool { return false }

func TestAutoDetectOrder(t *testing.T) {
	hermetic(t)
	t.Setenv("OPENAI_API_KEY", "o")
	if got, _ := ResolveProviderName("auto", down); got != "openai" {
		t.Fatalf("openai only: got %q", got)
	}
	t.Setenv("GOOGLE_API_KEY", "g")
	if got, _ := ResolveProviderName("auto", down); got != "gemini" {
		t.Fatalf("gemini beats openai: got %q", got)
	}
	t.Setenv("ANTHROPIC_API_KEY", "a")
	if got, _ := ResolveProviderName("", down); got != "anthropic" {
		t.Fatalf("anthropic beats gemini: got %q", got)
	}
	// keys beat reachable ollama
	if got, _ := ResolveProviderName("auto", up); got != "anthropic" {
		t.Fatalf("keys beat ollama: got %q", got)
	}
}

func TestAutoDetectFromTokensFile(t *testing.T) {
	dir := hermetic(t)
	writeTokens(t, dir, "NEXUS_GEMINI_API_KEY=file-g\n")
	if got, _ := ResolveProviderName("auto", down); got != "gemini" {
		t.Fatalf("got %q", got)
	}
}

func TestAutoFallsBackToOllamaOnlyIfReachable(t *testing.T) {
	hermetic(t)
	if got, err := ResolveProviderName("auto", up); err != nil || got != "ollama" {
		t.Fatalf("got %q err %v", got, err)
	}
	_, err := ResolveProviderName("auto", down)
	if err == nil {
		t.Fatal("expected error with nothing configured")
	}
	for _, want := range []string{"ANTHROPIC_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY", "OPENAI_API_KEY", "tokens.env"} {
		if !contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestLoomProviderEnvAndExplicitName(t *testing.T) {
	hermetic(t)
	t.Setenv("ANTHROPIC_API_KEY", "a")
	t.Setenv("LOOM_PROVIDER", "openai")
	if got, _ := ResolveProviderName("auto", down); got != "openai" {
		t.Fatalf("LOOM_PROVIDER should win over autodetect: %q", got)
	}
	if got, _ := ResolveProviderName("gemini", down); got != "gemini" {
		t.Fatalf("explicit flag should win: %q", got)
	}
}

func TestGoogleAPIKeyAcceptedForGemini(t *testing.T) {
	hermetic(t)
	t.Setenv("GOOGLE_API_KEY", "gk")
	if p := NewGeminiProvider("", "", "", false); p.APIKey != "gk" {
		t.Fatalf("GOOGLE_API_KEY not used: %q", p.APIKey)
	}
}

func TestEnvOverridesTokensFile(t *testing.T) {
	dir := hermetic(t)
	writeTokens(t, dir, "ANTHROPIC_API_KEY=file\nNEXUS_GEMINI_API_KEY=filegem\n")
	t.Setenv("ANTHROPIC_API_KEY", "env")
	if got := ResolveAPIKey("ANTHROPIC_API_KEY"); got != "env" {
		t.Fatalf("env must beat file, got %q", got)
	}
	// env under a later alias still beats file under an earlier alias
	t.Setenv("GEMINI_API_KEY", "envgem")
	if got := ResolveAPIKey("NEXUS_GEMINI_API_KEY", "GEMINI_API_KEY"); got != "envgem" {
		t.Fatalf("env alias must beat file alias, got %q", got)
	}
	if got := ResolveAPIKey("NEXUS_GEMINI_API_KEY"); got != "filegem" {
		t.Fatalf("file fallback broken, got %q", got)
	}
}

func TestDefaultOllamaModelByOS(t *testing.T) {
	if got := DefaultOllamaModel("darwin"); got != "qwen3.5:4b-mlx" {
		t.Fatalf("darwin: %q", got)
	}
	for _, os := range []string{"linux", "windows"} {
		if got := DefaultOllamaModel(os); got != "qwen3:4b" {
			t.Fatalf("%s: %q", os, got)
		}
	}
}

func TestLoomModelAppliesToEveryProvider(t *testing.T) {
	hermetic(t)
	t.Setenv("LOOM_MODEL", "custom-m")
	got := map[string]string{
		"ollama":    NewOllamaProvider("", "", false).Model,
		"openai":    NewOpenAIProvider("", "k", "", false).Model,
		"anthropic": NewAnthropicProvider("", "k", "", false).Model,
		"gemini":    NewGeminiProvider("", "k", "", false).Model,
		"gap":       NewGoogleAgentPlatformProvider("", "k", "", false).Model,
		"typesafe":  NewTypeSafeProvider("", "k", "", false).Model,
	}
	for n, m := range got {
		if m != "custom-m" {
			t.Errorf("%s ignored LOOM_MODEL: %q", n, m)
		}
	}
	if m := NewOpenAIProvider("", "k", "explicit", false).Model; m != "explicit" {
		t.Errorf("explicit --model must win: %q", m)
	}
}

func TestOllamaReachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			w.Write([]byte(`{"models":[]}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Setenv("OLLAMA_HOST", srv.URL)
	if !OllamaReachable() {
		t.Fatal("expected reachable")
	}
	srv.Close()
	if OllamaReachable() {
		t.Fatal("expected unreachable after close")
	}
}

func TestAutoPHISkipsCloudProviders(t *testing.T) {
	hermetic(t)
	t.Setenv("OPENAI_BASE_URL", "")
	t.Setenv("OLLAMA_HOST", "")
	t.Setenv("ANTHROPIC_API_KEY", "a")
	t.Setenv("GEMINI_API_KEY", "g")
	t.Setenv("OPENAI_API_KEY", "o")
	if got, err := ResolveProviderNamePHI("auto", up, true); err != nil || got != "ollama" {
		t.Fatalf("phi+keys+ollama: got %q, %v", got, err)
	}
	if _, err := ResolveProviderNamePHI("auto", down, true); err == nil {
		t.Fatal("phi with cloud keys and no ollama must error")
	}
	t.Setenv("OPENAI_BASE_URL", "http://127.0.0.1:8000")
	if got, _ := ResolveProviderNamePHI("auto", down, true); got != "openai" {
		t.Fatalf("phi with local openai endpoint: got %q", got)
	}
}

func TestAutoNonPHIUnchanged(t *testing.T) {
	hermetic(t)
	t.Setenv("ANTHROPIC_API_KEY", "a")
	if got, _ := ResolveProviderNamePHI("auto", up, false); got != "anthropic" {
		t.Fatalf("non-phi: got %q", got)
	}
	if got, _ := ResolveProviderName("auto", up); got != "anthropic" {
		t.Fatalf("wrapper: got %q", got)
	}
}

func TestKeyPrecedenceStandardThenNexusThenFile(t *testing.T) {
	dir := hermetic(t)
	writeTokens(t, dir, "ANTHROPIC_API_KEY=file-std\nNEXUS_ANTHROPIC_API_KEY=file-nx\nOPENAI_API_KEY=file-oa\n")
	if p := NewAnthropicProvider("", "", "", false); p.APIKey != "file-std" {
		t.Fatalf("file fallback (standard name first): %q", p.APIKey)
	}
	t.Setenv("NEXUS_ANTHROPIC_API_KEY", "env-nx")
	if p := NewAnthropicProvider("", "", "", false); p.APIKey != "env-nx" {
		t.Fatalf("NEXUS env must beat file: %q", p.APIKey)
	}
	t.Setenv("ANTHROPIC_API_KEY", "env-std")
	if p := NewAnthropicProvider("", "", "", false); p.APIKey != "env-std" {
		t.Fatalf("standard env must beat NEXUS env: %q", p.APIKey)
	}
	t.Setenv("NEXUS_OPENAI_API_KEY", "nx-oa")
	t.Setenv("OPENAI_API_KEY", "std-oa")
	if p := NewOpenAIProvider("", "", "", false); p.APIKey != "std-oa" {
		t.Fatalf("openai standard must win: %q", p.APIKey)
	}
}

func TestMissingTokenFileIsSilent(t *testing.T) {
	hermetic(t)
	if got := ResolveAPIKey("ANTHROPIC_API_KEY"); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}
