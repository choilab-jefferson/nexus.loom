package engine

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
	for _, k := range []string{"NEXUS_SENSITIVITY", "LOOM_NO_AGENTS_MD"} {
		os.Unsetenv(k)
	}
	code := m.Run()
	os.RemoveAll(tmp)
	os.Exit(code)
}
