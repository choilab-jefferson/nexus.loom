package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/choilab-jefferson/nexus.loom/pkg/engine"
)

// contextFlags are --context-in / --context-out (run and prompt mode).
type contextFlags struct{ in, out *string }

func addContextFlags(fs *flag.FlagSet) contextFlags {
	return contextFlags{
		in:  fs.String("context-in", "", "continue from a loom.context.v1 JSON file (\"-\" = stdin); the new -p prompt is appended"),
		out: fs.String("context-out", "", "write the updated loom.context.v1 JSON here after the run (\"-\" = stdout; result then goes to stderr)"),
	}
}

func (c contextFlags) apply(spec *engine.TaskSpec) {
	if *c.in != "" {
		spec.ContextIn = *c.in
	}
	if *c.out != "" {
		spec.ContextOut = *c.out
	}
}

// loadContext reads and validates --context-in. Errors are exit-2 material. Repairs are
// reported one per line on stderr.
func loadContext(path string) (*engine.Context, error) {
	var raw []byte
	var err error
	if path == "-" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read context %q: %v", path, err)
	}
	c, notes, err := engine.ParseContext(raw)
	if err != nil {
		return nil, err
	}
	for _, n := range notes {
		fmt.Fprintf(os.Stderr, "loom context repair: %s\n", n)
	}
	return c, nil
}

// writeContext writes the context JSON to stdout ("-") or atomically (0600) to path.
func writeContext(path string, c *engine.Context) error {
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if path == "-" {
		_, err = os.Stdout.Write(append(raw, '\n'))
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".ctx-*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(append(raw, '\n'))
	cerr := tmp.Close()
	if werr != nil || cerr != nil || os.Chmod(tmp.Name(), 0o600) != nil || os.Rename(tmp.Name(), path) != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("cannot write context %q", path)
	}
	return nil
}

// resultWriter is where TaskResult / output text go: stderr when stdout carries the context.
func resultWriter(contextOut string) io.Writer {
	if strings.TrimSpace(contextOut) == "-" {
		return os.Stderr
	}
	return os.Stdout
}
