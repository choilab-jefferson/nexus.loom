package engine

import (
	"os"
	"path/filepath"
	"strings"
)

const maxAgentsMDBytes = 12000

// DiscoverAgentsMD walks up from workspace to the enclosing git root (inclusive) to find AGENTS.md
// and returns its path and contents (clamped to 12,000 bytes). It never reads above the git root;
// with no git root, only workspace itself is checked.
func DiscoverAgentsMD(workspace string) (string, string) {
	if strings.TrimSpace(workspace) == "" {
		workspace = "."
	}
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return "", ""
	}
	limit := abs
	for cur := abs; ; {
		if _, err := os.Stat(filepath.Join(cur, ".git")); err == nil {
			limit = cur
			break
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	cur := abs
	for {
		candidate := filepath.Join(cur, "AGENTS.md")
		data, err := os.ReadFile(candidate)
		if err == nil && len(data) > 0 {
			if len(data) > maxAgentsMDBytes {
				data = data[:maxAgentsMDBytes]
			}
			return candidate, string(data)
		}
		if cur == limit {
			break
		}
		cur = filepath.Dir(cur)
	}
	return "", ""
}
