package kingdom

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// guardFixture registers a repository with a rat worktree in a disposable
// store and returns the real Git, the repository, the worktree and a scratch
// directory outside every repository.
func guardFixture(t *testing.T) (string, string, string, string) {
	t.Helper()
	real := gitExecutable()
	if real == "" {
		t.Skip("git not found")
	}
	s, e := Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "registered")
	if e = os.Mkdir(repo, 0700); e != nil {
		t.Fatal(e)
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"-c", "user.name=t", "-c", "user.email=t@example.invalid", "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "base"}, {"worktree", "add", "-q", "-b", "rats/a", filepath.Join(tmp, "rat")}} {
		if _, e = gitOutput(repo, args...); e != nil {
			t.Fatal(e)
		}
	}
	_, common, e := canonicalRepo(repo)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.saveRepo(&Repo{ID: "r", Name: "registered", Path: repo, CommonDir: common}); e != nil {
		t.Fatal(e)
	}
	t.Setenv("RK_HOME", s.Root)
	// Only the disposable store: no run guard directories from the caller's PATH.
	t.Setenv("PATH", filepath.Dir(real))
	t.Setenv("RK_REAL_GIT", real)
	t.Setenv("GIT_DIR", "")
	os.Unsetenv("GIT_DIR")
	scratch := filepath.Join(tmp, "scratch")
	if e = os.Mkdir(scratch, 0700); e != nil {
		t.Fatal(e)
	}
	return real, repo, filepath.Join(tmp, "rat"), scratch
}

func TestGitTargetResolution(t *testing.T) {
	real, repo, rat, scratch := guardFixture(t)
	unrelated := filepath.Join(scratch, "unrelated")
	if _, e := gitOutput(scratch, "init", "-q", unrelated); e != nil {
		t.Fatal(e)
	}
	if e := os.Mkdir(filepath.Join(repo, "sub"), 0700); e != nil {
		t.Fatal(e)
	}
	link := filepath.Join(scratch, "link")
	if e := os.Symlink(repo, link); e != nil {
		t.Fatal(e)
	}
	fresh := filepath.Join(scratch, "fresh")
	if e := os.Mkdir(fresh, 0700); e != nil {
		t.Fatal(e)
	}
	cases := []struct {
		name      string
		cwd       string
		args      []string
		protected bool
	}{
		{"cwd in registered repo", repo, []string{"status"}, true},
		{"cwd in a subdirectory", filepath.Join(repo, "sub"), []string{"status"}, true},
		{"-C registered repo", scratch, []string{"-C", repo, "status"}, true},
		{"cumulative -C chain", scratch, []string{"-C", filepath.Dir(repo), "-C", "registered", "-C", "sub", "status"}, true},
		{"relative -C", filepath.Dir(repo), []string{"-C", "registered", "status"}, true},
		{"symlinked path", scratch, []string{"-C", "link", "status"}, true},
		{"rat worktree", rat, []string{"status"}, true},
		{"--git-dir", scratch, []string{"--git-dir=" + filepath.Join(repo, ".git"), "status"}, true},
		{"-C into rat worktree after an unrelated -C", unrelated, []string{"-C", rat, "status"}, true},
		{"unrelated repository", unrelated, []string{"status"}, false},
		{"-C out of a registered repo", repo, []string{"-C", unrelated, "status"}, false},
		{"not yet a repository", fresh, []string{"init"}, false},
		{"-C not yet a repository", repo, []string{"-C", fresh, "init"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(tc.cwd)
			c, e := parseGitCall(tc.args)
			if e != nil {
				t.Fatal(e)
			}
			got, e := protectedTarget(real, c)
			if e != nil || got != tc.protected {
				t.Fatalf("protected=%v err=%v, want %v", got, e, tc.protected)
			}
		})
	}
	t.Run("GIT_DIR", func(t *testing.T) {
		t.Chdir(scratch)
		t.Setenv("GIT_DIR", filepath.Join(rat, ".git"))
		if got, e := protectedTarget(real, gitCall{}); e != nil || !got {
			t.Fatalf("protected=%v err=%v", got, e)
		}
	})
	t.Run("macOS-style symlinked temp root", func(t *testing.T) {
		// /tmp and /var are symlinks on macOS; the registry holds resolved paths.
		root := filepath.Join(scratch, "private")
		if e := os.Symlink(filepath.Dir(repo), root); e != nil {
			t.Fatal(e)
		}
		t.Chdir(filepath.Join(root, "registered"))
		if got, e := protectedTarget(real, gitCall{}); e != nil || !got {
			t.Fatalf("protected=%v err=%v", got, e)
		}
	})
	for _, args := range [][]string{{"-C", filepath.Join(scratch, "missing"), "init"}, {"--frobnicate", "status"}, {"-C"}, {"--git-dir"}} {
		t.Chdir(scratch)
		c, e := parseGitCall(args)
		if e == nil {
			_, e = protectedTarget(real, c)
		}
		if e == nil || !strings.Contains(e.Error(), "cannot determine") {
			t.Errorf("%v: want fail-closed denial, got %v", args, e)
		}
	}
}

func TestGuardProtectsItsOwnSessionStore(t *testing.T) {
	real, repo, _, scratch := guardFixture(t)
	session := os.Getenv("RK_HOME")
	bin := filepath.Join(session, "runs", "abc", "bin")
	if e := os.MkdirAll(bin, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(real, filepath.Join(bin, "git")); e != nil {
		t.Fatal(e)
	}
	// A different store, driven by this session as a user would drive it.
	other, e := Open(filepath.Join(scratch, "other-home"))
	if e != nil {
		t.Fatal(e)
	}
	driven := filepath.Join(scratch, "driven")
	if _, e = gitOutput(scratch, "init", "-q", driven); e != nil {
		t.Fatal(e)
	}
	_, common, e := canonicalRepo(driven)
	if e == nil {
		e = other.saveRepo(&Repo{ID: "d", Name: "driven", Path: driven, CommonDir: common})
	}
	if e != nil {
		t.Fatal(e)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+filepath.Dir(real))
	t.Setenv("RK_HOME", other.Root)
	for dir, want := range map[string]bool{repo: true, driven: false} {
		c, _ := parseGitCall([]string{"-C", dir, "status"})
		if got, e := protectedTarget(real, c); e != nil || got != want {
			t.Errorf("%s: protected=%v err=%v, want %v", dir, got, e, want)
		}
	}
	// Without a run guard directory on PATH, RK_HOME's store is the registry.
	t.Setenv("PATH", filepath.Dir(real))
	for dir, want := range map[string]bool{repo: false, driven: true} {
		c, _ := parseGitCall([]string{"-C", dir, "status"})
		if got, e := protectedTarget(real, c); e != nil || got != want {
			t.Errorf("RK_HOME %s: protected=%v err=%v, want %v", dir, got, e, want)
		}
	}
}

func TestGuardedGitScopesRestrictions(t *testing.T) {
	_, repo, rat, scratch := guardFixture(t)
	t.Chdir(scratch)
	fresh := filepath.Join(scratch, "fresh")
	if e := os.Mkdir(fresh, 0700); e != nil {
		t.Fatal(e)
	}
	if e := GuardedGit([]string{"-C", fresh, "init", "-q", "-b", "main"}); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(filepath.Join(fresh, ".git")); e != nil {
		t.Fatal(e)
	}
	if e := GuardedGit([]string{"-C", fresh, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "x"}); e != nil {
		t.Fatal(e)
	}
	if e := GuardedGit([]string{"-C", fresh, "init", "-q", "--bare", "remote.git"}); e != nil {
		t.Fatal(e)
	}
	if e := GuardedGit([]string{"-C", fresh, "config", "alias.q", "push"}); e != nil {
		t.Fatal(e)
	}
	denied := map[string][]string{
		"worktree add in registered repo": {"-C", repo, "worktree", "add", filepath.Join(scratch, "escape")},
		"worktree add in rat worktree":    {"-C", rat, "worktree", "add", filepath.Join(scratch, "escape")},
		"init in registered repo":         {"-C", repo, "init"},
		"protected global option":         {"--git-dir=" + filepath.Join(repo, ".git"), "worktree", "list"},
		"push from unrelated repo":        {"-C", fresh, "push", "remote.git", "main"},
		"uppercase push":                  {"-C", fresh, "PUSH", "remote.git", "main"},
		"send-pack":                       {"-C", fresh, "send-pack", "remote.git", "main"},
		"notes":                           {"-C", fresh, "notes", "add", "-m", "x"},
		"refs/notes":                      {"-C", fresh, "update-ref", "refs/notes/commits", "HEAD"},
		"--no-verify":                     {"-C", fresh, "commit", "--no-verify", "--allow-empty", "-m", "x"},
		"inline alias":                    {"-C", fresh, "-c", "alias.p=push", "p", "remote.git", "main"},
		"configured alias":                {"-C", fresh, "q", "remote.git", "main"},
		"unknown global option":           {"-C", fresh, "--frobnicate", "status"},
	}
	for name, args := range denied {
		if e := GuardedGit(args); e == nil || !strings.Contains(e.Error(), "denied") && !strings.Contains(e.Error(), "not allowed") {
			t.Errorf("%s: %v", name, e)
		}
	}
	// A typo autocorrected to push would run in-process; it must not publish.
	GuardedGit([]string{"-C", fresh, "-c", "help.autocorrect=1", "psuh", "remote.git", "main"})
	if _, e := os.Stat(filepath.Join(fresh, "remote.git", "refs", "heads", "main")); e == nil {
		t.Fatal("autocorrected push reached the remote")
	}
	if e := GuardedGit([]string{"-C", rat, "status", "--porcelain"}); e != nil {
		t.Fatal(e)
	}
}
