package kingdom

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// tmuxGone reports whether a failed tmux command found no session or no server.
func tmuxGone(e error, out []byte) bool {
	var ee *exec.ExitError
	if !errors.As(e, &ee) || ee.ExitCode() != 1 {
		return false
	}
	text := string(out)
	return strings.Contains(text, "can't find session") || strings.Contains(text, "no server running") || strings.Contains(text, "No such file or directory") || strings.Contains(text, "Connection refused")
}

func sessionExists(session string) (bool, error) {
	cmd := exec.Command("tmux", "has-session", "-t", "="+session)
	out, e := cmd.CombinedOutput()
	if e == nil {
		return true, nil
	}
	if tmuxGone(e, out) {
		return false, nil
	}
	return false, fmt.Errorf("tmux observation failed: %w: %s", e, out)
}

// killSession closes a session. A session that ended after it was observed,
// possibly taking the last-session server with it, counts as closed.
func killSession(session string) ([]byte, error) {
	out, e := exec.Command("tmux", "kill-session", "-t", "="+session).CombinedOutput()
	if e != nil && tmuxGone(e, out) {
		return out, nil
	}
	return out, e
}
func (s *Store) prepare(repo *Repo, r *Run, exe string) error {
	dir := s.runDir(r.ID)
	bin := filepath.Join(dir, "bin")
	if e := os.MkdirAll(bin, 0700); e != nil {
		return e
	}
	for _, name := range []string{"rk", "git", "br"} {
		p := filepath.Join(bin, name)
		os.Remove(p)
		if e := os.Symlink(exe, p); e != nil {
			return e
		}
	}
	prompt, e := s.Prompt(repo, r)
	if e != nil {
		return e
	}
	if e = atomicFile(filepath.Join(dir, "prompt.txt"), []byte(prompt), 0600); e != nil {
		return e
	}
	_, e = hooks(s, r, repo, exe)
	if e != nil {
		return e
	}
	// Provider hooks inspect shell tool calls too, including absolute Git paths.
	settings := map[string]any{"hooks": map[string]any{"PreToolUse": []any{map[string]any{"matcher": "Bash", "hooks": []any{map[string]any{"type": "command", "command": shellCommand([]string{exe, "_check-command"})}}}}}}
	return atomicJSON(filepath.Join(dir, "claude-settings.json"), settings)
}
func (s *Store) Prompt(repo *Repo, r *Run) (string, error) {
	common := fmt.Sprintf(`You are %s, a %s in rat-kingdom. Run ID: %s. Repository: %s.
Use rk commands; RK_HOME, RK_RUN, RK_ROLE and BEADS_DIR are set for you.
Do not push, delete remote refs, use Git notes, or perform synchronization.
Do not bypass the Git command guards or alter their environment/hooks.
Local commits are encouraged. Preserve unrelated work and existing hooks.
Check rk inbox at natural checkpoints, then acknowledge processed comments with rk inbox --ack ID.
Do not treat terminal silence, process exit, or a claim as task completion.
`, r.Name, r.Role, r.ID, repo.Name)
	if r.Role == "king" {
		return common + fmt.Sprintf(`You are the user's coordinator. Create small tasks with explicit acceptance criteria using rk task --repo %s create ... .
Use rk spawn rat --repo %s [--task ID]. Each rat works on one task in an isolated worktree.
Use rk wait --timeout 60s to wait for results, and rk reconcile and rk list to monitor; rk show RUN and rk task show TASK to inspect results.
Review the exact reported commit and checks. Stop a finished rat with rk stop RUN, then rk integrate RUN.
Integration runs the configured repository check, merges locally, and only then closes the task.
Blocked work remains open. Use rk tell RUN MESSAGE or rk resume RUN --message TEXT for rework.
Use rk clean RUN only after integration. Never discard useful work without the user's explicit request.
There is no steward and no autonomous review/dismissal agent.
Only when the user specifically asks you to push: rk publish --repo %s --remote NAME --branch BRANCH --request 'the user request'.
Publication is limited to that branch and that operation. Never pass publication authority to rats.
`, repo.Name, repo.Name, repo.Name), nil
	}
	t, e := getTask(repo, r.Task)
	if e != nil {
		return "", e
	}
	return common + fmt.Sprintf(`Your task %s is already exclusively claimed by this run. Do not claim another task or close this one.
Title: %s
Description:
%s
Work only in %s on %s. Integration target: %s.
Implement the task and run appropriate checks. Record useful findings as task comments (br comments add %s TEXT).
When ready: commit your intended files, leave a clean worktree, then rk finish --summary TEXT --checks TEXT.
If blocked: rk block --summary TEXT. Never report blocked work as ready.
After reporting, stop working. The king reviews, integrates and closes the ticket.
Check rk inbox before handoff. No push is needed for completion or recovery.
Previous handoff / continuation: %s
`, t.ID, t.Title, t.Description, r.Worktree, r.Branch, r.Target, t.ID, r.Summary), nil
}
func (s *Store) runEnv(repo *Repo, r *Run) []string {
	realGit := gitExecutable()
	realBR := beadsExecutable()
	return cleanEnv(map[string]string{
		"RK_HOME": s.Root, "RK_RUN": r.ID, "RK_ROLE": r.Role, "RK_REPO": repo.ID,
		"RK_REAL_GIT": realGit, "RK_REAL_BR": realBR, "BEADS_DIR": repo.BeadsDir,
		"PATH":             filepath.Join(s.runDir(r.ID), "bin") + string(os.PathListSeparator) + os.Getenv("PATH"),
		"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "core.hooksPath", "GIT_CONFIG_VALUE_0": filepath.Join(s.runDir(r.ID), "hooks"),
		"BR_AGENT_NAME": r.Name, "BR_SESSION": r.ID, "BR_HARNESS": "rat-kingdom", "RUST_LOG": "error",
	})
}
func (s *Store) RunProcess(id string, attempt int) error {
	r, e := s.Run(id)
	if e != nil {
		return e
	}
	live, e := processLock(filepath.Join(s.runDir(r.ID), "process.lock"))
	if e != nil {
		return errors.New("launcher already active")
	}
	defer live.Close()
	unlock, e := s.lock()
	if e != nil {
		return e
	}
	r, e = s.Run(id)
	if e != nil {
		unlock()
		return e
	}
	if r.Attempt != attempt || r.Process != "starting" {
		unlock()
		return errors.New("stale launcher attempt")
	}
	repo, e := s.Repo(r.RepoID)
	if e != nil {
		unlock()
		return e
	}
	r.PID = os.Getpid()
	r.Process = "starting"
	if e = s.saveRun(r); e != nil {
		unlock()
		return e
	}
	unlock()
	prompt, e := os.ReadFile(filepath.Join(s.runDir(id), "prompt.txt"))
	if e != nil {
		return e
	}
	args := append([]string{}, repo.Command...)
	for i, a := range args {
		a = strings.ReplaceAll(a, "{prompt}", string(prompt))
		a = strings.ReplaceAll(a, "{prompt_file}", filepath.Join(s.runDir(id), "prompt.txt"))
		args[i] = a
	}
	if filepath.Base(args[0]) == "claude" {
		args = append(args, "--settings", filepath.Join(s.runDir(id), "claude-settings.json"))
	}
	logfile := filepath.Join(s.runDir(id), fmt.Sprintf("attempt-%d.log", attempt))
	var cmd *exec.Cmd
	if runtime.GOOS == "darwin" {
		cmd = exec.Command("/usr/bin/script", append([]string{"-q", logfile}, args...)...)
	} else {
		cmd = exec.Command("script", "--quiet", "--return", "--flush", "--command", shellCommand(args), logfile)
	}
	cmd.Dir = r.Worktree
	cmd.Env = s.runEnv(repo, r)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.ExtraFiles = []*os.File{live} // Descendants retain the liveness lock if the launcher is killed.
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sig)
	runErr := cmd.Start()
	code := 1
	if runErr == nil {
		updateUnlock, updateErr := s.lock()
		if updateErr == nil {
			current, readErr := s.Run(id)
			if readErr != nil {
				updateErr = readErr
			} else {
				current.ChildPID = cmd.Process.Pid
				current.Process = "running"
				updateErr = s.saveRun(current)
			}
			updateUnlock()
		}
		if updateErr != nil {
			syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
			cmd.Wait()
			return updateErr
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case runErr = <-done:
		case <-sig:
			syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
			select {
			case runErr = <-done:
			case <-time.After(3 * time.Second):
				syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				runErr = <-done
			}
		}
		if cmd.ProcessState != nil {
			code = cmd.ProcessState.ExitCode()
		}
	}
	unlock, e = s.lock()
	if e != nil {
		return e
	}
	defer unlock()
	r, e = s.Run(id)
	if e != nil {
		return e
	}
	if r.Attempt == attempt {
		r.PID = 0
		r.ChildPID = 0
		r.Process = "exited"
		r.ExitCode = &code
		if runErr != nil {
			r.Error = runErr.Error()
		}
		if e = s.saveRun(r); e != nil {
			return e
		}
	}
	return runErr
}
func (s *Store) Stop(id string) (*Run, error) {
	unlock, e := s.lock()
	if e != nil {
		return nil, e
	}
	r, e := s.Run(id)
	unlock()
	if e != nil {
		return nil, e
	}
	alive, e := runAlive(s, r)
	if e != nil {
		return nil, e
	}
	if alive {
		// The script process owns the PTY. Closing it also hangs up the agent,
		// even if the launcher was killed and cannot forward a signal.
		if r.ChildPID > 0 {
			if signalErr := syscall.Kill(-r.ChildPID, syscall.SIGTERM); signalErr != nil && !errors.Is(signalErr, syscall.ESRCH) {
				return nil, signalErr
			}
		}
		// Closing the owned terminal also stops descendants if the launcher died.
		exists, observationErr := sessionExists(r.Session)
		if observationErr != nil {
			return nil, observationErr
		}
		if exists {
			if out, stopErr := killSession(r.Session); stopErr != nil {
				return nil, fmt.Errorf("stop terminal: %w: %s", stopErr, out)
			}
		}
		if r.PID <= 0 {
			return nil, errors.New("launcher lock held but PID not recorded; retry shortly")
		}
		if e = syscall.Kill(r.PID, syscall.SIGTERM); e != nil && !errors.Is(e, syscall.ESRCH) {
			return nil, e
		}
		deadline := time.Now().Add(6 * time.Second)
		for time.Now().Before(deadline) {
			alive, e = runAlive(s, r)
			if e != nil {
				return nil, e
			}
			if !alive {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if alive {
			return nil, errors.New("launcher did not stop; work retained; inspect with rk attach")
		}
	}
	exists, e := sessionExists(r.Session)
	if e != nil {
		return nil, e
	}
	if exists {
		if out, e := killSession(r.Session); e != nil {
			return nil, fmt.Errorf("stop tmux: %w: %s", e, out)
		}
	}
	unlock, e = s.lock()
	if e != nil {
		return nil, e
	}
	defer unlock()
	current, e := s.Run(id)
	if e != nil {
		return nil, e
	}
	if current.Attempt != r.Attempt {
		return nil, errors.New("run resumed concurrently; refusing to overwrite state")
	}
	current.Process = "stopped"
	current.PID = 0
	current.ChildPID = 0
	return current, s.saveRun(current)
}
func (s *Store) Logs(id string, w io.Writer) error {
	r, e := s.Run(id)
	if e != nil {
		return e
	}
	f, e := os.Open(filepath.Join(s.runDir(r.ID), fmt.Sprintf("attempt-%d.log", r.Attempt)))
	if e != nil {
		return e
	}
	defer f.Close()
	_, e = io.Copy(w, f)
	return e
}

// CheckShell is an additional provider hook, not a shell parser or a security sandbox.
func CheckShell(reader io.Reader) error {
	var v struct {
		ToolInput struct {
			Command string `json:"command"`
		} `json:"tool_input"`
	}
	if e := json.NewDecoder(reader).Decode(&v); e != nil {
		return e
	}
	text := strings.ToLower(v.ToolInput.Command)
	for _, bad := range []string{"git push", "/git push", "send-pack", "receive-pack", "refs/notes", "git notes", "--no-verify", "core.hookspath", "git_config", "rk_real_git", "rk_role=", "unset rk_"} {
		if strings.Contains(text, bad) {
			return fmt.Errorf("managed command denied (%s); use the king's explicit rk publish path", strconv.Quote(bad))
		}
	}
	return nil
}
