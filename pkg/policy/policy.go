package policy

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	ExitOK         = 0
	ExitTaskFail   = 1
	ExitHarnessErr = 2
	ExitEscalate   = 3
	// ExitBudget: token/deadline/tool-call budget exhausted (distinct from task failure).
	ExitBudget = 4
	// ExitInterrupted: SIGINT/SIGTERM (128+SIGINT).
	ExitInterrupted = 130
)

// ViolationError represents an Invariant I4 or harness policy violation (Exit Code 2).
type ViolationError struct {
	Reason string
}

func (e *ViolationError) Error() string {
	return fmt.Sprintf("policy violation: %s", e.Reason)
}

func (e *ViolationError) ExitCode() int {
	return ExitHarnessErr
}

// IsViolation reports whether err is or wraps a *ViolationError.
func IsViolation(err error) bool {
	var v *ViolationError
	return errors.As(err, &v)
}

// PolicyConfig defines the capability and workspace scope for a Loom leaf run (Invariant I4).
type PolicyConfig struct {
	Workspace      string
	ReadAllow      []string
	WriteAllow     []string
	DenyGlobs      []string
	AllowCommands  [][]string
	MaxReadLines   int
	MaxOutputBytes int
	PHIMode        bool
}

// DefaultPolicyConfig returns a baseline policy rooted at workspace.
func DefaultPolicyConfig(workspace string) *PolicyConfig {
	return &PolicyConfig{
		Workspace:      workspace,
		ReadAllow:      []string{"**"},
		WriteAllow:     []string{"**"},
		DenyGlobs:      []string{".git", ".git/**", "**/.git/**", ".env", ".env*", "**/.env*"},
		AllowCommands:  nil,
		MaxReadLines:   400,
		MaxOutputBytes: 16384,
		PHIMode:        false,
	}
}

var forbiddenWrappers = map[string]bool{
	"sh":   true,
	"bash": true,
	"zsh":  true,
	"dash": true,
	"fish": true,
	"eval": true,
	"exec": true,
	"make": true,
	"npm":  true,
	"npx":  true,
	"uvx":  true,
	"sudo": true,
	"su":   true,
}

// IntersectAllowCommands computes effective_allowlist = global ∩ agent ∩ task.
// For any two prefix rules A and B, if A is a token-prefix of B, the narrower rule B
// is retained. No layer can widen permissions beyond any other layer.
func IntersectAllowCommands(global, agent, task [][]string) [][]string {
	ab := intersectTwo(global, agent)
	return intersectTwo(ab, task)
}

func intersectTwo(a, b [][]string) [][]string {
	if len(a) == 0 || len(b) == 0 {
		return [][]string{}
	}
	var out [][]string
	seen := make(map[string]bool)
	for _, pa := range a {
		if len(pa) == 0 {
			continue
		}
		for _, pb := range b {
			if len(pb) == 0 {
				continue
			}
			var narrower []string
			if isTokenPrefix(pa, pb) {
				narrower = cloneSlice(pb)
			} else if isTokenPrefix(pb, pa) {
				narrower = cloneSlice(pa)
			}
			if narrower != nil {
				key := strings.Join(narrower, "\x00")
				if !seen[key] {
					seen[key] = true
					out = append(out, narrower)
				}
			}
		}
	}
	return out
}

func isTokenPrefix(prefix, full []string) bool {
	if len(prefix) > len(full) {
		return false
	}
	for i := range prefix {
		if i == 0 {
			if !matchArgv0(prefix[0], full[0]) {
				return false
			}
			continue
		}
		if prefix[i] != full[i] {
			return false
		}
	}
	return true
}

func matchArgv0(allowed, candidate string) bool {
	if allowed == candidate {
		return true
	}
	// If allowed is a bare command name (no slash), match exact basename of candidate.
	if !strings.ContainsRune(allowed, '/') && !strings.ContainsRune(allowed, filepath.Separator) {
		return filepath.Base(candidate) == allowed
	}
	return false
}

func cloneSlice(s []string) []string {
	c := make([]string, len(s))
	copy(c, s)
	return c
}

// ResolveScopedPath resolves target inside workspace, evaluating symlinks on existing
// path components, and checks DenyGlobs and ReadAllow/WriteAllow.
func ResolveScopedPath(workspace, target string, write bool, cfg *PolicyConfig) (string, error) {
	if strings.TrimSpace(workspace) == "" {
		return "", &ViolationError{Reason: "workspace root is empty"}
	}
	if strings.TrimSpace(target) == "" {
		// Model-caused argument error: recoverable tool error, not a policy violation.
		return "", fmt.Errorf("target path is empty")
	}

	wsAbs, err := filepath.Abs(workspace)
	if err != nil {
		return "", &ViolationError{Reason: fmt.Sprintf("invalid workspace path: %v", err)}
	}
	wsReal, err := evalSymlinksWalk(wsAbs)
	if err != nil {
		return "", &ViolationError{Reason: fmt.Sprintf("cannot resolve workspace symlinks: %v", err)}
	}

	var candidate string
	if filepath.IsAbs(target) {
		candidate = filepath.Clean(target)
	} else {
		candidate = filepath.Join(wsReal, target)
	}

	if write {
		if fi, lerr := os.Lstat(candidate); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
			return "", &ViolationError{Reason: fmt.Sprintf("refusing to write through symlink %q", target)}
		}
	}

	resolved, err := evalSymlinksWalk(candidate)
	if err != nil {
		// Unresolvable model-supplied path (ENOTDIR, dangling symlink): recoverable.
		return "", fmt.Errorf("cannot resolve target path %q: %v", target, err)
	}

	rel, err := filepath.Rel(wsReal, resolved)
	if err != nil {
		return "", &ViolationError{Reason: fmt.Sprintf("cannot compute relative path: %v", err)}
	}
	relSlash := filepath.ToSlash(rel)
	if relSlash == ".." || strings.HasPrefix(relSlash, "../") || filepath.IsAbs(rel) {
		return "", &ViolationError{Reason: fmt.Sprintf("path %q escapes workspace root", target)}
	}

	var denyGlobs []string
	var allowGlobs []string
	if cfg != nil {
		denyGlobs = cfg.DenyGlobs
		if write {
			allowGlobs = cfg.WriteAllow
		} else {
			allowGlobs = cfg.ReadAllow
		}
	} else {
		denyGlobs = []string{".git", ".git/**", "**/.git/**", ".env", ".env*", "**/.env*"}
	}

	if relSlash != "." {
		for _, g := range denyGlobs {
			if matchGlob(g, relSlash) {
				return "", &ViolationError{Reason: fmt.Sprintf("path %q matches deny pattern %q", relSlash, g)}
			}
		}
		if len(allowGlobs) > 0 {
			matched := false
			for _, g := range allowGlobs {
				if matchGlob(g, relSlash) {
					matched = true
					break
				}
			}
			if !matched {
				mode := "read"
				if write {
					mode = "write"
				}
				return "", &ViolationError{Reason: fmt.Sprintf("path %q not permitted for %s by allowlist", relSlash, mode)}
			}
		}
	}

	return resolved, nil
}

// evalSymlinksWalk resolves symlinks for p even if the leaf or intermediate directories do not yet exist.
func evalSymlinksWalk(p string) (string, error) {
	clean := filepath.Clean(p)
	resolved, err := filepath.EvalSymlinks(clean)
	if err == nil {
		return filepath.Clean(resolved), nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	// A path that fails to resolve yet exists under Lstat is a dangling symlink;
	// joining its name onto the parent would hide where it really points.
	if fi, lerr := os.Lstat(clean); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("dangling symlink %q", clean)
	}
	parent := filepath.Dir(clean)
	if parent == clean {
		return clean, nil
	}
	resolvedParent, err := evalSymlinksWalk(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolvedParent, filepath.Base(clean)), nil
}

// matchGlob matches a slash-normalized relative path against a glob pattern supporting "**".
func matchGlob(pattern, relSlash string) bool {
	pattern = strings.TrimSpace(filepath.ToSlash(pattern))
	relSlash = strings.TrimPrefix(filepath.ToSlash(relSlash), "./")
	if pattern == "" {
		return false
	}
	if pattern == "**" || pattern == "**/*" {
		return true
	}

	// Check component-level deny rules such as ".git/**" or "**/.git/**" or ".env*"
	parts := strings.Split(relSlash, "/")
	if pattern == ".git/**" || pattern == "**/.git/**" || pattern == ".git" {
		for _, part := range parts {
			if part == ".git" {
				return true
			}
		}
	}
	if pattern == ".env*" || pattern == "**/.env*" {
		for _, part := range parts {
			if part == ".env" || strings.HasPrefix(part, ".env.") || strings.HasPrefix(part, ".env_") || strings.HasPrefix(part, ".env-") {
				return true
			}
			if ok, _ := path.Match(".env*", part); ok {
				return true
			}
		}
	}

	if strings.HasSuffix(pattern, "/**") {
		prefix := strings.TrimSuffix(pattern, "/**")
		prefix = strings.TrimPrefix(prefix, "**/")
		if relSlash == prefix || strings.HasPrefix(relSlash, prefix+"/") {
			return true
		}
	}

	if strings.HasPrefix(pattern, "**/") {
		sub := strings.TrimPrefix(pattern, "**/")
		if ok, _ := path.Match(sub, relSlash); ok {
			return true
		}
		if ok, _ := path.Match(sub, path.Base(relSlash)); ok {
			return true
		}
		for i := range parts {
			tail := strings.Join(parts[i:], "/")
			if ok, _ := path.Match(sub, tail); ok {
				return true
			}
		}
	}

	if ok, _ := path.Match(pattern, relSlash); ok {
		return true
	}
	return false
}

// ValidateCommandArgv validates an argv vector against Loom v5.1 §9 rules.
func ValidateCommandArgv(argv []string, cfg *PolicyConfig) error {
	if len(argv) == 0 {
		return &ViolationError{Reason: "empty command argv"}
	}
	bin := strings.TrimSpace(argv[0])
	if bin == "" {
		return &ViolationError{Reason: "empty executable in argv[0]"}
	}
	base := filepath.Base(bin)
	if forbiddenWrappers[base] {
		return &ViolationError{Reason: fmt.Sprintf("forbidden shell or wrapper executable %q", base)}
	}
	interp := interpreterBase(base)
	for _, arg := range argv[1:] {
		if interp && interpreterEvalFlags[arg] {
			return &ViolationError{Reason: fmt.Sprintf("forbidden interpreter inline-code flag %q for %q", arg, base)}
		}
		if arg == "-c" || arg == "--eval" || strings.Contains(arg, "$(") || strings.Contains(arg, "`") {
			return &ViolationError{Reason: fmt.Sprintf("forbidden shell evaluation argument %q", arg)}
		}
	}
	if cfg == nil || len(cfg.AllowCommands) == 0 {
		return &ViolationError{Reason: fmt.Sprintf("command %v rejected: allow_commands is empty (grant commands with -allow-cmd \"<prefix>\", e.g. -allow-cmd \"go test\", or task.json constraints.allow_commands)", argv)}
	}
	resolved, rerr := ResolveArgv0(bin, cfg.Workspace)
	if rerr != nil {
		return &ViolationError{Reason: fmt.Sprintf("cannot resolve executable %q: %v", bin, rerr)}
	}
	for _, allowed := range cfg.AllowCommands {
		if len(allowed) == 0 || !isTokenPrefixArgs(allowed, argv) {
			continue
		}
		if argv0Allowed(allowed[0], bin, resolved, cfg.Workspace) {
			return nil
		}
	}
	return &ViolationError{Reason: fmt.Sprintf("command %v does not match any allow_commands entry (allowed prefixes: %s)", argv, formatAllowed(cfg.AllowCommands))}
}

func formatAllowed(list [][]string) string {
	parts := make([]string, 0, len(list))
	for _, a := range list {
		parts = append(parts, strconv.Quote(strings.Join(a, " ")))
	}
	return strings.Join(parts, ", ")
}

// ReadNexusTokenStore reads key=value pairs from $NEXUS_CONFIG_HOME/tokens.env
// (default ~/.config/nexus/tokens.env).
func ReadNexusTokenStore() map[string]string {
	out := make(map[string]string)
	cfgDir := strings.TrimSpace(os.Getenv("NEXUS_CONFIG_HOME"))
	if cfgDir == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return out
		}
		cfgDir = filepath.Join(home, ".config", "nexus")
	}
	tokensPath := filepath.Join(cfgDir, "tokens.env")
	data, err := os.ReadFile(tokensPath)
	if err != nil {
		return out
	}
	for _, rawLine := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if len(v) >= 2 {
			if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
				v = v[1 : len(v)-1]
			}
		}
		if k != "" && v != "" {
			out[k] = v
		}
	}
	return out
}

// ResolveAPIKey returns the first non-empty candidate, preferring the process
// environment over $NEXUS_CONFIG_HOME/tokens.env (env wins, file is fallback).
func ResolveAPIKey(envAndStoreKeys ...string) string {
	for _, k := range envAndStoreKeys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	store := ReadNexusTokenStore()
	for _, k := range envAndStoreKeys {
		if v := strings.TrimSpace(store[k]); v != "" {
			return v
		}
	}
	return ""
}

var interpreterEvalFlags = map[string]bool{
	"-e": true, "-E": true, "-p": true, "-r": true, "--eval": true, "--print": true, "--command": true,
}

func interpreterBase(base string) bool {
	switch {
	case strings.HasPrefix(base, "python"), strings.HasPrefix(base, "perl"), strings.HasPrefix(base, "ruby"),
		strings.HasPrefix(base, "php"), strings.HasPrefix(base, "lua"):
		return true
	}
	return base == "node" || base == "nodejs" || base == "deno" || base == "bun"
}

// ResolveArgv0 resolves the executable to an absolute path. Bare names are
// searched on PATH via exec.LookPath; names containing a slash are resolved
// relative to workspace.
func ResolveArgv0(argv0, workspace string) (string, error) {
	if strings.ContainsRune(argv0, '/') {
		p := argv0
		if !filepath.IsAbs(p) {
			p = filepath.Join(workspace, p)
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			return "", err
		}
		if real, err := filepath.EvalSymlinks(abs); err == nil {
			abs = real
		}
		return abs, nil
	}
	p, err := exec.LookPath(argv0)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	return abs, nil
}

func insideDir(path, dir string) bool {
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// argv0Allowed: a bare allowlist name matches only a bare argv0 found on PATH
// outside the workspace; a path entry must resolve to the same file.
func argv0Allowed(allowed, bin, resolved, workspace string) bool {
	if !strings.ContainsRune(allowed, '/') {
		if bin != allowed {
			return false
		}
		return !insideDir(resolved, workspace)
	}
	want, err := ResolveArgv0(allowed, workspace)
	return err == nil && want == resolved
}

func isTokenPrefixArgs(prefix, full []string) bool {
	if len(prefix) > len(full) {
		return false
	}
	for i := 1; i < len(prefix); i++ {
		if prefix[i] != full[i] {
			return false
		}
	}
	return true
}

// EffectiveAllowCommands applies the global and agent layers (operator-set via
// LOOM_GLOBAL_ALLOW_COMMANDS / LOOM_AGENT_ALLOW_COMMANDS; entries separated by
// ';', tokens by spaces) to the task list. An unset layer does not restrict.
func EffectiveAllowCommands(task [][]string) [][]string {
	layer := func(env string) [][]string {
		v := strings.TrimSpace(os.Getenv(env))
		if v == "" {
			return task
		}
		out := [][]string{}
		for _, e := range strings.Split(v, ";") {
			if f := strings.Fields(e); len(f) > 0 {
				out = append(out, f)
			}
		}
		return out
	}
	return IntersectAllowCommands(layer("LOOM_GLOBAL_ALLOW_COMMANDS"), layer("LOOM_AGENT_ALLOW_COMMANDS"), task)
}
