package policy

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Read-only command allowlist for the run_readonly tool. Unlike run_command's
// prefix allowlist this is exact: argv[0] must be a bare "git" or "glab", the
// subcommand must be one of the listed read-only verbs, and every flag is
// checked. No shell is involved; argv is exec'd directly. Denials are
// recoverable tool errors (KindAllowlist), like run_command allowlist misses.

// ReadonlySupported lists the executables run_readonly can ever enable.
var ReadonlySupported = []string{"git", "glab"}

// ReadonlyAllowlist describes the allowed argv shapes per executable, for
// `loom capabilities --json` and docs. Informational; validation is in code.
func ReadonlyAllowlist() map[string][]string {
	return map[string][]string{
		"git": {"log", "show", "diff", "status", "branch --list", "rev-parse", "ls-files", "blame"},
		"glab": {"mr view", "mr list", "mr diff", "issue view", "issue list",
			"ci status", "ci list", "api <path> (GET only, path-prefix allowlist)"},
	}
}

func roDeny(format string, a ...any) error {
	return &ViolationError{Reason: "run_readonly: " + fmt.Sprintf(format, a...), Kind: KindAllowlist}
}

// ValidateReadonlyArgv checks argv against the read-only allowlist enabled by
// cfg.ReadonlyCommands. It returns the resolved executable path and the argv to
// exec (argv[0] kept as the bare name; git gets --no-ext-diff --no-textconv
// appended for log/show/diff/blame).
func ValidateReadonlyArgv(argv []string, cfg *PolicyConfig) (string, []string, error) {
	if len(argv) == 0 {
		return "", nil, roDeny("empty argv")
	}
	bin := argv[0]
	if bin != "git" && bin != "glab" {
		return "", nil, roDeny("executable %q is not allowed (only bare git and glab)", bin)
	}
	if cfg == nil || !containsString(cfg.ReadonlyCommands, bin) {
		return "", nil, roDeny("%s is not enabled (task constraints.readonly_commands)", bin)
	}
	for _, a := range argv {
		if strings.ContainsRune(a, 0) {
			return "", nil, roDeny("NUL byte in argument")
		}
	}
	ws, err := ResolveScopedPath(cfg.Workspace, ".", false, cfg)
	if err != nil {
		return "", nil, err
	}
	var rest []string
	switch bin {
	case "git":
		rest, err = validateGit(argv[1:], ws)
	default:
		rest, err = validateGlab(argv[1:], cfg.ReadonlyGlabGroups)
	}
	if err != nil {
		return "", nil, err
	}
	resolved, err := ResolveArgv0(bin, ws)
	if err != nil {
		return "", nil, fmt.Errorf("cannot resolve executable %q: %v", bin, err)
	}
	if insideDir(resolved, ws) {
		return "", nil, roDeny("%s resolves inside the workspace (%s)", bin, resolved)
	}
	return resolved, append([]string{bin}, rest...), nil
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// longFlagForbidden reports whether a long flag name (without "=value") equals
// or is a prefix of (git accepts unambiguous abbreviations) a forbidden name.
func longFlagForbidden(name string, forbidden []string) bool {
	if len(name) < 3 {
		return true // "--" is handled by callers; a bare "--x" abbreviation is too ambiguous
	}
	for _, f := range forbidden {
		if strings.HasPrefix(f, name) {
			return true
		}
	}
	return false
}

// gitForbiddenLong are long options refused for every git subcommand: they
// write files, run external programs, read outside the repo, or re-point it.
var gitForbiddenLong = []string{
	"--output", "--exec", "--exec-path", "--ext-diff", "--textconv", "--no-index",
	"--git-dir", "--work-tree", "--contents", "--ignore-revs-file", "--orderfile",
	"--open-files-in-pager", "--resolve-git-dir", "--config", "--show-signature",
	"--upload-pack", "--receive-pack", "--exclude-from", "--exclude-per-directory",
}

func validateGit(args []string, ws string) ([]string, error) {
	var global []string
	i := 0
	for i < len(args) && strings.HasPrefix(args[i], "-") {
		a := args[i]
		switch {
		case a == "--no-pager":
			global = append(global, a)
			i++
		case a == "--git-dir" || a == "--work-tree":
			if i+1 >= len(args) {
				return nil, roDeny("%s needs a value", a)
			}
			if err := insideWorkspace(ws, args[i+1]); err != nil {
				return nil, err
			}
			global = append(global, a, args[i+1])
			i += 2
		case strings.HasPrefix(a, "--git-dir=") || strings.HasPrefix(a, "--work-tree="):
			_, v, _ := strings.Cut(a, "=")
			if err := insideWorkspace(ws, v); err != nil {
				return nil, err
			}
			global = append(global, a)
			i++
		default:
			return nil, roDeny("git global option %q is not allowed (-C and -c are refused; only --no-pager and --git-dir/--work-tree inside the workspace)", a)
		}
	}
	if i >= len(args) {
		return nil, roDeny("missing git subcommand")
	}
	sub, rest := args[i], args[i+1:]
	switch sub {
	case "log", "show", "diff", "status", "rev-parse", "ls-files", "blame":
		if err := checkGitArgs(sub, rest, ws); err != nil {
			return nil, err
		}
	case "branch":
		if err := checkGitBranch(rest); err != nil {
			return nil, err
		}
		if err := checkGitArgs(sub, positionalsOnly(rest), ws); err != nil {
			return nil, err
		}
	default:
		return nil, roDeny("git subcommand %q is not in the read-only allowlist", sub)
	}
	out := append(global, sub)
	if sub == "log" || sub == "show" || sub == "diff" || sub == "blame" {
		// Appended after the model's flags (before any "--") so the model cannot
		// re-enable external diff or textconv drivers.
		inj := []string{"--no-ext-diff", "--no-textconv"}
		cut := len(rest)
		for k, a := range rest {
			if a == "--" {
				cut = k
				break
			}
		}
		out = append(out, rest[:cut]...)
		out = append(out, inj...)
		out = append(out, rest[cut:]...)
		return out, nil
	}
	return append(out, rest...), nil
}

// checkGitArgs validates every argument of a read-only git subcommand.
//
// Flags: forbidden/abbreviated long flags, -c, -o, -O*, -X*, and blame -S are
// refused. Git short-flag clusters (e.g. "-co") are deliberately NOT decomposed:
// the only short options that write, execute or read arbitrary files are -c
// (global only, parsed before the subcommand), -o (exact), -O<file> and
// -X<param> (prefix-checked), so a cluster cannot smuggle one in; clusters of
// harmless display flags are fine.
//
// Non-flag arguments (revisions, pathspecs, and values of value-taking flags)
// all go through checkPathLike, including everything after "--": an argument
// that could name a file must resolve inside the workspace. This is what stops
// `git diff /etc/passwd /dev/null` (implicit --no-index) from reading outside.
func checkGitArgs(sub string, rest []string, ws string) error {
	afterSep := false
	for _, a := range rest {
		if afterSep {
			if err := checkPathLike(ws, a); err != nil {
				return err
			}
			continue
		}
		if a == "--" {
			afterSep = true
			continue
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			if err := checkPathLike(ws, a); err != nil {
				return err
			}
			continue
		}
		switch {
		case a == "-c":
			return roDeny("git -c (config override) is not allowed")
		case a == "-o":
			return roDeny("git -o is not allowed")
		case strings.HasPrefix(a, "-O") && !strings.HasPrefix(a, "--"):
			return roDeny("git %s is not allowed (reads an order file)", a)
		case strings.HasPrefix(a, "-X") && !strings.HasPrefix(a, "--"):
			return roDeny("git %s is not allowed (reads an exclude file / dirstat param)", a)
		case sub == "blame" && a == "-S":
			return roDeny("git blame -S is not allowed (reads a revs file)")
		case strings.HasPrefix(a, "--"):
			name, val, hasVal := strings.Cut(a, "=")
			if sub == "rev-parse" && a == "--git-dir" {
				continue // query only: prints the path
			}
			if longFlagForbidden(name, gitForbiddenLong) {
				return roDeny("git flag %q is not allowed", a)
			}
			if hasVal {
				if err := checkPathLike(ws, val); err != nil {
					return err
				}
			}
		default: // short flag with an attached value, e.g. -L1,5:file
			if len(a) > 2 {
				if err := checkPathLike(ws, a[2:]); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// checkPathLike refuses an argument that could name a file or tree outside the
// task workspace: ".." segments (also after ':' as in rev:path), magic
// pathspecs ":(...)" and ":/..." (top/absolute), and absolute or relative
// paths whose symlink-resolved target leaves the workspace (so /dev/null and
// /etc/passwd are refused). Anything else is a revision or plain value that git
// cannot resolve to a file outside the repository.
func checkPathLike(ws, a string) error {
	if strings.HasPrefix(a, ":(") || strings.HasPrefix(a, ":/") {
		return roDeny("magic pathspec %q is not allowed", a)
	}
	for _, seg := range strings.FieldsFunc(a, func(r rune) bool { return r == '/' || r == ':' }) {
		if seg == ".." {
			return roDeny("argument %q contains a '..' path segment", a)
		}
	}
	if filepath.IsAbs(a) {
		return insideWorkspace(ws, a)
	}
	if a == "" || strings.HasPrefix(a, "-") {
		return nil
	}
	if _, err := os.Lstat(filepath.Join(ws, a)); err == nil {
		return insideWorkspace(ws, a)
	}
	return nil
}

// GitGlobalArgs returns the leading global options (already validated) of an
// exec argv returned by ValidateReadonlyArgv, argv[0] excluded.
func GitGlobalArgs(execArgv []string) []string {
	var out []string
	for i := 1; i < len(execArgv) && strings.HasPrefix(execArgv[i], "-"); i++ {
		out = append(out, execArgv[i])
		if (execArgv[i] == "--git-dir" || execArgv[i] == "--work-tree") && i+1 < len(execArgv) {
			i++
			out = append(out, execArgv[i])
		}
	}
	return out
}

// CheckGitConfigList inspects the output of `git config --list -z --no-includes`
// and refuses repositories whose own config can make a read-only command execute
// a program: filter.* (clean/smudge run on `status`/`diff`), diff.external and
// diff.*.command|textconv, core.fsmonitor/hooksPath/pager/sshCommand/askPass,
// gpg program settings, core.worktree, core.bare=true, extensions.worktreeConfig and any include/includeIf (which could pull in the rest).
// Aliases are not refused: git never lets an alias shadow a builtin, and every
// allowed subcommand is a builtin. Pager settings are inert (stdout is a pipe,
// GIT_PAGER=cat).
func CheckGitConfigList(out string) error {
	for _, rec := range strings.Split(out, "\x00") {
		key, val, _ := strings.Cut(rec, "\n")
		if bad := dangerousGitConfigKey(strings.ToLower(strings.TrimSpace(key)), strings.ToLower(strings.TrimSpace(val))); bad != "" {
			return roDeny("repository git config key %q can run external programs (%s); refusing git commands", key, bad)
		}
	}
	return nil
}

func dangerousGitConfigKey(k, v string) string {
	switch {
	case k == "":
		return ""
	case strings.HasPrefix(k, "filter."):
		return "filter driver"
	case k == "diff.external":
		return "external diff"
	case strings.HasPrefix(k, "diff.") && (strings.HasSuffix(k, ".command") || strings.HasSuffix(k, ".textconv")):
		return "diff driver"
	case k == "core.fsmonitor", k == "core.hookspath", k == "core.pager", k == "core.sshcommand", k == "core.askpass", k == "core.editor":
		return "program setting"
	case k == "gpg.program" || (strings.HasPrefix(k, "gpg.") && strings.HasSuffix(k, ".program")):
		return "gpg program"
	case k == "core.worktree":
		return "redirects the work tree"
	case k == "core.bare" && v != "false" && v != "no" && v != "off" && v != "0":
		return "bare repository"
	case k == "extensions.worktreeconfig" && v != "false" && v != "no" && v != "off" && v != "0":
		return "per-worktree config (config.worktree)"
	case strings.HasPrefix(k, "include.") || strings.HasPrefix(k, "includeif."):
		return "include"
	}
	return ""
}

// gitBranchFlags are the only flags accepted by `git branch --list`.
var gitBranchFlags = map[string]bool{
	"--list": true, "-a": true, "--all": true, "-r": true, "--remotes": true,
	"-v": true, "-vv": true, "--verbose": true, "--show-current": true,
	"--no-color": true, "--color": true, "--contains": true, "--no-contains": true,
	"--merged": true, "--no-merged": true, "--points-at": true, "--sort": true,
	"--format": true, "-i": true, "--ignore-case": true, "--column": true,
	"--no-column": true, "--abbrev": true, "--no-abbrev": true,
}

// positionalsOnly drops flag tokens (branch flags are whitelisted separately).
func positionalsOnly(rest []string) []string {
	var out []string
	for _, a := range rest {
		if !strings.HasPrefix(a, "-") {
			out = append(out, a)
		}
	}
	return out
}

func checkGitBranch(rest []string) error {
	hasList := false
	for _, a := range rest {
		if !strings.HasPrefix(a, "-") {
			continue // pattern / commit-ish (only meaningful with --list)
		}
		name, _, _ := strings.Cut(a, "=")
		if !gitBranchFlags[name] {
			return roDeny("git branch flag %q is not allowed (only --list and display filters)", a)
		}
		if a == "--list" {
			hasList = true
		}
	}
	if !hasList {
		return roDeny("git branch requires --list (branch creation/deletion is not allowed)")
	}
	return nil
}

// insideWorkspace requires p (resolved against ws, symlinks evaluated) to stay
// inside the workspace. Unlike ResolveScopedPath it does not apply deny globs,
// since --git-dir=.git is legitimate.
func insideWorkspace(ws, p string) error {
	if strings.TrimSpace(p) == "" {
		return roDeny("empty path")
	}
	cand := p
	if !filepath.IsAbs(cand) {
		cand = filepath.Join(ws, cand)
	}
	real, err := evalSymlinksWalk(filepath.Clean(cand))
	if err != nil {
		return roDeny("cannot resolve path %q: %v", p, err)
	}
	if !insideDir(real, ws) {
		return roDeny("path %q escapes the task workspace", p)
	}
	return nil
}

var glabRepoRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*/[A-Za-z0-9_][A-Za-z0-9_.-]*(/[A-Za-z0-9_][A-Za-z0-9_.-]*)?$`)

// glabRepoOK accepts exactly OWNER/REPO or GROUP/SUBGROUP/REPO, always on the
// host glab is configured for (workspace remote or GITLAB_HOST, chosen by the
// host operator). A host-looking first segment (contains '.') is refused so a
// model can neither pick another host nor smuggle HOST/OWNER/REPO.
//
// The three-segment form is accepted only when its first segment (the group) is
// listed in the task's constraints.readonly_glab_groups.
func glabRepoOK(v string, groups []string) bool {
	if !glabRepoRe.MatchString(v) {
		return false
	}
	first, _, _ := strings.Cut(v, "/")
	if strings.Contains(first, ".") {
		return false
	}
	if strings.Count(v, "/") == 2 {
		return containsString(groups, first)
	}
	return true
}

var glabVerbs = map[string]bool{
	"mr view": true, "mr list": true, "mr diff": true,
	"issue view": true, "issue list": true,
	"ci status": true, "ci list": true, // "ci view" is a TUI: not offered
}

func validateGlab(args []string, groups []string) ([]string, error) {
	if len(args) == 0 {
		return nil, roDeny("missing glab subcommand")
	}
	if args[0] == "api" {
		return args, validateGlabAPI(args[1:])
	}
	if len(args) < 2 || !glabVerbs[args[0]+" "+args[1]] {
		return nil, roDeny("glab subcommand %q is not in the read-only allowlist", strings.Join(args[:min(2, len(args))], " "))
	}
	rest := args[2:]
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		switch {
		case a == "--web" || a == "--hostname" || strings.HasPrefix(a, "--hostname="):
			return nil, roDeny("glab flag %q is not allowed", a)
		case a == "-R" || a == "--repo":
			if i+1 >= len(rest) || !glabRepoOK(rest[i+1], groups) {
				return nil, roDeny("glab %s needs OWNER/REPO or GROUP/SUBGROUP/REPO without a host", a)
			}
			i++
		case strings.HasPrefix(a, "--repo="):
			if !glabRepoOK(strings.TrimPrefix(a, "--repo="), groups) {
				return nil, roDeny("glab --repo needs OWNER/REPO or GROUP/SUBGROUP/REPO without a host")
			}
		case strings.HasPrefix(a, "-R"):
			if !glabRepoOK(strings.TrimPrefix(strings.TrimPrefix(a, "-R"), "="), groups) {
				return nil, roDeny("glab -R needs OWNER/REPO or GROUP/SUBGROUP/REPO without a host")
			}
		case strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.ContainsRune(a, 'w'):
			return nil, roDeny("glab short flag cluster %q may contain -w (--web)", a)
		}
	}
	return args, nil
}

func validateGlabAPI(rest []string) error {
	path := ""
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		switch {
		case a == "-X" || a == "--method":
			if i+1 >= len(rest) || !strings.EqualFold(rest[i+1], "GET") {
				return roDeny("glab api %s must be GET", a)
			}
			i++
		case strings.HasPrefix(a, "--method="):
			if !strings.EqualFold(strings.TrimPrefix(a, "--method="), "GET") {
				return roDeny("glab api --method must be GET")
			}
		case strings.HasPrefix(a, "-X") && !strings.HasPrefix(a, "--"):
			if !strings.EqualFold(strings.TrimPrefix(a[2:], "="), "GET") {
				return roDeny("glab api -X must be GET")
			}
		case a == "--paginate" || a == "-i" || a == "--include" || a == "--silent":
		case strings.HasPrefix(a, "-"):
			return roDeny("glab api flag %q is not allowed (GET only: no fields, input, headers or hostname)", a)
		default:
			if path != "" {
				return roDeny("glab api takes exactly one path")
			}
			path = a
		}
	}
	if path == "" {
		return roDeny("glab api needs a path")
	}
	return checkGlabAPIPath(path)
}

var (
	glabProjectIDRe = regexp.MustCompile(`^([0-9]+|[A-Za-z0-9_][A-Za-z0-9_.-]*(%2[Ff][A-Za-z0-9_][A-Za-z0-9_.-]*)+)$`)
	// First path segment after projects/:id or groups/:id.
	glabProjectSub = map[string]bool{
		"merge_requests": true, "issues": true, "pipelines": true, "jobs": true,
		"repository": true, "labels": true, "milestones": true,
	}
	glabGroupSub = map[string]bool{"merge_requests": true, "issues": true, "labels": true, "milestones": true}
	glabRepoSub  = map[string]bool{"commits": true, "branches": true, "tags": true, "compare": true, "files": true, "tree": true}
	glabDenySeg  = map[string]bool{
		"variables": true, "deploy_tokens": true, "deploy_keys": true, "runners": true, "hooks": true,
		"access_tokens": true, "members": true, "user": true, "users": true, "personal_access_tokens": true,
		"trace": true, "artifacts": true, "secure_files": true, "graphql": true, "api": true,
	}
	glabDenyQuery = map[string]bool{"private_token": true, "access_token": true, "job_token": true, "sudo": true}
)

// checkGlabAPIPath enforces a GET path-prefix allowlist so read-only api calls
// cannot reach secret-bearing endpoints (variables, tokens, keys, runners,
// hooks, members, user, job traces/artifacts). Allowed shapes:
//
//	projects/:id/{merge_requests,issues,pipelines,jobs,labels,milestones}[/...]
//	projects/:id/repository/{commits,branches,tags,compare,files,tree}[/...]
//	groups/:id/{merge_requests,issues,labels,milestones}[/...]
//
// :id is numeric or a URL-encoded namespace/path (group%2Fproject). The bare
// project object, the user, and anything else are refused.
func checkGlabAPIPath(path string) error {
	if strings.Contains(path, "://") || strings.HasPrefix(path, "/") || strings.Contains(path, "\\") {
		return roDeny("glab api path must be a relative endpoint like projects/ID/merge_requests")
	}
	p, query, _ := strings.Cut(path, "?")
	if query != "" {
		q, err := url.ParseQuery(query)
		if err != nil {
			return roDeny("glab api query is malformed")
		}
		for k := range q {
			if glabDenyQuery[strings.ToLower(k)] {
				return roDeny("glab api query parameter %q is not allowed", k)
			}
		}
	}
	segs := strings.Split(p, "/")
	for i, sg := range segs {
		dec, err := url.PathUnescape(sg)
		if err != nil || sg == "" || dec == "." || dec == ".." {
			return roDeny("glab api path has an empty, dot or undecodable segment")
		}
		// Only the project/group id (segs[1]) may carry an encoded namespace
		// slash; elsewhere it could hide a denied segment (so nested
		// repository/files paths are not reachable: use git or read_file).
		if i != 1 && strings.ContainsAny(dec, `/\`) {
			return roDeny("glab api segment %q contains an encoded slash", sg)
		}
	}
	if len(segs) < 3 {
		return roDeny("glab api path %q is not in the read-only endpoint allowlist", p)
	}
	var sub map[string]bool
	switch segs[0] {
	case "projects":
		sub = glabProjectSub
	case "groups":
		sub = glabGroupSub
	default:
		return roDeny("glab api path %q is not in the read-only endpoint allowlist", p)
	}
	if !glabProjectIDRe.MatchString(segs[1]) || !sub[segs[2]] {
		return roDeny("glab api path %q is not in the read-only endpoint allowlist", p)
	}
	if segs[0] == "projects" && segs[2] == "repository" && (len(segs) < 4 || !glabRepoSub[segs[3]]) {
		return roDeny("glab api repository endpoint %q is not in the read-only endpoint allowlist", p)
	}
	for _, sg := range segs[2:] {
		dec, _ := url.PathUnescape(sg)
		if glabDenySeg[strings.ToLower(dec)] {
			return roDeny("glab api path segment %q is a secret-bearing endpoint", dec)
		}
	}
	return nil
}

// ReadonlyDenyf builds a recoverable run_readonly denial for callers outside
// this package (the exec layer's repository-root and config checks).
func ReadonlyDenyf(format string, a ...any) error { return roDeny(format, a...) }

// PathInsideWorkspace reports whether p, symlinks evaluated, lies inside ws.
func PathInsideWorkspace(ws, p string) bool {
	real, err := evalSymlinksWalk(filepath.Clean(p))
	return err == nil && insideDir(real, ws)
}
