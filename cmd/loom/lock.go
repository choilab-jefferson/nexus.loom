package main

import (
	"errors"
	"fmt"
	"path/filepath"
)

// acquireRunLocks locks the trace file (when non-empty) for the whole run. The workspace is not locked: parallel runs
// may share one workspace (a team in one project directory, PI 2026-11-13); the write tools lock each file they write
// (pkg/tools openLocked). On conflict it returns a one-line error.
func acquireRunLocks(workspace, trace string) (release func(), err error) {
	var rels []func()
	release = func() {
		for _, r := range rels {
			r()
		}
	}
	abs := func(p string) string {
		if a, e := filepath.Abs(p); e == nil {
			return a
		}
		return p
	}
	_ = workspace
	if trace != "" {
		r, err := flockPath(trace, true)
		if errors.Is(err, errLocked) {
			release()
			return nil, fmt.Errorf("loom: trace %s is in use by another loom process (parallel runs need distinct --trace)", abs(trace))
		} else if err != nil {
			release()
			return nil, fmt.Errorf("loom: lock trace %s: %v", abs(trace), err)
		}
		rels = append(rels, r)
	}
	return release, nil
}
