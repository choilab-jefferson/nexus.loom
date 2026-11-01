package main

import (
	"errors"
	"fmt"
	"path/filepath"
)

// acquireRunLocks locks the workspace directory and (when non-empty) the trace
// file for the whole run. On conflict it returns a one-line error.
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
	r, err := flockPath(workspace, false)
	if errors.Is(err, errLocked) {
		return nil, fmt.Errorf("loom: workspace %s is in use by another loom process (parallel runs need distinct --workspace and --trace)", abs(workspace))
	} else if err != nil {
		return nil, fmt.Errorf("loom: lock workspace %s: %v", abs(workspace), err)
	}
	rels = append(rels, r)
	if trace != "" {
		r, err := flockPath(trace, true)
		if errors.Is(err, errLocked) {
			release()
			return nil, fmt.Errorf("loom: trace %s is in use by another loom process (parallel runs need distinct --workspace and --trace)", abs(trace))
		} else if err != nil {
			release()
			return nil, fmt.Errorf("loom: lock trace %s: %v", abs(trace), err)
		}
		rels = append(rels, r)
	}
	return release, nil
}
