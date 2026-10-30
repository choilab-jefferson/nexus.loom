package main

import (
	"bytes"
	"testing"
)

func TestVersionDefault(t *testing.T) {
	var b bytes.Buffer
	if rc := runVersion(&b); rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	if got, want := b.String(), "loom dev (commit unknown)\n"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestVersionInjected(t *testing.T) {
	ov, oc := version, commit
	defer func() { version, commit = ov, oc }()
	version, commit = "v1.2.3", "abc1234"
	var b bytes.Buffer
	runVersion(&b)
	if got, want := b.String(), "loom v1.2.3 (commit abc1234)\n"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestVersionDispatch(t *testing.T) {
	if rc := runMain([]string{"version"}); rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
}
