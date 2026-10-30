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
func gitExecutable() string {
	if p := os.Getenv("RK_REAL_GIT"); p != "" {
		return p
	}
	p, _ := exec.LookPath("git")
	return p
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
	for _, a := range args {
		if strings.Contains(a, "refs/notes") || strings.HasPrefix(a, "--exec") || strings.HasPrefix(a, "--upload-pack") || a == "--no-verify" {
			return fmt.Errorf("Git argument %q denied", a)
		}
	}
	if sub == "help" && len(args) > i+1 {
		return errors.New("use git --help or rk help")
	}
	return nil
}
func GuardedGit(args []string) error {
	if e := GuardGit(args); e != nil {
		return e
	}
	p := os.Getenv("RK_REAL_GIT")
	if p == "" {
		return errors.New("missing managed Git executable")
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
