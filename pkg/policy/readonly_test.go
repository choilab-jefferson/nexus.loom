package policy

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func roCfg(t *testing.T, enabled ...string) *PolicyConfig {
	t.Helper()
	cfg := DefaultPolicyConfig(t.TempDir())
	cfg.ReadonlyCommands = enabled
	return cfg
}

func TestReadonlyAllowed(t *testing.T) {
	cfg := roCfg(t, "git", "glab")
	cfg.ReadonlyGlabGroups = []string{"grp"}
	cases := [][]string{
		{"git", "log", "--oneline", "-n", "5"},
		{"git", "log", "-p", "--", "README.md"},
		{"git", "show", "HEAD:README.md"},
		{"git", "diff", "main...HEAD", "--stat"},
		{"git", "status", "--porcelain"},
		{"git", "branch", "--list"},
		{"git", "branch", "--list", "-a", "feat/*"},
		{"git", "rev-parse", "--git-dir"},
		{"git", "rev-parse", "HEAD"},
		{"git", "ls-files", "pkg"},
		{"git", "blame", "-L", "1,5", "README.md"},
		{"git", "--no-pager", "log", "-1"},
		{"git", "--git-dir", ".git", "log", "-1"},
		{"git", "--git-dir=.git", "--work-tree=.", "status"},
		{"glab", "mr", "view", "12"},
		{"glab", "mr", "list", "--state", "opened", "-R", "grp/proj"},
		{"glab", "mr", "diff", "12", "--repo=grp/sub/proj"},
		{"git", "diff", "HEAD~1", "--", "pkg/a.go"},
		{"git", "log", "--author", "John Smith", "--since=2.weeks"},
		{"git", "show", "HEAD:README.md"},
		{"git", "log", "main...HEAD", "--", "."},
		{"git", "ls-files", "-s"},
		{"glab", "api", "projects/acme%2Fapp/issues/12/notes"},
		{"glab", "api", "projects/12/repository/commits?ref_name=main&per_page=5"},
		{"glab", "api", "projects/12/repository/files/README.md/raw?ref=main"},
		{"glab", "api", "projects/12/pipelines/5/jobs"},
		{"glab", "api", "groups/7/merge_requests?state=opened"},
		{"glab", "issue", "view", "7", "--comments"},
		{"glab", "issue", "list"},
		{"glab", "ci", "status"},
		{"glab", "ci", "list"},
		{"glab", "api", "projects/1/merge_requests"},
		{"glab", "api", "projects/1/issues", "-X", "GET"},
		{"glab", "api", "--method=get", "projects/1/issues", "--paginate"},
		{"glab", "api", "projects/1/issues", "-XGET", "-i"},
	}
	for _, argv := range cases {
		bin, got, err := ValidateReadonlyArgv(argv, cfg)
		if err != nil {
			// The executable may be absent on a CI image; that is not a policy denial.
			if _, isPolicy := ViolationKindOf(err); isPolicy {
				t.Errorf("%v: unexpected denial: %v", argv, err)
			}
			continue
		}
		if bin == "" || got[0] != argv[0] {
			t.Errorf("%v: bad result %q %v", argv, bin, got)
		}
	}
}

func TestReadonlyDenied(t *testing.T) {
	cfg := roCfg(t, "git", "glab")
	outside := t.TempDir()
	cases := []struct {
		name string
		argv []string
	}{
		{"empty", nil},
		{"other binary", []string{"ls", "-la"}},
		{"path binary", []string{"/usr/bin/git", "log"}},
		{"shell", []string{"sh", "-c", "git log"}},
		{"git push", []string{"git", "push", "origin", "main"}},
		{"git commit", []string{"git", "commit", "-m", "x"}},
		{"git config", []string{"git", "config", "user.name", "x"}},
		{"git checkout", []string{"git", "checkout", "main"}},
		{"git reset", []string{"git", "reset", "--hard"}},
		{"git clean", []string{"git", "clean", "-fdx"}},
		{"git fetch", []string{"git", "fetch"}},
		{"git branch create", []string{"git", "branch", "newbranch"}},
		{"git branch delete", []string{"git", "branch", "-D", "x"}},
		{"git branch list+delete", []string{"git", "branch", "--list", "-d", "x"}},
		{"git branch copy", []string{"git", "branch", "--list", "-c", "a", "b"}},
		{"git no subcommand", []string{"git"}},
		{"git global -c", []string{"git", "-c", "core.pager=evil", "log"}},
		{"git global -C", []string{"git", "-C", "/", "log"}},
		{"git global exec-path", []string{"git", "--exec-path=/tmp/x", "log"}},
		{"git -c after sub", []string{"git", "log", "-c"}},
		{"git log --output", []string{"git", "log", "--output=/tmp/x"}},
		{"git diff --output sep", []string{"git", "diff", "--output", "/tmp/x"}},
		{"git diff --output abbrev", []string{"git", "diff", "--outp=/tmp/x"}},
		{"git log -o", []string{"git", "log", "-o"}},
		{"git ls-files -o", []string{"git", "ls-files", "-o"}},
		{"git diff --ext-diff", []string{"git", "diff", "--ext-diff"}},
		{"git diff --ext-diff abbrev", []string{"git", "diff", "--ext-d"}},
		{"git show --textconv", []string{"git", "show", "--textconv", "HEAD"}},
		{"git log --exec", []string{"git", "log", "--exec=id"}},
		{"git diff --no-index", []string{"git", "diff", "--no-index", "/etc/passwd", "/etc/hosts"}},
		{"git --git-dir outside", []string{"git", "--git-dir=" + outside, "log"}},
		{"git --git-dir outside sep", []string{"git", "--git-dir", outside, "log"}},
		{"git --work-tree outside", []string{"git", "--work-tree", outside, "status"}},
		{"git --git-dir dotdot", []string{"git", "--git-dir=../x", "log"}},
		{"git --git-dir after sub", []string{"git", "log", "--git-dir=.git"}},
		{"git --git-dir missing value", []string{"git", "--git-dir"}},
		{"git blame -S", []string{"git", "blame", "-S", "/etc/passwd", "f"}},
		{"git blame --contents", []string{"git", "blame", "--contents", "/etc/passwd", "f"}},
		{"git diff -O", []string{"git", "diff", "-O/etc/passwd"}},
		{"git log show-signature", []string{"git", "log", "--show-signature"}},
		{"git rev-parse --resolve-git-dir", []string{"git", "rev-parse", "--resolve-git-dir", "/"}},
		{"glab ci view TUI", []string{"glab", "ci", "view", "main"}},
		{"glab -R 4 segments", []string{"glab", "mr", "list", "-R", "a/b/c/d"}},
		{"glab -R single", []string{"glab", "mr", "list", "-R", "proj"}},
		{"ls-files -X", []string{"git", "ls-files", "-X", "ignore.txt"}},
		{"ls-files -Xfile", []string{"git", "ls-files", "-X/etc/passwd"}},
		{"ls-files --exclude-from", []string{"git", "ls-files", "--exclude-from=x"}},
		{"ls-files --exclude-from sep", []string{"git", "ls-files", "--exclude-from", "x"}},
		{"ls-files --exclude-per-directory", []string{"git", "ls-files", "--exclude-per-directory=.gitignore"}},
		{"ls-files --exclude-from abbrev", []string{"git", "ls-files", "--exclude-f=x"}},
		// exfiltration via implicit --no-index and path arguments
		{"diff /etc/passwd /dev/null", []string{"git", "diff", "/etc/passwd", "/dev/null"}},
		{"diff /dev/null /etc/passwd", []string{"git", "diff", "/dev/null", "/etc/passwd"}},
		{"diff -- outside", []string{"git", "diff", "--", "/etc/passwd", "/dev/null"}},
		{"diff outside tmp file", []string{"git", "diff", filepath.Join(outside, "secret"), "/dev/null"}},
		{"diff /proc/self/environ", []string{"git", "diff", "/proc/self/environ", "/dev/null"}},
		{"diff credentials", []string{"git", "diff", "/dev/null", "/root/.git-credentials"}},
		{"diff dotdot", []string{"git", "diff", "../a", "../b"}},
		{"diff ./dotdot", []string{"git", "diff", "./../../etc/passwd", "/dev/null"}},
		{"diff mid dotdot", []string{"git", "diff", "a/../../b", "c"}},
		{"diff dotdot after --", []string{"git", "diff", "--", "../x", "y"}},
		{"diff bare ..", []string{"git", "diff", ".."}},
		{"blame abs outside", []string{"git", "blame", "/etc/passwd"}},
		{"blame -- abs outside", []string{"git", "blame", "--", "/etc/hosts"}},
		{"log -- /proc", []string{"git", "log", "--", "/proc/self/environ"}},
		{"show abs outside", []string{"git", "show", "/etc/passwd"}},
		{"show rev:../x", []string{"git", "show", "HEAD:../x"}},
		{"ls-files abs outside", []string{"git", "ls-files", "/etc"}},
		{"status abs outside", []string{"git", "status", "/root"}},
		{"rev-parse abs outside", []string{"git", "rev-parse", "/etc/passwd"}},
		{"magic top", []string{"git", "log", "--", ":(top)secret"}},
		{"magic top no sep", []string{"git", "diff", ":(top,exclude)x"}},
		{"magic root", []string{"git", "ls-files", ":/etc"}},
		{"long opt value abs", []string{"git", "log", "--relative=/etc"}},
		{"short opt value dotdot", []string{"git", "log", "-L1,5:../../etc/passwd"}},
		{"branch pattern abs", []string{"git", "branch", "--list", "/etc/passwd"}},
		{"glab -R 3 seg unlisted group", []string{"glab", "mr", "list", "-R", "grp/sub/proj"}},
		{"glab --repo= 3 seg unlisted", []string{"glab", "mr", "list", "--repo=grp/sub/proj"}},
		{"glab api encoded slash in issues", []string{"glab", "api", "projects/1/issues/1%2Fvariables"}},
		{"glab api encoded slash in files", []string{"glab", "api", "projects/1/repository/files/a%2Fb/raw"}},
		{"glab api encoded slash lower", []string{"glab", "api", "projects/1/repository/commits/x%2fy"}},
		{"glab api encoded backslash", []string{"glab", "api", "projects/1/issues/a%5Cb"}},
		{"glab mr merge", []string{"glab", "mr", "merge", "1"}},
		{"glab mr create", []string{"glab", "mr", "create"}},
		{"glab mr close", []string{"glab", "mr", "close", "1"}},
		{"glab mr note", []string{"glab", "mr", "note", "1", "-m", "x"}},
		{"glab issue create", []string{"glab", "issue", "create", "-t", "x"}},
		{"glab issue update", []string{"glab", "issue", "update", "1"}},
		{"glab issue close", []string{"glab", "issue", "close", "1"}},
		{"glab ci run", []string{"glab", "ci", "run"}},
		{"glab ci retry", []string{"glab", "ci", "retry", "1"}},
		{"glab repo delete", []string{"glab", "repo", "delete", "x"}},
		{"glab auth", []string{"glab", "auth", "status"}},
		{"glab only group", []string{"glab", "mr"}},
		{"glab none", []string{"glab"}},
		{"glab mr view --web", []string{"glab", "mr", "view", "1", "--web"}},
		{"glab mr view -w", []string{"glab", "mr", "view", "1", "-w"}},
		{"glab mr view cluster -w", []string{"glab", "mr", "view", "1", "-cw"}},
		{"glab hostname", []string{"glab", "mr", "list", "--hostname", "evil.example"}},
		{"glab hostname=", []string{"glab", "mr", "list", "--hostname=evil.example"}},
		{"glab -R host", []string{"glab", "mr", "list", "-R", "evil.example/grp/proj"}},
		{"glab -R url", []string{"glab", "mr", "list", "-R", "https://evil.example/grp/proj"}},
		{"glab --repo= host", []string{"glab", "mr", "list", "--repo=evil.example/grp/proj"}},
		{"glab -R missing", []string{"glab", "mr", "list", "-R"}},
		{"glab api POST", []string{"glab", "api", "-X", "POST", "projects/1/issues"}},
		{"glab api DELETE", []string{"glab", "api", "--method", "DELETE", "projects/1"}},
		{"glab api --method=PUT", []string{"glab", "api", "--method=PUT", "projects/1"}},
		{"glab api -XPOST", []string{"glab", "api", "-XPOST", "projects/1"}},
		{"glab api -X missing", []string{"glab", "api", "projects/1", "-X"}},
		{"glab api -f", []string{"glab", "api", "projects/1/issues", "-f", "title=x"}},
		{"glab api -F", []string{"glab", "api", "projects/1/issues", "-F", "title=x"}},
		{"glab api --field", []string{"glab", "api", "projects/1/issues", "--field", "title=x"}},
		{"glab api --raw-field", []string{"glab", "api", "projects/1/issues", "--raw-field", "t=x"}},
		{"glab api --input", []string{"glab", "api", "projects/1/issues", "--input", "body.json"}},
		{"glab api header override", []string{"glab", "api", "projects/1", "-H", "X-HTTP-Method-Override: DELETE"}},
		{"glab api hostname", []string{"glab", "api", "projects/1", "--hostname", "evil.example"}},
		{"glab api no path", []string{"glab", "api"}},
		{"glab api two paths", []string{"glab", "api", "a", "b"}},
		{"glab api url", []string{"glab", "api", "https://evil.example/x"}},
		{"glab api graphql", []string{"glab", "api", "graphql"}},
		{"glab api user", []string{"glab", "api", "user"}},
		{"glab api personal_access_tokens", []string{"glab", "api", "personal_access_tokens"}},
		{"glab api bare project", []string{"glab", "api", "projects/1"}},
		{"glab api projects list", []string{"glab", "api", "projects"}},
		{"glab api variables", []string{"glab", "api", "projects/1/variables"}},
		{"glab api pipeline variables", []string{"glab", "api", "projects/1/pipelines/5/variables"}},
		{"glab api deploy_tokens", []string{"glab", "api", "projects/1/deploy_tokens"}},
		{"glab api deploy_keys", []string{"glab", "api", "projects/1/deploy_keys"}},
		{"glab api runners", []string{"glab", "api", "projects/1/runners"}},
		{"glab api hooks", []string{"glab", "api", "projects/1/hooks"}},
		{"glab api access_tokens", []string{"glab", "api", "projects/1/access_tokens"}},
		{"glab api members", []string{"glab", "api", "projects/1/members"}},
		{"glab api job trace", []string{"glab", "api", "projects/1/jobs/9/trace"}},
		{"glab api job artifacts", []string{"glab", "api", "projects/1/jobs/9/artifacts"}},
		{"glab api secure_files", []string{"glab", "api", "projects/1/secure_files"}},
		{"glab api repository archive", []string{"glab", "api", "projects/1/repository/archive"}},
		{"glab api repository bare", []string{"glab", "api", "projects/1/repository"}},
		{"glab api group members", []string{"glab", "api", "groups/1/members"}},
		{"glab api group variables", []string{"glab", "api", "groups/1/variables"}},
		{"glab api leading slash", []string{"glab", "api", "/projects/1/issues"}},
		{"glab api api/v4", []string{"glab", "api", "api/v4/projects/1/issues"}},
		{"glab api dotdot", []string{"glab", "api", "projects/1/issues/../variables"}},
		{"glab api encoded dotdot", []string{"glab", "api", "projects/1/issues/%2e%2e/variables"}},
		{"glab api empty seg", []string{"glab", "api", "projects/1//variables"}},
		{"glab api private_token query", []string{"glab", "api", "projects/1/issues?private_token=x"}},
		{"glab api sudo query", []string{"glab", "api", "projects/1/issues?sudo=1"}},
		{"glab api bad id", []string{"glab", "api", "projects/a;b/issues"}},
		{"glab api users", []string{"glab", "api", "users/1/issues"}},
	}
	for _, tc := range cases {
		_, _, err := ValidateReadonlyArgv(tc.argv, cfg)
		if err == nil {
			t.Errorf("%s: %v was allowed", tc.name, tc.argv)
			continue
		}
		if k, ok := ViolationKindOf(err); !ok || k != KindAllowlist {
			t.Errorf("%s: want recoverable allowlist violation, got %v", tc.name, err)
		}
	}
}

func TestReadonlyDisabledByDefault(t *testing.T) {
	for _, cfg := range []*PolicyConfig{DefaultPolicyConfig(t.TempDir()), roCfg(t, "git")} {
		if _, _, err := ValidateReadonlyArgv([]string{"glab", "mr", "list"}, cfg); err == nil {
			t.Fatal("glab allowed without being enabled")
		}
	}
	if _, _, err := ValidateReadonlyArgv([]string{"git", "log"}, DefaultPolicyConfig(t.TempDir())); err == nil {
		t.Fatal("git allowed with readonly_commands unset")
	}
	if _, _, err := ValidateReadonlyArgv([]string{"git", "log"}, nil); err == nil {
		t.Fatal("git allowed with nil config")
	}
}

func TestReadonlyGitInjectsNoExtDiffBeforeSeparator(t *testing.T) {
	cfg := roCfg(t, "git")
	var got []string
	var err error
	for _, sub := range []string{"log", "show", "diff", "blame"} {
		_, got, err = ValidateReadonlyArgv([]string{"git", sub, "-n", "1", "--", "a.txt"}, cfg)
		if err != nil {
			if _, pol := ViolationKindOf(err); pol {
				t.Fatalf("%s: %v", sub, err)
			}
			t.Skip("git not installed")
		}
		want := []string{"git", sub, "-n", "1", "--no-ext-diff", "--no-textconv", "--", "a.txt"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %v want %v", sub, got, want)
		}
	}
	_, got, err = ValidateReadonlyArgv([]string{"git", "status"}, cfg)
	if err == nil && !reflect.DeepEqual(got, []string{"git", "status"}) {
		t.Errorf("status must not get injected flags: %v", got)
	}
}

func TestReadonlyGitDirInsideWorkspaceAllowed(t *testing.T) {
	cfg := roCfg(t, "git")
	if err := os.MkdirAll(filepath.Join(cfg.Workspace, "sub", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, _, err := ValidateReadonlyArgv([]string{"git", "--git-dir=sub/.git", "status"}, cfg)
	if _, pol := ViolationKindOf(err); err != nil && pol {
		t.Fatalf("in-workspace git-dir denied: %v", err)
	}
	// Symlink escaping the workspace is resolved and refused.
	link := filepath.Join(cfg.Workspace, "esc")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ValidateReadonlyArgv([]string{"git", "--git-dir=esc", "status"}, cfg); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("symlink escape not refused: %v", err)
	}
}

func TestReadonlyAllowlistMatchesSupported(t *testing.T) {
	al := ReadonlyAllowlist()
	for _, b := range ReadonlySupported {
		if len(al[b]) == 0 {
			t.Errorf("no allowlist entry for %s", b)
		}
	}
	if len(al) != len(ReadonlySupported) {
		t.Errorf("allowlist keys %v != supported %v", al, ReadonlySupported)
	}
}

func TestReadonlyPathArgsInsideWorkspace(t *testing.T) {
	cfg := roCfg(t, "git")
	ws := cfg.Workspace
	if err := os.WriteFile(filepath.Join(ws, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, argv := range [][]string{
		{"git", "diff", "--", "f.txt"},
		{"git", "diff", filepath.Join(ws, "f.txt")},
		{"git", "log", "--", filepath.Join(ws, "f.txt")},
		{"git", "blame", "f.txt"},
	} {
		if _, _, err := ValidateReadonlyArgv(argv, cfg); err != nil {
			if _, pol := ViolationKindOf(err); pol {
				t.Errorf("%v: %v", argv, err)
			}
		}
	}
	// A symlink inside the workspace that points outside is an escape.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("s"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(ws, "lnk")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(ws, "lnkfile")); err != nil {
		t.Fatal(err)
	}
	for _, argv := range [][]string{
		{"git", "diff", "lnk/secret", "lnkfile"},
		{"git", "diff", "--", "lnkfile", "f.txt"},
		{"git", "blame", "lnk/secret"},
	} {
		if _, _, err := ValidateReadonlyArgv(argv, cfg); err == nil {
			t.Errorf("%v: symlink escape allowed", argv)
		}
	}
}

func TestCheckGitConfigList(t *testing.T) {
	rec := func(kv ...string) string { return strings.Join(kv, "\x00") + "\x00" }
	ok := rec("core.bare\nfalse", "user.name\nt", "alias.st\nstatus", "remote.origin.url\nssh://x", "diff.algorithm\npatience")
	if err := CheckGitConfigList(ok); err != nil {
		t.Fatalf("benign config refused: %v", err)
	}
	for _, key := range []string{
		"filter.lfs.clean", "filter.x.smudge", "filter.x.process", "diff.external", "diff.x.command",
		"diff.x.textconv", "core.fsmonitor", "core.hooksPath", "core.pager", "core.sshCommand",
		"core.askPass", "core.worktree", "extensions.worktreeConfig", "gpg.program", "gpg.ssh.program", "include.path", "includeIf.gitdir:/x/.path",
	} {
		if err := CheckGitConfigList(rec("core.bare\nfalse", key+"\nval")); err == nil {
			t.Errorf("%s was allowed", key)
		} else if k, _ := ViolationKindOf(err); k != KindAllowlist {
			t.Errorf("%s: kind %v", key, k)
		}
	}
	if err := CheckGitConfigList(rec("core.bare\ntrue")); err == nil {
		t.Error("core.bare=true allowed")
	}
	if err := CheckGitConfigList(rec("core.bare\nfalse", "extensions.worktreeConfig\nfalse")); err != nil {
		t.Errorf("false-valued keys refused: %v", err)
	}
	// Keys are case-insensitive in git.
	if err := CheckGitConfigList(rec("Filter.X.Clean\ncat")); err == nil {
		t.Error("mixed-case filter key allowed")
	}
}

func TestGitGlobalArgs(t *testing.T) {
	got := GitGlobalArgs([]string{"git", "--no-pager", "--git-dir", "x/.git", "--work-tree=x", "log", "-1"})
	want := []string{"--no-pager", "--git-dir", "x/.git", "--work-tree=x"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if g := GitGlobalArgs([]string{"git", "status"}); len(g) != 0 {
		t.Fatalf("got %v", g)
	}
}

func TestReadonlyGlabThreeSegmentGroupAllowlist(t *testing.T) {
	cfg := roCfg(t, "glab")
	cfg.ReadonlyGlabGroups = []string{"acme"}
	for _, v := range []string{"acme/tools/widget", "acme/app"} {
		if _, _, err := ValidateReadonlyArgv([]string{"glab", "mr", "list", "-R", v}, cfg); err != nil {
			if _, pol := ViolationKindOf(err); pol {
				t.Errorf("%s refused: %v", v, err)
			}
		}
	}
	if _, _, err := ValidateReadonlyArgv([]string{"glab", "mr", "list", "-R", "other/tools/widget"}, cfg); err == nil {
		t.Error("three-segment form with an unlisted group allowed")
	}
}
