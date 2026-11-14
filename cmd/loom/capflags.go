package main

import (
	"flag"
	"fmt"
	"strconv"
	"strings"

	"github.com/choilab-jefferson/nexus.loom/pkg/engine"
)

// capabilityFlags registers the capability-restriction flags shared by run and prompt mode.
type capabilityFlags struct {
	tools         *multiStringFlag
	readGlobs     *multiStringFlag
	writeGlobs    *multiStringFlag
	maxDisallowed *optionalIntFlag
}

func addCapabilityFlags(fs *flag.FlagSet) capabilityFlags {
	var tools multiStringFlag
	fs.Var(&tools, "tools", "expose only these built-in tools: NAME[,NAME] (repeatable; overrides task.json constraints.tools; default all)")
	var readGlobs, writeGlobs multiStringFlag
	fs.Var(&readGlobs, "read-glob", "file tools may read only matching workspace paths (repeatable; overrides constraints.read_globs; default **; does not confine run_command)")
	fs.Var(&writeGlobs, "write-glob", "file tools may write only matching workspace paths (repeatable; overrides constraints.write_globs; default **; does not confine run_command)")
	maxDisallowed := &optionalIntFlag{}
	fs.Var(maxDisallowed, "max-disallowed-tool-requests", "end with constraint_failure (exit 1) once one tool is refused more than N times (not exposed or not allowlisted); unset = no limit")
	return capabilityFlags{tools: &tools, readGlobs: &readGlobs, writeGlobs: &writeGlobs, maxDisallowed: maxDisallowed}
}

// apply overlays explicitly-set capability flags onto the task constraints.
func (c capabilityFlags) apply(spec *engine.TaskSpec) {
	if names := splitList(*c.tools); len(names) > 0 {
		spec.Constraints.Tools = names
	}
	if len(*c.readGlobs) > 0 {
		spec.Constraints.ReadGlobs = append([]string(nil), *c.readGlobs...)
	}
	if len(*c.writeGlobs) > 0 {
		spec.Constraints.WriteGlobs = append([]string(nil), *c.writeGlobs...)
	}
	if c.maxDisallowed.v != nil {
		spec.Constraints.MaxDisallowedToolRequests = c.maxDisallowed.v
	}
}

// splitList flattens repeated comma-separated flag values, trimming blanks and duplicates.
func splitList(raw []string) []string {
	var out []string
	for _, r := range raw {
		for _, v := range strings.Split(r, ",") {
			v = strings.TrimSpace(v)
			if v != "" && !containsStr(out, v) {
				out = append(out, v)
			}
		}
	}
	return out
}

// optionalIntFlag is a non-negative integer flag that remembers whether it was set.
type optionalIntFlag struct{ v *int }

func (o *optionalIntFlag) String() string {
	if o == nil || o.v == nil {
		return ""
	}
	return strconv.Itoa(*o.v)
}

func (o *optionalIntFlag) Set(s string) error {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return fmt.Errorf("want an integer >= 0")
	}
	o.v = &n
	return nil
}
