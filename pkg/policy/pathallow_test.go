package policy

import "testing"

func TestPathAllowlistMissIsRecoverable(t *testing.T) {
	ws := t.TempDir()
	cfg := DefaultPolicyConfig(ws)
	cfg.WriteAllow = []string{"work/**"}
	_, err := ResolveScopedPath(ws, "inputs/x.csv", true, cfg)
	kind, ok := ViolationKindOf(err)
	if !ok || kind != KindPathNotAllowed || !kind.Recoverable() || kind.String() != "path_not_allowed" {
		t.Fatalf("got kind %v (ok=%v) err=%v", kind, ok, err)
	}
	// Escape and deny globs stay fatal.
	if k, _ := ViolationKindOf(func() error { _, e := ResolveScopedPath(ws, "../x", false, cfg); return e }()); k != KindFatal {
		t.Errorf("escape kind = %v, want fatal", k)
	}
	if k, _ := ViolationKindOf(func() error { _, e := ResolveScopedPath(ws, ".git/config", false, cfg); return e }()); k != KindFatal {
		t.Errorf("deny-glob kind = %v, want fatal", k)
	}
}
