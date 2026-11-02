package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/choilab-jefferson/nexus.loom/pkg/compact"
	"github.com/choilab-jefferson/nexus.loom/pkg/engine"
	"github.com/choilab-jefferson/nexus.loom/pkg/policy"
	"github.com/choilab-jefferson/nexus.loom/pkg/providers"
)

// run executes runMain with captured stdout/stderr and the given stdin.
func run(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	oo, oe, oi := os.Stdout, os.Stderr, os.Stdin
	defer func() { os.Stdout, os.Stderr, os.Stdin = oo, oe, oi }()
	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	inR, inW, _ := os.Pipe()
	os.Stdout, os.Stderr, os.Stdin = outW, errW, inR
	go func() { _, _ = inW.WriteString(stdin); inW.Close() }()
	var ob, eb bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(&ob, outR) }()
	go func() { defer wg.Done(); _, _ = io.Copy(&eb, errR) }()
	rc := runMain(args)
	outW.Close()
	errW.Close()
	wg.Wait()
	return rc, ob.String(), eb.String()
}

// fakeProvider is a deterministic "model" that works only from the history it is shown:
//   - prompt containing "create": write_file (a made-up name), then final text
//   - prompt containing "refer": read the file the history's write_file call created (the
//     prompt never names it), then report its content
//   - anything else: echo how many messages it saw
type fakeProvider struct {
	name string
	seen [][]compact.Message
}

func (f *fakeProvider) Chat(_ context.Context, _ compact.PrefixHeader, msgs []compact.Message, _ int) (providers.Response, error) {
	f.seen = append(f.seen, append([]compact.Message(nil), msgs...))
	base := providers.Response{RawStopReason: "stop", PromptTokens: 10, OutputTokens: 5, Provider: f.name}
	tc := func(id, name string, args map[string]any) providers.Response {
		r := base
		r.ToolCalls = []compact.ToolCall{{ID: id, Name: name, Arguments: args}}
		return r
	}
	last := msgs[len(msgs)-1]
	if last.Role == "tool" {
		r := base
		r.Content = f.name + " saw tool result: " + last.Content
		return r, nil
	}
	// Find the last user prompt.
	prompt := last.Content
	switch {
	case strings.Contains(prompt, "create"):
		return tc("c-write", "write_file", map[string]any{"path": "secret-name-7.txt", "content": "payload-42"}), nil
	case strings.Contains(prompt, "refer"):
		for _, m := range msgs {
			for _, c := range m.ToolCalls {
				if c.Name == "write_file" {
					return tc("c-read", "read_file", map[string]any{"path": c.Arguments["path"]}), nil
				}
			}
		}
		r := base
		r.Content = f.name + " no file known"
		return r, nil
	}
	r := base
	r.Content = f.name + " saw " + itoa(len(msgs)) + " msgs"
	return r, nil
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func useFakes(t *testing.T) map[string]*fakeProvider {
	t.Helper()
	fakes := map[string]*fakeProvider{}
	testProviderHook = func(name string) providers.Provider {
		f := &fakeProvider{name: name}
		fakes[name] = f
		return f
	}
	t.Cleanup(func() { testProviderHook = nil })
	return fakes
}

func readCtx(t *testing.T, path string) *engine.Context {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c, _, err := engine.ParseContext(raw)
	if err != nil {
		t.Fatalf("context invalid: %v\n%s", err, raw)
	}
	return c
}

// (a) call 1 creates a file; caller deletes a middle message and adds a note; call 2 refers to
// the file without being told its name.
func TestContextEditedByCallerContinues(t *testing.T) {
	fakes := useFakes(t)
	ws := t.TempDir()
	ctxPath := filepath.Join(t.TempDir(), "ctx.json")
	rc, out, errS := run(t, "", "-p", "create a file", "--provider", "mock", "--workspace", ws, "--json", "--context-out", ctxPath)
	if rc != 0 {
		t.Fatalf("rc=%d out=%s err=%s", rc, out, errS)
	}
	var res engine.TaskResult
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.ContextPath != ctxPath || res.UserTurn != 1 {
		t.Fatalf("result %v %+v", err, res)
	}
	if _, err := os.Stat(filepath.Join(ws, "secret-name-7.txt")); err != nil {
		t.Fatal(err)
	}
	c := readCtx(t, ctxPath)
	// Messages: user, assistant(write_file), tool, assistant(final).
	if len(c.Messages) != 4 || c.SchemaVersion != "loom.context.v1" || c.Runs[0].FilesChanged.Added[0] != "secret-name-7.txt" {
		t.Fatalf("%+v", c)
	}
	// Caller edit: delete the middle assistant final message... and inject a note.
	c.Messages = append(c.Messages[:3], compact.Message{Role: "user", Content: "NOTE: the user likes terse answers."})
	raw, _ := json.Marshal(c)
	if err := os.WriteFile(ctxPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	rc, out, errS = run(t, "", "-p", "refer to the file you made", "--provider", "mock", "--workspace", ws, "--json", "--context-in", ctxPath, "--context-out", ctxPath)
	if rc != 0 {
		t.Fatalf("rc=%d out=%s err=%s", rc, out, errS)
	}
	if !strings.Contains(out, "payload-42") || !strings.Contains(out, `"user_turn":2`) {
		t.Fatalf("call 2 did not use restored context: %s", out)
	}
	first := fakes["mock"].seen[len(fakes["mock"].seen)-2]
	if got := first[len(first)-1].Content; !strings.Contains(got, "NOTE: the user likes terse") || !strings.Contains(got, "refer to the file") {
		t.Fatalf("note not merged with prompt: %q", got)
	}
	c2 := readCtx(t, ctxPath)
	if c2.UserTurn != 2 || len(c2.Runs) != 2 || c2.CumulativeUsage.TotalTokens != 15*4 {
		t.Fatalf("c2 turn=%d runs=%d cum=%+v", c2.UserTurn, len(c2.Runs), c2.CumulativeUsage)
	}
	var res2 engine.TaskResult
	_ = json.Unmarshal([]byte(out), &res2)
	if res2.Usage.TotalTokens != 30 || res2.CumulativeUsage.TotalTokens != 60 {
		t.Fatalf("usage=%+v cum=%+v", res2.Usage, res2.CumulativeUsage)
	}
}

// (b) dangling tool_call / orphan tool_result are repaired per the documented rule.
func TestContextBrokenStructureRepaired(t *testing.T) {
	fakes := useFakes(t)
	ctx := engine.Context{SchemaVersion: engine.ContextSchema, UserTurn: 1, Messages: []compact.Message{
		{Role: "user", Content: "start"},
		{Role: "assistant", Content: "calling", ToolCalls: []compact.ToolCall{{ID: "d1", Name: "read_file", Arguments: map[string]any{"path": "x"}}}},
		{Role: "user", Content: "never mind"},
		{Role: "tool", ToolCallID: "ghost", ToolName: "read_file", Content: "orphan"},
		{Role: "assistant", Content: "ok"},
	}}
	raw, _ := json.Marshal(ctx)
	rc, out, errS := run(t, string(raw), "-p", "continue", "--provider", "mock", "--workspace", t.TempDir(), "--context-in", "-")
	if rc != 0 {
		t.Fatalf("rc=%d out=%s err=%s", rc, out, errS)
	}
	for _, want := range []string{"dangling tool_call", "orphan tool_result"} {
		if !strings.Contains(errS, want) {
			t.Errorf("stderr missing %q: %s", want, errS)
		}
	}
	for _, m := range fakes["mock"].seen[0] {
		if m.Role == "tool" || len(m.ToolCalls) > 0 {
			t.Fatalf("broken structure reached the provider: %+v", fakes["mock"].seen[0])
		}
	}
}

func TestContextInvalidExits2(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"bad-json":   "{nope",
		"bad-schema": `{"schema_version":"loom.context.v2","messages":[]}`,
	} {
		p := filepath.Join(dir, name)
		_ = os.WriteFile(p, []byte(body), 0o600)
		rc, out, errS := run(t, "", "-p", "x", "--provider", "mock", "--workspace", dir, "--context-in", p)
		if rc != policy.ExitHarnessErr || out != "" || !strings.Contains(errS, "context") {
			t.Errorf("%s: rc=%d out=%q err=%q", name, rc, out, errS)
		}
	}
	if rc, _, _ := run(t, "", "-p", "x", "--provider", "mock", "--workspace", dir, "--context-in", filepath.Join(dir, "missing")); rc != policy.ExitHarnessErr {
		t.Errorf("missing file rc=%d", rc)
	}
}

// (c) context from provider A (mock) continues on provider B (mock2).
func TestContextCrossProvider(t *testing.T) {
	fakes := useFakes(t)
	ws := t.TempDir()
	ctxPath := filepath.Join(t.TempDir(), "c.json")
	if rc, out, e := run(t, "", "-p", "create it", "--provider", "mock", "--workspace", ws, "--context-out", ctxPath); rc != 0 {
		t.Fatalf("%d %s %s", rc, out, e)
	}
	rc, out, e := run(t, "", "-p", "refer to it", "--provider", "mock2", "--workspace", ws, "--context-in", ctxPath)
	if rc != 0 || !(strings.Contains(out, "mock2 saw tool result") && strings.Contains(out, "payload-42")) {
		t.Fatalf("%d %s %s", rc, out, e)
	}
	if fakes["mock2"] == nil || fakes["mock"] == nil {
		t.Fatal("both providers must have been used")
	}
	if rc, out, _ := run(t, "", "-p", "hello", "--provider", "mock2", "--workspace", ws); rc != 0 {
		t.Fatalf("rc=%d out=%s", rc, out)
	}
}

// Without the test hook, mock2 is a built-in alias.
func TestMock2Alias(t *testing.T) {
	rc, out, _ := run(t, "", "-p", "hi", "--provider", "mock2", "--workspace", t.TempDir())
	if rc != 0 || strings.TrimSpace(out) != "mock2 task complete" {
		t.Fatalf("rc=%d out=%q", rc, out)
	}
}

// (d) stdout/stdin pipeline: --context-out - | --context-in -
func TestContextPipeline(t *testing.T) {
	useFakes(t)
	ws := t.TempDir()
	rc, ctx1, e := run(t, "", "-p", "one", "--provider", "mock", "--workspace", ws, "--json", "--context-out", "-")
	if rc != 0 {
		t.Fatalf("%d %s", rc, e)
	}
	// stdout is exactly one context document; the --json result went to stderr.
	if c, _, err := engine.ParseContext([]byte(ctx1)); err != nil || c.UserTurn != 1 {
		t.Fatalf("stdout is not a context: %v %q", err, ctx1)
	}
	if !strings.Contains(e, `"user_turn":1`) {
		t.Fatalf("result not on stderr: %q", e)
	}
	rc, ctx2, e := run(t, ctx1, "-p", "two", "--provider", "mock2", "--workspace", ws, "--context-in", "-", "--context-out", "-")
	if rc != 0 {
		t.Fatalf("%d %s", rc, e)
	}
	c2, _, err := engine.ParseContext([]byte(ctx2))
	if err != nil || c2.UserTurn != 2 || len(c2.Messages) != 4 || c2.CumulativeUsage.TotalTokens != 30 {
		t.Fatalf("%v %+v", err, c2)
	}
}

// (e1) an interrupted run still resumes from loom's own checkpoint, and a context-continued
// run keeps its prior totals through resume and writes the context.
func TestResumeStillWorksWithContext(t *testing.T) {
	useFakes(t)
	dir := t.TempDir()
	trace := filepath.Join(dir, "t.jsonl")
	ctxOut := filepath.Join(dir, "out.json")
	prior := &engine.Context{SchemaVersion: engine.ContextSchema, UserTurn: 1, CumulativeUsage: engine.Usage{PromptTokens: 100, TotalTokens: 100}, Runs: []engine.RunRecord{{RunIndex: 1, Status: "success"}}}
	meta := runMeta{Workspace: dir, Provider: "mock", Trace: trace, ContextOut: ctxOut, Prior: prior}
	writeCkpt(meta, engine.Checkpoint{RunID: "r1", Spec: engine.TaskSpec{Instruction: "go on", Constraints: engine.TaskConstraints{MaxTurns: 4}}, Turn: 1,
		Msgs: []compact.Message{{Role: "user", Content: "old"}, {Role: "assistant", Content: "older"}, {Role: "user", Content: "go on"}}})
	rc, out, e := run(t, "", "resume", trace, "--json")
	if rc != 0 {
		t.Fatalf("%d %s %s", rc, out, e)
	}
	var res engine.TaskResult
	_ = json.Unmarshal([]byte(out), &res)
	if res.RunID != "r1" || res.UserTurn != 2 || res.CumulativeUsage.TotalTokens != 100+res.Usage.TotalTokens {
		t.Fatalf("%+v", res)
	}
	c := readCtx(t, ctxOut)
	if c.UserTurn != 2 || len(c.Runs) != 2 || c.Messages[0].Content != "old" {
		t.Fatalf("%+v", c)
	}
	// Plain resume (no context) is unchanged.
	trace2 := filepath.Join(dir, "t2.jsonl")
	writeCkpt(runMeta{Workspace: dir, Provider: "mock", Trace: trace2}, engine.Checkpoint{RunID: "r2", Spec: engine.TaskSpec{Instruction: "x"}, Turn: 1,
		Msgs: []compact.Message{{Role: "user", Content: "x"}}})
	if rc, out, e := run(t, "", "resume", trace2); rc != 0 {
		t.Fatalf("%d %s %s", rc, out, e)
	}
}

// (e2) two parallel conversations with distinct traces/workspaces do not interfere.
func TestParallelConversationsIndependent(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and spawns processes")
	}
	bin := buildLoom(t)
	root := t.TempDir()
	var wg sync.WaitGroup
	errs := make([]string, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ws := filepath.Join(root, "ws"+string(rune('a'+i)))
			_ = os.MkdirAll(ws, 0o755)
			tr := filepath.Join(root, "tr"+string(rune('a'+i))+".jsonl")
			cx := filepath.Join(root, "cx"+string(rune('a'+i))+".json")
			for turn := 1; turn <= 2; turn++ {
				args := []string{"-p", "conv-" + string(rune('a'+i)) + "-turn-" + string(rune('0'+turn)), "--provider", "mock", "--workspace", ws, "--trace", tr, "--context-out", cx}
				if turn == 2 {
					args = append(args, "--context-in", cx)
				}
				if out, err := exec.Command(bin, args...).CombinedOutput(); err != nil {
					errs[i] = string(out)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != "" {
			t.Fatalf("conv %d: %s", i, e)
		}
	}
	for i := 0; i < 2; i++ {
		c := readCtx(t, filepath.Join(root, "cx"+string(rune('a'+i))+".json"))
		other := "conv-" + string(rune('a'+1-i))
		raw, _ := json.Marshal(c.Messages)
		if c.UserTurn != 2 || strings.Contains(string(raw), other) || !strings.Contains(string(raw), "conv-"+string(rune('a'+i))+"-turn-1") {
			t.Fatalf("conv %d crossed or wrong: %s", i, raw)
		}
	}
}

func TestCapabilitiesAdvertisesContextContinuation(t *testing.T) {
	m := capabilitiesMap()
	cc, ok := m["context_continuation"].(map[string]any)
	if !ok || cc["schema"] != "loom.context.v1" {
		t.Fatalf("%v", m)
	}
}
