package main

import (
	"fmt"
	"io"
	"os"
)

// Set at build time via -ldflags "-X main.version=... -X main.commit=...".
var (
	version = "dev"
	commit  = "unknown"
)

func versionString() string {
	return fmt.Sprintf("loom %s (commit %s)", version, commit)
}

func runVersion(w io.Writer) int {
	fmt.Fprintln(w, versionString())
	return 0
}

func runVersionCmd() int { return runVersion(os.Stdout) }
