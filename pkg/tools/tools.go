package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
			Name: "edit_file",
			Description: "Change an existing workspace file by exact replacement blocks. Use this for ordinary changes " +
				"(write_file only for a new file or a full rewrite). Each block's target must appear exactly once in the " +
				"file as it is now (add surrounding lines to make it unique); all blocks are matched against the current " +
				"file and applied together, or none is. Returns a unified diff of the change.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{"type": "string"},
					"edits": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"target":      map[string]any{"type": "string", "description": "exact current text, unique in the file"},
								"replacement": map[string]any{"type": "string", "description": "text that replaces it"},
							},
							"required": []string{"target", "replacement"},
						},
					},
				},
				"required": []string{"path", "edits"},
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
			Name: "run_readonly",
			Description: "Run a read-only git or glab command as an argv array (no shell). Enabled only when the task sets constraints.readonly_commands. " +
				"git: log, show, diff, status, branch --list, rev-parse, ls-files, blame. " +
				"glab: mr view|list|diff, issue view|list, ci status|list, api <path> (GET only, project/group merge_requests/issues/pipelines/jobs/repository/labels/milestones paths). Anything else is refused.",
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
		{
			Name:        "job_kill",
			Description: "Kill a background job's process group.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"job_id": map[string]any{"type": "string"},
				},
				"required": []string{"job_id"},
			},
		},
		{
			Name:        "job_log",
			Description: "Return the last tail_n lines of a background job's log.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"job_id": map[string]any{"type": "string"},
					"tail_n": map[string]any{"type": "integer"},
				},
				"required": []string{"job_id"},
			},
		},
		{
			Name:        "job_start",
			Description: "Start an allowlisted command argv in the background (no shell); returns job_id. Non-detached jobs are killed when the run exits; detach=true jobs survive it.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"argv":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"detach": map[string]any{"type": "boolean"},
				},
				"required": []string{"argv"},
			},
		},
		{
			Name:        "job_status",
			Description: "Return status (running/exited/killed), exit_code, pid and detached flag of a background job.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"job_id": map[string]any{"type": "string"},
				},
				"required": []string{"job_id"},
			},
		},
		{
			Name:        "job_wait",
			Description: "Block up to timeout_s until a background job exits; returns final status and log tail.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"job_id":    map[string]any{"type": "string"},
					"timeout_s": map[string]any{"type": "integer"},
				},
				"required": []string{"job_id"},
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
	return SchemasJSON(BuiltinSchemas())
}

// FilterSchemas returns the built-in schemas named in names, in canonical order.
// nil or empty names means the full built-in set; an unknown name is an error.
func FilterSchemas(names []string) ([]ToolSchema, error) {
	all := BuiltinSchemas()
	if len(names) == 0 {
		return all, nil
	}
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	out := []ToolSchema{}
	for _, s := range all {
		if want[s.Name] {
			out = append(out, s)
			delete(want, s.Name)
		}
	}
	for n := range want {
		return nil, fmt.Errorf("unknown tool %q in tool set", n)
	}
	return out, nil
}

// SchemasJSON is the compact JSON array of schemas (the form frozen in the run prefix).
func SchemasJSON(schemas []ToolSchema) string {
	if schemas == nil {
		schemas = []ToolSchema{}
	}
	raw, err := json.Marshal(schemas)
	if err != nil {
		return "[]"
	}
	return string(raw)
}

// ParseSchemasJSON decodes a prefix's tool schemas. An empty string means the full
// built-in set (callers that never froze a tool set); "[]" or invalid JSON means no tools.
func ParseSchemasJSON(raw string) []ToolSchema {
	if strings.TrimSpace(raw) == "" {
		return BuiltinSchemas()
	}
	var out []ToolSchema
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return []ToolSchema{} // fail closed: an unreadable tool set exposes nothing
	}
	return out
}

// NotAvailableKind is the ViolationKind of a call to a built-in tool that is not exposed.
const NotAvailableKind = "tool_not_available"

// NotAvailable is the observation for a call to a tool the run does not expose; nothing runs.
func NotAvailable(name string) ToolResult {
	out := fmt.Sprintf(`{"status":%q,"tool":%q}`, NotAvailableKind, name)
	return ToolResult{Output: out, BytesRaw: len(out), BytesPassed: len(out), IsError: true, ViolationKind: NotAvailableKind}
}

// IsBuiltin reports whether name is a built-in tool.
func IsBuiltin(name string) bool {
	for _, s := range BuiltinSchemas() {
		if s.Name == name {
			return true
		}
	}
	return false
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
	if len(cfg.ReadonlyCommands) > 0 {
		switch name {
		case "write_file", "edit_file", "run_command", "job_start":
			err = &policy.ViolationError{Reason: fmt.Sprintf("%s is disabled in a task that enables run_readonly (read-only tasks cannot write or run commands)", name), Kind: policy.KindAllowlist}
			return clampResult(fmt.Sprintf("error: %v", err), true, cfg.MaxOutputBytes).withViolation(policy.KindAllowlist), nil
		}
	}
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
	case "run_readonly":
		rawOut, meta, err = execReadonly(ctx, args, cfg)
	case "job_start":
		rawOut, err = execJobStart(args, cfg)
	case "job_status":
		rawOut, err = execJobStatus(args, cfg)
	case "job_log":
		rawOut, err = execJobLog(args, cfg)
	case "job_wait":
		rawOut, err = execJobWait(ctx, args, cfg)
	case "job_kill":
		rawOut, err = execJobKill(args, cfg)
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

func (r ToolResult) withViolation(k policy.ViolationKind) ToolResult {
	r.ViolationKind = k.String()
	return r
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
	cfg.Seen.Record(resolved, data) // the run has seen this content (write_file's stale guard)
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

// rewriteLocked replaces the whole content of an open, locked file.
func rewriteLocked(f *os.File, data []byte) error {
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.WriteAt(data, 0); err != nil {
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
	_, statErr := os.Lstat(resolved)
	existed := statErr == nil
	f, err := openLocked(resolved, true) // waits while another process writes the same file
	if err != nil {
		return "", err
	}
	defer f.Close()
	rel := filepath.ToSlash(relPath)
	if existed && cfg.Seen != nil {
		// overwrite only what this run has seen as it is now (PI 2026-11-13: check right before the change)
		cur, err := io.ReadAll(f)
		if err != nil {
			return "", err
		}
		seen, ok := cfg.Seen.Lookup(resolved)
		switch {
		case !ok:
			return "", fmt.Errorf("%s exists and was not read in this run: read it first, or change it with edit_file", rel)
		case seen != policy.ContentHash(cur):
			return "", fmt.Errorf("%s changed since you read it (another process wrote it): read it again before overwriting, or use edit_file", rel)
		}
	}
	if err := rewriteLocked(f, []byte(content)); err != nil {
		return "", err
	}
	cfg.Seen.Record(resolved, []byte(content))
	what := "created"
	if existed {
		what = "replaced"
	}
	return fmt.Sprintf("wrote %d bytes to %s (%s)", len(content), rel, what), nil
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

// clampRunTimeout caps timeout_s at cfg.MaxCmdTimeoutSec (default maxRunTimeoutSec).
func clampRunTimeout(sec int, cfg *policy.PolicyConfig) int {
	limit := maxRunTimeoutSec
	if cfg != nil && cfg.MaxCmdTimeoutSec > 0 {
		limit = cfg.MaxCmdTimeoutSec
	}
	if sec > limit {
		return limit
	}
	return sec
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
	timeoutSec = clampRunTimeout(timeoutSec, cfg)
	env := scrubSubprocessEnv(os.Environ(), argv[0], cfg.PassEnv...)
	return runChild(ctx, resolved, argv, wsReal, env, timeoutSec, maxRunOutput)
}

// runChild execs resolved with argv (argv[0] kept as the caller's name), cwd dir
// and exactly env, under a timeout, killing the whole process group on timeout
// or return. Output (stdout+stderr) is capped at maxBytes, head + tail kept.
func runChild(ctx context.Context, resolved string, argv []string, dir string, env []string, timeoutSec, maxBytes int) (string, *runMeta, error) {
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(runCtx, resolved, argv[1:]...)
	// Keep argv[0] as the caller's name: multi-call binaries (busybox on
	// alpine, where yes/sleep are symlinks) dispatch on it, and the resolved
	// path is the symlink target.
	cmd.Args[0] = argv[0]
	cmd.Dir = dir
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = runWaitDelay
	combined := &cappedBuffer{max: maxBytes}
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
