package kingdom

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func cleanEnv(extra map[string]string) []string {
	env := []string{}
	for _, v := range os.Environ() {
		k, _, _ := strings.Cut(v, "=")
		if strings.HasPrefix(k, "GIT_") || strings.HasPrefix(k, "BEADS_") || strings.HasPrefix(k, "BD_") || strings.HasPrefix(k, "BR_") || k == "RK_RUN" || k == "RK_ROLE" {
			continue
		}
		if _, ok := extra[k]; !ok {
			env = append(env, v)
		}
	}
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}
func gitExecutable() string { return realExecutable("RK_REAL_GIT", "git") }

// realExecutable finds the real binary behind a guard. A run launched from a
// managed session inherits that session's guard directory on PATH (tmux copies
// the client's PATH), and a guard that resolves to itself execs itself forever.
// Other runs' guard directories are skipped too: they may hold a different rk.
func realExecutable(env, name string) string {
	if p := os.Getenv(env); p != "" && !isGuard(p) {
		return p
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if _, guard := guardRoot(dir); dir == "" || guard {
			continue
		}
		p, e := exec.LookPath(filepath.Join(dir, name))
		if e == nil && !isGuard(p) {
			return p
		}
	}
	return ""
}

// isGuard reports whether p is the running rk executable, under any name.
func isGuard(p string) bool {
	exe, e := os.Executable()
	if e != nil {
		return false
	}
	self, e := os.Stat(exe)
	if e != nil {
		return false
	}
	other, e := os.Stat(p)
	return e == nil && os.SameFile(self, other)
}
func gitOutput(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, gitExecutable(), append([]string{"-C", dir}, args...)...)
	cmd.Env = cleanEnv(nil)
	var out, errout bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errout
	if e := cmd.Run(); e != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), e, strings.TrimSpace(errout.String()))
	}
	return strings.TrimSpace(out.String()), nil
}
func canonicalRepo(path string) (string, string, error) {
	p, e := filepath.Abs(path)
	if e != nil {
		return "", "", e
	}
	common, e := gitOutput(p, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if e != nil {
		return "", "", e
	}
	common, e = filepath.EvalSymlinks(common)
	if e != nil {
		return "", "", e
	}
	list, e := gitOutput(p, "worktree", "list", "--porcelain")
	if e != nil {
		return "", "", e
	}
	for _, line := range strings.Split(list, "\n") {
		if strings.HasPrefix(line, "worktree ") {
			root := strings.TrimPrefix(line, "worktree ")
			root, e = filepath.EvalSymlinks(root)
			return root, common, e
		}
	}
	return "", "", errors.New("no working checkout found")
}
func refOID(dir, ref string) (string, error) {
	return gitOutput(dir, "rev-parse", "--verify", ref+"^{commit}")
}
func dirty(dir string) (bool, error) {
	v, e := gitOutput(dir, "status", "--porcelain", "--untracked-files=all")
	return v != "", e
}
func ancestor(dir, a, b string) bool {
	_, e := gitOutput(dir, "merge-base", "--is-ancestor", a, b)
	return e == nil
}
func validateBranch(dir, branch string) error {
	if strings.HasPrefix(branch, "-") || strings.HasPrefix(branch, "refs/") {
		return errors.New("expected a local branch name")
	}
	_, e := gitOutput(dir, "check-ref-format", "--branch", branch)
	return e
}

// excludeLocalBeads keeps the whole tracker local, including the files that br
// deliberately leaves eligible for version control. Git's common info/exclude
// applies to linked worktrees too, without changing any tracked ignore files.
func excludeLocalBeads(commonDir string) error {
	path := filepath.Join(commonDir, "info", "exclude")
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "/.beads/" {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	// Append rather than replacing the file so another tool's exclusions cannot
	// be lost between the read and write. A leading newline also handles files
	// whose existing last line has no newline.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.WriteString("\n# rat-kingdom: local task and communication data\n/.beads/\n")
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	return errors.Join(writeErr, closeErr)
}
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
func shellCommand(args []string) string {
	q := make([]string, len(args))
	for i, s := range args {
		q[i] = quote(s)
	}
	return strings.Join(q, " ")
}

// GuardGit rejects publication, notes, alias expansion, and command-level bypasses.
// It is an operational guard, not a hostile-code security sandbox.
func GuardGit(args []string) error {
	i := 0
	for i < len(args) && strings.HasPrefix(args[i], "-") {
		a := args[i]
		switch {
		case a == "-C":
			i += 2
		case a == "--no-pager" || a == "--paginate" || a == "--literal-pathspecs" || a == "--no-optional-locks":
			i++
		case a == "--version" || a == "--help":
			return nil
		default:
			return fmt.Errorf("Git global option %q is not allowed in a managed session", a)
		}
	}
	if i >= len(args) {
		return errors.New("Git subcommand required")
	}
	sub := args[i]
	allowed := " status diff log show add commit branch checkout switch restore reset rm mv merge rebase cherry-pick revert stash fetch tag grep ls-files ls-tree cat-file rev-parse rev-list merge-base show-ref symbolic-ref describe check-ref-format blame shortlog diff-tree diff-index diff-files check-ignore version help "
	if !strings.Contains(allowed, " "+sub+" ") {
		return fmt.Errorf("Git command %q denied; publication is only through rk publish after a user request to the king", sub)
	}
	if sub == "help" && len(args) > i+1 {
		return errors.New("use git --help or rk help")
	}
	return deniedGitArgument(args)
}
func deniedGitArgument(args []string) error {
	for _, a := range args {
		if strings.Contains(a, "refs/notes") || strings.HasPrefix(a, "--exec") || strings.HasPrefix(a, "--upload-pack") || a == "--no-verify" {
			return fmt.Errorf("Git argument %q denied", a)
		}
	}
	return nil
}

// gitGlobalOptions maps Git's leading options to whether they take the next
// argument as their value when not written as --option=value.
var gitGlobalOptions = map[string]bool{
	"-C": true, "-c": true, "--config-env": true, "--git-dir": true, "--work-tree": true,
	"--namespace": true, "--super-prefix": true, "--attr-source": true,
	"-p": false, "--paginate": false, "-P": false, "--no-pager": false, "--bare": false,
	"--no-replace-objects": false, "--no-lazy-fetch": false, "--no-optional-locks": false, "--no-advice": false,
	"--literal-pathspecs": false, "--glob-pathspecs": false, "--noglob-pathspecs": false, "--icase-pathspecs": false,
	"--exec-path": false, "--html-path": false, "--man-path": false, "--info-path": false, "--list-cmds": false,
	"-v": false, "--version": false, "-h": false, "--help": false,
}

// gitLocating are the leading options that choose a call's repository or its
// configuration; GIT_DIR and the other environment variables are inherited.
var gitLocating = map[string]bool{"-C": true, "-c": true, "--config-env": true, "--git-dir": true, "--work-tree": true, "--bare": true}

type gitCall struct {
	locate []string // leading options to replay when resolving the repository
	sub    string
	at     int  // index of sub in the arguments, or their length
	info   bool // --version or --help: no repository is involved
}

func parseGitCall(args []string) (gitCall, error) {
	c := gitCall{at: len(args)}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			c.sub, c.at = a, i
			return c, nil
		}
		name, _, inline := strings.Cut(a, "=")
		if !strings.HasPrefix(a, "--") {
			name, inline = a, false
		}
		value, known := gitGlobalOptions[name]
		if !known {
			return c, fmt.Errorf("cannot determine the target repository: unrecognized Git global option %q; denied", a)
		}
		if name == "--version" || name == "--help" {
			c.info = true
			return c, nil
		}
		n := 1
		if value && !inline {
			if i+1 == len(args) {
				return c, fmt.Errorf("cannot determine the target repository: Git option %q needs a value; denied", a)
			}
			n = 2
		}
		if gitLocating[name] {
			c.locate = append(c.locate, args[i:i+n]...)
		}
		i += n - 1
	}
	return c, nil
}

// guardEverywhere keeps publication paths closed whichever repository a call
// targets, including repositories that rat-kingdom does not manage.
func guardEverywhere(c gitCall, args []string) error {
	// Lowercase: a case-insensitive filesystem can find git-push as git-PUSH.
	sub := strings.ToLower(c.sub)
	switch {
	case sub == "push" || sub == "send-pack" || sub == "receive-pack" || sub == "http-push" || sub == "notes" || strings.HasPrefix(sub, "remote-"):
		return fmt.Errorf("Git command %q denied; publication is only through rk publish after a user request to the king", c.sub)
	}
	return deniedGitArgument(args)
}

// gitCommonDir resolves the repository a call would use the way Git does:
// through -C, --git-dir, --work-tree, GIT_DIR and the current directory. It
// returns "" outside any repository, such as a directory about to be
// initialized, and fails when the target cannot be determined.
func gitCommonDir(real string, c gitCall) (string, error) {
	out, e := gitProbe(real, c, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if e != nil {
		if strings.Contains(e.Error(), "not a git repository") {
			return "", nil
		}
		return "", fmt.Errorf("cannot determine the target repository (%v); denied", e)
	}
	common, e := filepath.EvalSymlinks(out)
	if e != nil {
		return "", fmt.Errorf("cannot determine the target repository (%v); denied", e)
	}
	return common, nil
}

// gitProbe runs a read-only query in the call's repository context.
func gitProbe(real string, c gitCall, args ...string) (string, error) {
	cmd := exec.Command(real, append(append([]string{}, c.locate...), args...)...)
	// Untranslated messages, so that "not a git repository" can be recognized.
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	var out, errout bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errout
	if e := cmd.Run(); e != nil {
		return "", fmt.Errorf("%w: %s", e, strings.TrimSpace(errout.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// protectedCommonDirs lists the common Git directories of the repositories
// registered in the stores whose run guard directories are on PATH: the
// session's own and, for a nested session, its parents'. Overriding RK_HOME
// therefore cannot unprotect them, and a disposable store that the session
// drives as a user, as the acceptance suite does, is not protected by it.
// Without any guard directory on PATH, RK_HOME's store is used.
func protectedCommonDirs() ([]string, error) {
	roots := []string{}
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		if root, ok := guardRoot(d); ok {
			roots = append(roots, root)
		}
	}
	if len(roots) == 0 {
		// As Open resolves the store, but without creating its directories.
		root := os.Getenv("RK_HOME")
		if root == "" {
			h, e := os.UserHomeDir()
			if e != nil {
				return nil, fmt.Errorf("cannot find registered repositories (%v); denied", e)
			}
			root = filepath.Join(h, ".rat-kingdom")
		}
		roots = append(roots, root)
	}
	dirs := []string{}
	for _, root := range roots {
		repos, e := (&Store{Root: root}).Repos()
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil {
			return nil, fmt.Errorf("cannot read registered repositories in %s (%v); denied", root, e)
		}
		for _, r := range repos {
			if d, e := filepath.EvalSymlinks(r.CommonDir); e == nil {
				dirs = append(dirs, d)
			} else {
				dirs = append(dirs, filepath.Clean(r.CommonDir))
			}
		}
	}
	return dirs, nil
}

func protectedTarget(real string, c gitCall) (bool, error) {
	common, e := gitCommonDir(real, c)
	if e != nil || common == "" {
		return false, e
	}
	dirs, e := protectedCommonDirs()
	if e != nil {
		return false, e
	}
	for _, d := range dirs {
		if d == common {
			return true, nil
		}
	}
	return false, nil
}

// refuseAlias denies aliases outside registered repositories: Git expands
// an alias such as p = push itself, without consulting this guard again.
func refuseAlias(real string, c gitCall) error {
	_, e := gitProbe(real, c, "config", "--get", "alias."+c.sub)
	var exit *exec.ExitError
	switch {
	case e == nil:
		return fmt.Errorf("Git alias %q denied in a managed session", c.sub)
	case errors.As(e, &exit) && exit.ExitCode() == 1:
		return nil
	default:
		return fmt.Errorf("cannot check Git alias %q (%v); denied", c.sub, e)
	}
}

// guardRoot recognizes a run's guard directory, ROOT/runs/ID/bin.
func guardRoot(dir string) (string, bool) {
	if dir == "" {
		return "", false
	}
	dir = filepath.Clean(dir)
	runs := filepath.Dir(filepath.Dir(dir))
	if filepath.Base(dir) != "bin" || filepath.Base(runs) != "runs" {
		return "", false
	}
	if _, e := os.Lstat(filepath.Join(dir, "git")); e != nil {
		return "", false
	}
	return filepath.Dir(runs), true
}

// GuardedGit applies GuardGit to registered repositories, their worktrees and
// rat worktrees. Elsewhere, such as a disposable test repository or a
// directory about to be initialized, Git runs unchanged except that
// publication, notes, hook bypasses and aliases stay denied.
func GuardedGit(args []string) error {
	p := os.Getenv("RK_REAL_GIT")
	if p != "" && isGuard(p) {
		return errors.New("managed Git executable is the rk guard itself; restart the run")
	}
	if p == "" {
		p = gitExecutable()
	}
	if p == "" {
		return errors.New("missing managed Git executable")
	}
	c, e := parseGitCall(args)
	if e != nil {
		return e
	}
	if e = guardEverywhere(c, args); e != nil {
		return e
	}
	if !c.info {
		protected, e := protectedTarget(p, c)
		if e != nil {
			return e
		}
		if protected {
			if e = GuardGit(args); e != nil {
				return e
			}
		} else if c.sub != "" {
			if e = refuseAlias(p, c); e != nil {
				return e
			}
			// A corrected typo runs in-process and could reach a denied command.
			// The later -c wins over any earlier help.autocorrect setting.
			args = append(append(append([]string{}, args[:c.at]...), "-c", "help.autocorrect=0"), args[c.at:]...)
		}
	}
	cmd := exec.Command(p, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	return cmd.Run()
}
func hooks(s *Store, r *Run, repo *Repo, exe string) (string, error) {
	dest := filepath.Join(s.runDir(r.ID), "hooks")
	if e := os.MkdirAll(dest, 0700); e != nil {
		return "", e
	}
	// Resolve existing hooks without modifying any repository configuration.
	original, e := gitOutput(r.Worktree, "rev-parse", "--path-format=absolute", "--git-path", "hooks")
	if e != nil {
		return "", e
	}
	entries, e := os.ReadDir(original)
	if e != nil && !errors.Is(e, os.ErrNotExist) {
		return "", e
	}
	for _, d := range entries {
		if d.IsDir() || d.Name() == "pre-push" || strings.HasSuffix(d.Name(), ".sample") {
			continue
		}
		target := filepath.Join(original, d.Name())
		info, statErr := os.Stat(target)
		if statErr != nil {
			return "", statErr
		}
		if info.Mode().Perm()&0111 == 0 {
			continue
		}
		destFile := filepath.Join(dest, d.Name())
		// Execute the original path so hooks using dirname "$0" retain their context.
		body := "#!/bin/sh\nexec " + quote(target) + " \"$@\"\n"
		if e = atomicFile(destFile, []byte(body), 0700); e != nil {
			return "", e
		}
	}
	body := "#!/bin/sh\necho 'rat-kingdom: direct pushes are disabled; ask the king to publish explicitly' >&2\nexit 1\n"
	if e = atomicFile(filepath.Join(dest, "pre-push"), []byte(body), 0700); e != nil {
		return "", e
	}
	return dest, nil
}
