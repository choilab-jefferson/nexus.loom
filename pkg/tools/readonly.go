package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/choilab-jefferson/nexus.loom/pkg/policy"
)

const (
	maxReadonlyOutput      = 1 << 18 // 256 KiB buffered before the usual max_output_bytes clamp
	defaultReadonlyTimeout = 30
)

// readonlyBaseEnv are the only inherited names a read-only child gets.
var readonlyBaseEnv = map[string]bool{
	"PATH": true, "HOME": true, "LANG": true, "LANGUAGE": true, "TZ": true, "XDG_CONFIG_HOME": true,
}

// readonlyEnv builds the minimal child env: PATH/HOME/locale, GITLAB_*/GLAB_*
// names only when the host listed them in pass_env (exact names, or PREFIX*), and
// fixed settings that keep git from prompting, locking the index, running
// fsmonitor or hooks, and glab from prompting or checking for updates.
func readonlyEnv(env []string, passEnv []string, ws string) []string {
	out := make([]string, 0, 16)
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		switch {
		case readonlyBaseEnv[key], strings.HasPrefix(key, "LC_"):
			out = append(out, kv)
		case (strings.HasPrefix(key, "GITLAB_") || strings.HasPrefix(key, "GLAB_")) && envPassed(key, passEnv):
			out = append(out, kv)
		}
	}
	return append(out,
		"TERM=dumb", "NO_COLOR=1", "NO_PROMPT=1", "GLAB_CHECK_UPDATE=false",
		"GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "GIT_PAGER=cat",
		// git must not discover a repository above the workspace.
		"GIT_CEILING_DIRECTORIES="+filepath.Dir(ws), "GIT_DISCOVERY_ACROSS_FILESYSTEM=0",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_ATTR_NOSYSTEM=1",
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=core.fsmonitor", "GIT_CONFIG_VALUE_0=false",
		"GIT_CONFIG_KEY_1=core.hooksPath", "GIT_CONFIG_VALUE_1=/dev/null",
	)
}

// execReadonly runs one allowlisted read-only git/glab argv with cwd = the task
// workspace, no shell, a minimal env, a timeout and an output cap.
func execReadonly(ctx context.Context, args map[string]any, cfg *policy.PolicyConfig) (string, *runMeta, error) {
	argv := getStringSlice(args, "argv")
	resolved, execArgv, err := policy.ValidateReadonlyArgv(argv, cfg)
	if err != nil {
		return "", nil, err
	}
	wsReal, err := policy.ResolveScopedPath(cfg.Workspace, ".", false, cfg)
	if err != nil {
		return "", nil, err
	}
	timeoutSec := getInt(args, "timeout_s", defaultReadonlyTimeout)
	if timeoutSec <= 0 {
		timeoutSec = defaultReadonlyTimeout
	}
	timeoutSec = clampRunTimeout(timeoutSec, cfg)
	env := readonlyEnv(os.Environ(), cfg.PassEnv, wsReal)
	if argv[0] == "git" {
		if err := checkRepoConfig(ctx, resolved, execArgv, wsReal, env); err != nil {
			return "", nil, err
		}
		if err := checkRepoRoot(ctx, resolved, execArgv, wsReal, env, cfg.ReadonlyGitCommonDirs); err != nil {
			return "", nil, err
		}
	}
	return runChild(ctx, resolved, execArgv, wsReal, env, timeoutSec, maxReadonlyOutput)
}

// checkRepoConfig reads the repository's effective config with `git config
// --list -z --no-includes` (parsing only; this runs no filter, hook or pager)
// and refuses when it defines anything that could execute a program. System and
// global config are already disabled by the env. A workspace should also deny
// writes to .git/** so the model cannot plant such config between this check
// and the command (loom's own write tools already deny .git/**).
func checkRepoConfig(ctx context.Context, resolved string, execArgv []string, dir string, env []string) error {
	argv := append([]string{"git"}, policy.GitGlobalArgs(execArgv)...)
	argv = append(argv, "config", "--list", "-z", "--no-includes")
	// Drop our injected GIT_CONFIG_* overrides so the list shows only what the
	// repository itself defines (the overrides still apply to the real command).
	chk := make([]string, 0, len(env))
	for _, kv := range env {
		if !strings.HasPrefix(kv, "GIT_CONFIG_COUNT=") && !strings.HasPrefix(kv, "GIT_CONFIG_KEY_") && !strings.HasPrefix(kv, "GIT_CONFIG_VALUE_") {
			chk = append(chk, kv)
		}
	}
	out, meta, err := runChild(ctx, resolved, argv, dir, chk, 10, 1<<16)
	if err != nil {
		return err
	}
	if meta.exitCode != 0 || meta.timedOut || meta.truncated {
		return fmt.Errorf("run_readonly: cannot verify repository git config (%s)", firstLine(out))
	}
	return policy.CheckGitConfigList(out)
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	if len(line) > 160 {
		line = line[:160]
	}
	return line
}

// checkRepoRoot requires the repository git resolves from the workspace to be
// the workspace's own: --show-toplevel must be inside the workspace and
// --git-common-dir must be inside it too, or exactly one of the common dirs the
// task passed in constraints.readonly_git_common_dirs (a linked worktree of the
// project's own repo). A workspace that is a plain subdirectory of a larger
// repo is refused (GIT_CEILING_DIRECTORIES already stops discovery there), so
// `git show HEAD:path` / `git log --all` cannot read a parent repository.
func checkRepoRoot(ctx context.Context, resolved string, execArgv []string, ws string, env []string, commonOK []string) error {
	argv := append([]string{"git"}, policy.GitGlobalArgs(execArgv)...)
	argv = append(argv, "rev-parse", "--show-toplevel", "--git-common-dir")
	out, meta, err := runChild(ctx, resolved, argv, ws, env, 10, 1<<14)
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if meta.exitCode != 0 || meta.timedOut || len(lines) < 2 {
		return policy.ReadonlyDenyf("the workspace is not the top of its own git repository (%s)", firstLine(out))
	}
	top, common := strings.TrimSpace(lines[0]), strings.TrimSpace(lines[len(lines)-1])
	if !filepath.IsAbs(common) {
		common = filepath.Join(ws, common)
	}
	if !policy.PathInsideWorkspace(ws, top) {
		return policy.ReadonlyDenyf("git work tree %q is outside the task workspace", top)
	}
	if policy.PathInsideWorkspace(ws, common) {
		return nil
	}
	realCommon, rerr := filepath.EvalSymlinks(common)
	for _, ok := range commonOK {
		if real, e := filepath.EvalSymlinks(ok); rerr == nil && e == nil && real == realCommon {
			return nil
		}
	}
	return policy.ReadonlyDenyf("git common dir %q is outside the task workspace (a linked worktree needs constraints.readonly_git_common_dirs)", common)
}
