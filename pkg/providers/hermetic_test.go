package providers

import (
	"os"
	"testing"
)

// TestMain isolates the suite from host config: temp HOME/NEXUS_CONFIG_HOME, no inherited Nexus env.
func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "loom-hermetic-")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", tmp)
	os.Setenv("NEXUS_CONFIG_HOME", tmp)
	// Never let a real host credential reach a test: the env wins over
	// the temp token store, so unset every provider key variable.
	for _, k := range []string{"NEXUS_SENSITIVITY", "LOOM_NO_AGENTS_MD",
		"OPENAI_API_KEY", "NEXUS_OPENAI_API_KEY", "LOOM_API_KEY",
		"ANTHROPIC_API_KEY", "NEXUS_ANTHROPIC_API_KEY",
		"GEMINI_API_KEY", "GOOGLE_API_KEY", "NEXUS_GEMINI_API_KEY",
		"NEXUS_GOOGLE_AGENT_API_KEY", "TYPESAFE_API_KEY", "OPENROUTER_API_KEY"} {
		os.Unsetenv(k)
	}
	code := m.Run()
	os.RemoveAll(tmp)
	os.Exit(code)
}
