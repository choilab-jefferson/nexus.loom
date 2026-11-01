package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/choilab-jefferson/nexus.loom/pkg/policy"
)

// EscalationSignal is returned when the model invokes the `escalate` tool (Exit Code 3).
type EscalationSignal struct {
	Reason         string   `json:"reason"`
	MinimalContext string   `json:"minimal_context"`
	Attempted      []string `json:"attempted"`
}

func (e *EscalationSignal) Error() string {
	return fmt.Sprintf("escalation_required: %s", e.Reason)
}

// ToolResult holds the sanitized result of an in-process tool execution.
type ToolResult struct {
	Output      string `json:"output"`
	BytesRaw    int    `json:"bytes_raw"`
	BytesPassed int    `json:"bytes_passed"`
	Truncated   bool   `json:"truncated"`
	IsError     bool   `json:"is_error,omitempty"`
	// ExitCode/TimedOut are set only by run_command. ExitCode is the child's
	// exit status, or -1 when it was killed by a signal (including the timeout
	// kill) or could not be started.
	ExitCode *int `json:"exit_code,omitempty"`
	TimedOut bool `json:"timed_out,omitempty"`
	// ViolationKind is set when a recoverable policy violation was converted
	// into this tool error (see policy.ViolationKind).
	ViolationKind string            `json:"violation_kind,omitempty"`
	Escalation    *EscalationSignal `json:"escalation,omitempty"`
}

// ToolSchema represents a normalized tool definition (§6.1).
type ToolSchema struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// BuiltinSchemas returns the deterministically sorted built-in tool schemas.
func BuiltinSchemas() []ToolSchema {
	schemas := []ToolSchema{
		{
			Name:        "edit_file",
			Description: "Replace an exact target string in a workspace file with replacement.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":        map[string]any{"type": "string"},
					"replacement": map[string]any{"type": "string"},
					"target":      map[string]any{"type": "string"},
				},
				"required": []string{"path", "target", "replacement"},
			},
		},
		{
			Name:        "escalate",
			Description: "Escalate the task to the Nexus orchestrator with structured context (Exit Code 3).",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"attempted":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"minimal_context": map[string]any{"type": "string"},
					"reason":          map[string]any{"type": "string"},
				},
				"required": []string{"reason", "minimal_context"},
			},
		},
		{
			Name:        "list_dir",
			Description: "List entries in a directory within the workspace scope.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{"type": "string"},
				},
				"required": []string{"path"},
			},
		},
		{
			Name:        "read_file",
			Description: "Read lines from a file within the workspace scope.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"end_line":   map[string]any{"type": "integer"},
					"path":       map[string]any{"type": "string"},
					"start_line": map[string]any{"type": "integer"},
				},
				"required": []string{"path"},
			},
		},
		{
			Name:        "run_command",
			Description: "Execute an allowlisted command argv directly without a shell.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"argv":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"timeout_s": map[string]any{"type": "integer"},
				},
				"required": []string{"argv"},
			},
		},
		{
			Name:        "search",
			Description: "Search workspace files with a regular expression, skipping binaries and denied paths.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"max_matches": map[string]any{"type": "integer"},
					"path":        map[string]any{"type": "string"},
					"pattern":     map[string]any{"type": "string"},
				},
				"required": []string{"pattern"},
			},
		},
		{
			Name:        "write_file",
			Description: "Write full content to a file within the workspace scope.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"content": map[string]any{"type": "string"},
					"path":    map[string]any{"type": "string"},
				},
				"required": []string{"path", "content"},
			},
		},
	}
	sort.Slice(schemas, func(i, j int) bool {
		return schemas[i].Name < schemas[j].Name
	})
	return schemas
}

// CanonicalToolSchemasJSON returns a deterministically sorted, compact JSON array of tool schemas (§6.1).
func CanonicalToolSchemasJSON() string {
	raw, err := json.Marshal(BuiltinSchemas())
	if err != nil {
		return "[]"
	}
	return string(raw)
}

// Execute runs a built-in tool confined by cfg.
// Fatal policy violations return a *policy.ViolationError (Exit Code 2); recoverable kinds
// (allowlist mismatch, eval flags) are returned as IsError results with ViolationKind set.
func Execute(ctx context.Context, name string, args map[string]any, cfg *policy.PolicyConfig) (ToolResult, error) {
	if cfg == nil {
		return ToolResult{}, &policy.ViolationError{Reason: "nil PolicyConfig"}
	}
	var (
		rawOut string
		err    error
		meta   *runMeta
	)
	switch name {
	case "read_file":
		rawOut, err = execReadFile(args, cfg)
	case "list_dir":
		rawOut, err = execListDir(args, cfg)
	case "search":
		rawOut, err = execSearch(args, cfg)
	case "edit_file":
		rawOut, err = execEditFile(args, cfg)
	case "write_file":
		rawOut, err = execWriteFile(args, cfg)
	case "run_command":
		rawOut, meta, err = execRunCommand(ctx, args, cfg)
	case "escalate":
		sig := parseEscalation(args)
		outBytes, _ := json.Marshal(sig)
		return ToolResult{
			Output:      string(outBytes),
			BytesRaw:    len(outBytes),
			BytesPassed: len(outBytes),
			Escalation:  sig,
		}, nil
	default:
		return ToolResult{
			Output:      fmt.Sprintf(`{"status":"tool_unavailable","tool":%q}`, name),
			BytesRaw:    len(name) + 36,
			BytesPassed: len(name) + 36,
			IsError:     true,
		}, nil
	}

	if err != nil {
		if kind, ok := policy.ViolationKindOf(err); ok {
			if !kind.Recoverable() {
				return ToolResult{}, err
			}
			res := clampResult(fmt.Sprintf("error: %v", err), true, cfg.MaxOutputBytes)
			res.ViolationKind = kind.String()
			return res, nil
		}
		errMsg := fmt.Sprintf("error: %v", err)
		return clampResult(errMsg, true, cfg.MaxOutputBytes), nil
	}
	res := clampResult(rawOut, false, cfg.MaxOutputBytes)
	if meta != nil {
		res.ExitCode = &meta.exitCode
		res.TimedOut = meta.timedOut
		res.Truncated = res.Truncated || meta.truncated
	}
	return res, nil
}

func truncMarker(dropped int) string {
	return fmt.Sprintf("\n[loom:truncated %d bytes in middle]\n", dropped)
}

// splitHeadTail keeps about 1/3 of maxBytes from the start and 2/3 from the end
// of raw, cutting on UTF-8 boundaries. dropped is the byte count removed.
func splitHeadTail(raw string, maxBytes int) (head, tail string, dropped int) {
	h := maxBytes / 3
	for h > 0 && h < len(raw) && !utf8.RuneStart(raw[h]) {
		h--
	}
	t := len(raw) - (maxBytes - maxBytes/3)
	if t < h {
		t = h
	}
	for t < len(raw) && !utf8.RuneStart(raw[t]) {
		t++
	}
	return raw[:h], raw[t:], t - h
}

func clampResult(raw string, isErr bool, maxBytes int) ToolResult {
	bytesRaw := len(raw)
	if maxBytes <= 0 {
		maxBytes = 16384
	}
	if bytesRaw <= maxBytes {
		return ToolResult{
			Output:      raw,
			BytesRaw:    bytesRaw,
			BytesPassed: bytesRaw,
			Truncated:   false,
			IsError:     isErr,
		}
	}
	head, tail, dropped := splitHeadTail(raw, maxBytes)
	return ToolResult{
		Output:      head + truncMarker(dropped) + tail,
		BytesRaw:    bytesRaw,
		BytesPassed: len(head) + len(tail),
		Truncated:   true,
		IsError:     isErr,
	}
}

func execReadFile(args map[string]any, cfg *policy.PolicyConfig) (string, error) {
	relPath := getString(args, "path")
	resolved, err := policy.ResolveScopedPath(cfg.Workspace, relPath, false, cfg)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(data), "\n")
	startLine := getInt(args, "start_line", 1)
	if startLine < 1 {
		startLine = 1
	}
	maxLines := cfg.MaxReadLines
	if maxLines <= 0 {
		maxLines = 400
	}
	endLine := getInt(args, "end_line", startLine+maxLines-1)
	if endLine-startLine+1 > maxLines {
		endLine = startLine + maxLines - 1
	}
	if startLine > len(lines) {
		return "", nil
	}
	if endLine > len(lines) {
		endLine = len(lines)
	}
	var b strings.Builder
	for i := startLine; i <= endLine; i++ {
		fmt.Fprintf(&b, "%d: %s\n", i, lines[i-1])
	}
	return b.String(), nil
}

func execListDir(args map[string]any, cfg *policy.PolicyConfig) (string, error) {
	relPath := getString(args, "path")
	if relPath == "" {
		relPath = "."
	}
	resolved, err := policy.ResolveScopedPath(cfg.Workspace, relPath, false, cfg)
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(resolved)
	if err != nil {
		return "", err
	}
	var names []string
	for _, e := range entries {
		name := e.Name()
		if name == ".git" || strings.HasPrefix(name, ".env") {
			continue
		}
		if e.IsDir() {
			name += "/"
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, "\n"), nil
}

func execSearch(args map[string]any, cfg *policy.PolicyConfig) (string, error) {
	pattern := getString(args, "pattern")
	if pattern == "" {
		return "", fmt.Errorf("search requires non-empty pattern")
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", fmt.Errorf("invalid regexp %q: %w", pattern, err)
	}
	relPath := getString(args, "path")
	if relPath == "" {
		relPath = "."
	}
	root, err := policy.ResolveScopedPath(cfg.Workspace, relPath, false, cfg)
	if err != nil {
		return "", err
	}
	maxMatches := getInt(args, "max_matches", 50)
	if maxMatches <= 0 {
		maxMatches = 50
	}

	wsReal, err := policy.ResolveScopedPath(cfg.Workspace, ".", false, cfg)
	if err != nil {
		return "", err
	}

	var matches []string
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if name == ".git" || name == ".worktrees" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, relErr := filepath.Rel(wsReal, p)
		if relErr != nil {
			return nil
		}
		if _, scopeErr := policy.ResolveScopedPath(cfg.Workspace, rel, false, cfg); scopeErr != nil {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil || info.Size() > 1<<20 {
			return nil
		}
		data, readErr := os.ReadFile(p)
		if readErr != nil || isBinaryContent(data) {
			return nil
		}
		lines := strings.Split(string(data), "\n")
		for idx, line := range lines {
			if re.MatchString(line) {
				matches = append(matches, fmt.Sprintf("%s:%d:%s", filepath.ToSlash(rel), idx+1, line))
				if len(matches) >= maxMatches {
					return fs.SkipAll
				}
			}
		}
		return nil
	})
	if walkErr != nil {
		return "", walkErr
	}
	return strings.Join(matches, "\n"), nil
}

func isBinaryContent(data []byte) bool {
	sample := data
	if len(sample) > 1024 {
		sample = sample[:1024]
	}
	if bytes.IndexByte(sample, 0) != -1 {
		return true
	}
	return !utf8.Valid(sample)
}

func execEditFile(args map[string]any, cfg *policy.PolicyConfig) (string, error) {
	relPath := getString(args, "path")
	target := getString(args, "target")
	replacement := getString(args, "replacement")
	if target == "" {
		return "", fmt.Errorf("edit_file requires non-empty target")
	}
	resolved, err := policy.ResolveScopedPath(cfg.Workspace, relPath, true, cfg)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return "", err
	}
	content := string(data)
	if !strings.Contains(content, target) {
		return "", fmt.Errorf("target string not found in %s", relPath)
	}
	updated := strings.Replace(content, target, replacement, 1)
	if err := writeNoFollow(resolved, []byte(updated)); err != nil {
		return "", err
	}
	return fmt.Sprintf("edited %s", filepath.ToSlash(relPath)), nil
}

// writeNoFollow writes data to path, refusing to follow a symlink at the leaf.
func writeNoFollow(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func execWriteFile(args map[string]any, cfg *policy.PolicyConfig) (string, error) {
	relPath := getString(args, "path")
	content := getString(args, "content")
	resolved, err := policy.ResolveScopedPath(cfg.Workspace, relPath, true, cfg)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
		return "", err
	}
	if err := writeNoFollow(resolved, []byte(content)); err != nil {
		return "", err
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(content), filepath.ToSlash(relPath)), nil
}

// safeEnvKeys is the env allowlist for run_command children. Everything else
// (API keys, cloud/VCS tokens, ...) is dropped.
var safeEnvKeys = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "SHELL": true,
	"LANG": true, "LANGUAGE": true, "TERM": true, "TZ": true, "TMPDIR": true,
	"GOPATH": true, "GOCACHE": true, "GOMODCACHE": true, "GOROOT": true, "GOFLAGS": true,
	"CARGO_HOME": true, "RUSTUP_HOME": true, "VIRTUAL_ENV": true,
	// Non-secret ML defaults.
	"PYTHONPATH": true, "PYTHONUNBUFFERED": true, "PYTHONHASHSEED": true,
	"CUDA_VISIBLE_DEVICES": true, "CUDA_HOME": true,
	"HF_HOME": true, "HF_HUB_CACHE": true, "HF_DATASETS_CACHE": true, "TRANSFORMERS_CACHE": true,
	"OMP_NUM_THREADS": true,
}

// envPassed reports whether key is allowed through to a child. Secret-looking
// names pass only when named exactly in passEnv (never via a prefix pattern).
func envPassed(key string, passEnv []string) bool {
	if policy.IsSecretEnvName(key) {
		for _, p := range passEnv {
			if p == key {
				return true
			}
		}
		return false
	}
	if safeEnvKeys[key] || strings.HasPrefix(key, "LC_") || strings.HasPrefix(key, "CUDA_") {
		return true
	}
	for _, p := range passEnv {
		if p == key || (strings.HasSuffix(p, "*") && strings.HasPrefix(key, strings.TrimSuffix(p, "*"))) {
			return true
		}
	}
	return false
}

const (
	maxRunTimeoutSec = 300
	maxRunOutput     = 1 << 20 // 1 MiB
	runWaitDelay     = 2 * time.Second
)

// scrubSubprocessEnv filters env to the allowlist plus the optional passEnv extras.
func scrubSubprocessEnv(env []string, argv0 string, passEnv ...string) []string {
	out := make([]string, 0, len(env)+2)
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		if envPassed(key, passEnv) {
			out = append(out, kv)
		}
	}
	if filepath.Base(argv0) == "ikb" {
		if tok := policy.ResolveAPIKey("IKB_MCP_TOKEN", "IKB_API_TOKEN"); tok != "" {
			out = append(out, "IKB_MCP_TOKEN="+tok, "IKB_API_TOKEN="+tok)
		}
	}
	return out
}

// cappedBuffer keeps the first max/3 bytes plus a ring of the last max-max/3
// bytes, so a runaway writer never blocks or grows memory while the end of the
// output (tracebacks, test summaries) survives.
type cappedBuffer struct {
	mu      sync.Mutex
	max     int
	head    []byte
	ring    []byte // len == tail capacity once allocated
	ringPos int    // next write index
	ringLen int    // valid bytes in ring (<= len(ring))
	total   int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(p)
	c.total += n
	headMax := c.max / 3
	if room := headMax - len(c.head); room > 0 {
		k := min(room, len(p))
		c.head = append(c.head, p[:k]...)
		p = p[k:]
	}
	if len(p) == 0 {
		return n, nil
	}
	if c.ring == nil {
		c.ring = make([]byte, c.max-headMax)
	}
	if len(p) >= len(c.ring) {
		p = p[len(p)-len(c.ring):]
		c.ringPos, c.ringLen = 0, 0
	}
	for len(p) > 0 {
		k := copy(c.ring[c.ringPos:], p)
		p = p[k:]
		c.ringPos = (c.ringPos + k) % len(c.ring)
		c.ringLen = min(c.ringLen+k, len(c.ring))
	}
	return n, nil
}

func (c *cappedBuffer) truncated() bool { return c.total > len(c.head)+c.ringLen }

// String returns head + marker + tail when bytes were dropped, else all bytes.
func (c *cappedBuffer) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	tail := make([]byte, 0, c.ringLen)
	if c.ringLen == len(c.ring) && c.ringLen > 0 {
		tail = append(tail, c.ring[c.ringPos:]...)
		tail = append(tail, c.ring[:c.ringPos]...)
	} else {
		tail = append(tail, c.ring[:c.ringLen]...)
	}
	if !c.truncated() {
		return string(c.head) + string(tail)
	}
	h, t := string(c.head), string(tail)
	// Trim partial runes at the cut edges (at most UTFMax-1 bytes each).
	hb := len(h)
	for i := 0; i < utf8.UTFMax && hb > 0; i++ {
		if r, sz := utf8.DecodeLastRuneInString(h[:hb]); r == utf8.RuneError && sz <= 1 {
			hb--
		} else {
			break
		}
	}
	ts := 0
	for ts < len(t) && ts < utf8.UTFMax && !utf8.RuneStart(t[ts]) {
		ts++
	}
	dropped := c.total - hb - (len(t) - ts)
	return h[:hb] + truncMarker(dropped) + t[ts:]
}

// execRunCommand runs an allowlisted argv. NOT a sandbox: the child has the
// harness user's full filesystem/network access; policy only gates argv.
func execRunCommand(ctx context.Context, args map[string]any, cfg *policy.PolicyConfig) (string, *runMeta, error) {
	argv := getStringSlice(args, "argv")
	if err := policy.ValidateCommandArgv(argv, cfg); err != nil {
		return "", nil, err
	}
	wsReal, err := policy.ResolveScopedPath(cfg.Workspace, ".", false, cfg)
	if err != nil {
		return "", nil, err
	}
	resolved, err := policy.ResolveArgv0(argv[0], cfg.Workspace)
	if err != nil {
		return "", nil, &policy.ViolationError{Reason: fmt.Sprintf("cannot resolve executable %q: %v", argv[0], err)}
	}
	timeoutSec := getInt(args, "timeout_s", 30)
	if timeoutSec <= 0 {
		timeoutSec = 30
	}
	if timeoutSec > maxRunTimeoutSec {
		timeoutSec = maxRunTimeoutSec
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(runCtx, resolved, argv[1:]...)
	// Keep argv[0] as the caller's name: multi-call binaries (busybox on
	// alpine, where yes/sleep are symlinks) dispatch on it, and the resolved
	// path is the symlink target.
	cmd.Args[0] = argv[0]
	cmd.Dir = wsReal
	cmd.Env = scrubSubprocessEnv(os.Environ(), argv[0], cfg.PassEnv...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = runWaitDelay
	combined := &cappedBuffer{max: maxRunOutput}
	cmd.Stdout = combined
	cmd.Stderr = combined

	runErr := cmd.Run()
	// Reap any backgrounded children left in the process group.
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	out := combined.String()
	meta := &runMeta{truncated: combined.truncated()}
	if runErr == nil {
		return out, meta, nil
	}
	meta.exitCode = -1
	var ee *exec.ExitError
	if errors.As(runErr, &ee) {
		meta.exitCode = ee.ExitCode()
	}
	meta.timedOut = errors.Is(runCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
	return fmt.Sprintf("%s\n[exit_error: %v]", strings.TrimSpace(out), runErr), meta, nil
}

// runMeta is the structured outcome of a run_command execution.
type runMeta struct {
	exitCode  int
	timedOut  bool
	truncated bool
}

func parseEscalation(args map[string]any) *EscalationSignal {
	return &EscalationSignal{
		Reason:         getString(args, "reason"),
		MinimalContext: getString(args, "minimal_context"),
		Attempted:      getStringSlice(args, "attempted"),
	}
}

func getString(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	v, ok := args[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

func getInt(args map[string]any, key string, def int) int {
	if args == nil {
		return def
	}
	v, ok := args[key]
	if !ok || v == nil {
		return def
	}
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i)
		}
	}
	return def
}

func getStringSlice(args map[string]any, key string) []string {
	if args == nil {
		return nil
	}
	v, ok := args[key]
	if !ok || v == nil {
		return nil
	}
	switch s := v.(type) {
	case []string:
		return s
	case []any:
		out := make([]string, 0, len(s))
		for _, item := range s {
			out = append(out, fmt.Sprintf("%v", item))
		}
		return out
	}
	return nil
}
