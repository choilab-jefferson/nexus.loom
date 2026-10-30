package compact

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
	os.Setenv("NEXUS_STM_DIR", tmp)
	for _, k := range []string{"NEXUS_PROJECT_ID", "NEXUS_SENSITIVITY", "NEXUS_STM_INJECT", "NEXUS_NO_AGENTS_MD"} {
		os.Unsetenv(k)
	}
	code := m.Run()
	os.RemoveAll(tmp)
	os.Exit(code)
}
