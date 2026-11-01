package engine

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/choilab-jefferson/nexus.loom/pkg/compact"
	"github.com/choilab-jefferson/nexus.loom/pkg/policy"
	"github.com/choilab-jefferson/nexus.loom/pkg/providers"
	"github.com/choilab-jefferson/nexus.loom/pkg/trace"
)

func fcSpec() TaskSpec { return TaskSpec{Instruction: "go", Constraints: TaskConstraints{MaxTurns: 5}} }

func TestFilesChangedCreateEditDelete(t *testing.T) {
	ws := t.TempDir()
	os.WriteFile(filepath.Join(ws, "edit.txt"), []byte("a"), 0o644)
	os.WriteFile(filepath.Join(ws, "del.txt"), []byte("a"), 0o644)
	os.Mkdir(filepath.Join(ws, ".git"), 0o755)
	os.WriteFile(filepath.Join(ws, ".git", "x"), []byte("a"), 0o644)
	pol := policy.DefaultPolicyConfig(ws)
	mock := providers.NewScriptedMockProvider(
		toolCallResp("1", "write_file", map[string]any{"path": "new.txt", "content": "n"}),
		toolCallResp("2", "write_file", map[string]any{"path": "edit.txt", "content": "longer content"}),
		toolCallResp("3", "write_file", map[string]any{"path": ".git/y", "content": "n"}),
		providers.Response{Content: "done", RawStopReason: "stop"},
	)
	// Deletion happens out-of-band via a wrapper is awkward; remove before final turn using a hook provider.
	res := RunTask(context.Background(), fcSpec(), pol, delProvider{mock, filepath.Join(ws, "del.txt")}, trace.NewWriter(&nopW{}))
	want := FilesChanged{Added: []string{"new.txt"}, Modified: []string{"edit.txt"}, Deleted: []string{"del.txt"}}
	if !reflect.DeepEqual(res.FilesChanged, want) {
		t.Fatalf("got %+v want %+v", res.FilesChanged, want)
	}
}

func TestFilesChangedUnchangedEmptyLists(t *testing.T) {
	ws := t.TempDir()
	pol := policy.DefaultPolicyConfig(ws)
	mock := providers.NewScriptedMockProvider(providers.Response{Content: "ok", RawStopReason: "stop"})
	res := RunTask(context.Background(), fcSpec(), pol, mock, trace.NewWriter(&nopW{}))
	if res.FilesChanged.Added == nil || res.FilesChanged.Modified == nil || res.FilesChanged.Deleted == nil ||
		len(res.FilesChanged.Added)+len(res.FilesChanged.Modified)+len(res.FilesChanged.Deleted) != 0 || res.FilesChanged.Truncated {
		t.Fatalf("got %+v", res.FilesChanged)
	}
}

func TestFilesChangedResumeUsesCheckpointBaseline(t *testing.T) {
	ws := t.TempDir()
	pol := policy.DefaultPolicyConfig(ws)
	var cp Checkpoint
	mock := providers.NewScriptedMockProvider(
		toolCallResp("1", "write_file", map[string]any{"path": "a.txt", "content": "n"}),
		providers.Response{Content: "ok", RawStopReason: "stop"},
	)
	opts := RunOptions{OnCheckpoint: func(c Checkpoint) {
		if cp.Baseline == nil {
			cp = c
		}
	}}
	RunTaskWithOptions(context.Background(), fcSpec(), pol, mock, trace.NewWriter(&nopW{}), opts)
	if cp.Baseline == nil {
		t.Fatal("checkpoint lacks baseline")
	}
	// a.txt exists now; resume must still report it as added (baseline predates it).
	res := RunTaskWithOptions(context.Background(), fcSpec(), pol,
		providers.NewScriptedMockProvider(providers.Response{Content: "ok", RawStopReason: "stop"}),
		trace.NewWriter(&nopW{}), RunOptions{Resume: &cp})
	if !reflect.DeepEqual(res.FilesChanged.Added, []string{"a.txt"}) {
		t.Fatalf("got %+v", res.FilesChanged)
	}
	// Old checkpoint without baseline: snapshot at resume time -> no changes.
	cp.Baseline = nil
	res = RunTaskWithOptions(context.Background(), fcSpec(), pol,
		providers.NewScriptedMockProvider(providers.Response{Content: "ok", RawStopReason: "stop"}),
		trace.NewWriter(&nopW{}), RunOptions{Resume: &cp})
	if len(res.FilesChanged.Added) != 0 {
		t.Fatalf("got %+v", res.FilesChanged)
	}
}

func TestDiffSnapshotsTruncation(t *testing.T) {
	base := &Snapshot{Files: map[string]FileStat{}}
	cur := &Snapshot{Files: map[string]FileStat{}}
	for i := 0; i < MaxFilesChanged+50; i++ {
		cur.Files[filepath.Join("d", string(rune('a'+i%26)), time.Duration(i).String())] = FileStat{}
	}
	fc := DiffSnapshots(base, cur)
	if n := len(fc.Added) + len(fc.Modified) + len(fc.Deleted); n != MaxFilesChanged || !fc.Truncated {
		t.Fatalf("n=%d truncated=%v", n, fc.Truncated)
	}
}

type nopW struct{}

func (*nopW) Write(b []byte) (int, error) { return len(b), nil }

// delProvider removes a file on the first Chat call, then delegates.
type delProvider struct {
	providers.Provider
	path string
}

func (d delProvider) Chat(ctx context.Context, p compact.PrefixHeader, m []compact.Message, n int) (providers.Response, error) {
	os.Remove(d.path)
	return d.Provider.Chat(ctx, p, m, n)
}

func TestFilesChangedExcludesLoomOwnTraceFiles(t *testing.T) {
	ws := t.TempDir()
	tracePath := filepath.Join(ws, "run.jsonl")
	pol := policy.DefaultPolicyConfig(ws)
	touch := func() {
		for _, n := range []string{"run.jsonl", "run.jsonl.ckpt", "run.jsonl.lock", ".ckpt-123"} {
			os.WriteFile(filepath.Join(ws, n), []byte(time.Now().String()), 0o644)
		}
	}
	mock := providers.NewScriptedMockProvider(
		toolCallResp("1", "write_file", map[string]any{"path": "real.txt", "content": "n"}),
		providers.Response{Content: "done", RawStopReason: "stop"},
	)
	touch()
	res := RunTaskWithOptions(context.Background(), fcSpec(), pol, touchProvider{mock, touch}, trace.NewWriter(&nopW{}), RunOptions{TracePath: tracePath})
	want := FilesChanged{Added: []string{"real.txt"}, Modified: []string{}, Deleted: []string{}}
	if !reflect.DeepEqual(res.FilesChanged, want) {
		t.Fatalf("got %+v want %+v", res.FilesChanged, want)
	}
}

type touchProvider struct {
	providers.Provider
	fn func()
}

func (p touchProvider) Chat(ctx context.Context, h compact.PrefixHeader, m []compact.Message, n int) (providers.Response, error) {
	p.fn()
	return p.Provider.Chat(ctx, h, m, n)
}
