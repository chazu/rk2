package kingdom

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func (s *Store) Init(path, name, target string, command, check []string, max int, prefix string) (*RepoView, error) {
	unlock, e := s.lock()
	if e != nil {
		return nil, e
	}
	defer unlock()
	root, common, e := canonicalRepo(path)
	if e != nil {
		return nil, e
	}
	id := repoID(common)
	if _, e = s.Repo(id); e == nil {
		return nil, errors.New("already registered; use rk config")
	}
	if _, e = refOID(root, "HEAD"); e != nil {
		return nil, errors.New("repository needs an initial commit before registration")
	}
	if name == "" {
		name = filepath.Base(root)
	}
	if !safeID(name) {
		return nil, errors.New("invalid repository name")
	}
	if prefix != "" && !validPrefix(prefix) {
		return nil, fmt.Errorf("invalid issue prefix %q: use lowercase letters, digits and single inner hyphens", prefix)
	}
	fallback := prefix
	if fallback == "" {
		fallback = defaultPrefix(name)
	}
	repos, e := s.Repos()
	if e != nil {
		return nil, e
	}
	for _, r := range repos {
		if r.Name == name {
			return nil, errors.New("repository name already registered")
		}
	}
	if target == "" {
		target = "rk/integration"
	}
	if e = validateBranch(root, target); e != nil {
		return nil, e
	}
	if max < 1 {
		return nil, errors.New("max-rats must be positive")
	}
	if len(command) == 0 {
		command = append([]string{}, defaultCommand...)
	}
	r := &Repo{ID: id, Name: name, Path: root, CommonDir: common, BeadsDir: filepath.Join(root, ".beads"), Target: target, Command: command, MaxRats: max, Check: check}
	if e = excludeLocalBeads(common); e != nil {
		return nil, fmt.Errorf("exclude local Beads data: %w", e)
	}
	if e = initWorkspace(r, prefix, fallback); e != nil {
		return nil, e
	}
	if _, e = beads(r, "king", "config", "set", "claim_exclusive", "true"); e != nil {
		return nil, e
	}
	if _, e = readyTasks(r); e != nil {
		return nil, e
	}
	if _, e = refOID(root, "refs/heads/"+target); e != nil {
		if _, e = gitOutput(root, "branch", target, "HEAD"); e != nil {
			return nil, e
		}
	}
	if e = s.saveRepo(r); e != nil {
		return nil, e
	}
	v := viewRepo(r)
	return &v, nil
}

// ConfigKing sets the king's agent command, stored in RK_HOME/king.json. It
// applies the next time the king launches; with no command it only reports.
func (s *Store) ConfigKing(command []string) (*King, error) {
	unlock, e := s.lock()
	if e != nil {
		return nil, e
	}
	defer unlock()
	if command != nil {
		if len(command) == 0 {
			return nil, errors.New("command cannot be empty")
		}
		if e = atomicJSON(filepath.Join(s.Root, "king.json"), &King{Command: command}); e != nil {
			return nil, e
		}
	}
	return s.KingConfig()
}

// Config changes rk's settings for a repository. A prefix is written to the
// Beads workspace config; existing issues keep their IDs.
func (s *Store) Config(id string, command, check []string, max int, prefix string) (*RepoView, error) {
	unlock, e := s.lock()
	if e != nil {
		return nil, e
	}
	defer unlock()
	r, e := s.Repo(id)
	if e != nil {
		return nil, e
	}
	if command != nil {
		if len(command) == 0 {
			return nil, errors.New("command cannot be empty")
		}
		r.Command = command
	}
	if check != nil {
		r.Check = check
	}
	if max > 0 {
		r.MaxRats = max
	}
	if prefix != "" {
		if e = setPrefix(r, prefix); e != nil {
			return nil, e
		}
	}
	if e = s.saveRepo(r); e != nil {
		return nil, e
	}
	v := viewRepo(r)
	return &v, nil
}
func (s *Store) Spawn(repoName, role, task, name string) (*Run, error) {
	unlock, e := s.lock()
	if e != nil {
		return nil, e
	}
	defer unlock()
	if role != "rat" && role != "king" {
		return nil, errors.New("role must be rat or king")
	}
	// The king belongs to no repository: it runs from its own directory with
	// the global agent command, so it can be spawned from anywhere.
	var repo *Repo
	var command []string
	if role == "king" {
		if repoName != "" {
			return nil, errors.New("the king serves every repository; spawn it without --repo")
		}
		k, e := s.KingConfig()
		if e != nil {
			return nil, e
		}
		command = k.Command
	} else {
		if repo, e = s.Repo(repoName); e != nil {
			return nil, e
		}
		command = repo.Command
	}
	if _, e = exec.LookPath("tmux"); e != nil {
		return nil, e
	}
	if len(command) == 0 {
		return nil, errors.New("no agent command configured")
	}
	if _, e = exec.LookPath(command[0]); e != nil {
		return nil, e
	}
	rs, e := s.Runs()
	if e != nil {
		return nil, e
	}
	n := 0
	for _, r := range rs {
		// One king serves every repository; it is resumed, never respawned.
		if r.Role == "king" && role == "king" && r.State != "dismissed" {
			if activeState(r) {
				return nil, errors.New("king already active; use rk attach king")
			}
			return nil, errors.New("king already exists; use rk resume king, or rk dismiss king for a fresh one")
		}
		if role == "rat" && r.Role == "rat" && r.RepoID == repo.ID && activeState(r) {
			n++
		}
	}
	if role == "rat" && n >= repo.MaxRats {
		return nil, fmt.Errorf("capacity reached (%d); stop or reconcile existing rats", repo.MaxRats)
	}
	if role == "rat" && task == "" {
		tasks, e := readyTasks(repo)
		if e != nil {
			return nil, e
		}
		for _, t := range tasks {
			if t.Type != "epic" {
				task = t.ID
				break
			}
		}
		if task == "" {
			return nil, errors.New("no unassigned ready tasks")
		}
	}
	inUse := func(name string) bool {
		for _, r := range rs {
			if r.Name == name && r.State != "cleaned" && r.State != "discarded" && r.State != "dismissed" {
				return true
			}
		}
		return false
	}
	id := newID()
	if name == "" && role == "king" {
		name = "king"
	} else if name == "" {
		if id, name, e = pickRatName(inUse, newID); e != nil {
			return nil, e
		}
	}
	if !safeID(name) || strings.ContainsAny(name, " \t\n:") {
		return nil, errors.New("invalid agent name")
	}
	if role == "rat" && name == "king" {
		return nil, errors.New("the name king is reserved")
	}
	if inUse(name) {
		return nil, errors.New("agent name already in use")
	}
	r := &Run{ID: id, Name: name, Role: role, State: "preparing", Process: "not_started", Created: time.Now().UTC(), Worktree: s.kingDir(), Session: "rk-" + id, Attempt: 1}
	if role == "rat" {
		r.RepoID = repo.ID
		r.Task = task
		r.Target = repo.Target
		r.Branch = "rats/" + name + "/" + id
		r.Worktree = filepath.Join(s.Root, "worktrees", repo.ID, id)
	}
	if e = s.saveRun(r); e != nil {
		return nil, e
	}
	fail := func(err error) (*Run, error) {
		r.State = "startup_failed"
		r.Process = "not_started"
		r.Error = err.Error()
		if se := s.saveRun(r); se != nil {
			return r, fmt.Errorf("%w (state save: %v)", err, se)
		}
		return r, fmt.Errorf("run %s preserved for recovery: %w", r.ID, err)
	}
	if role == "rat" {
		if _, e = beads(repo, "king", "config", "set", "claim_exclusive", "true"); e != nil {
			return fail(e)
		}
		if e = claimTask(repo, r); e != nil {
			return fail(e)
		}
		r.Claimed = true
		if e = s.saveRun(r); e != nil {
			return fail(e)
		}
		if _, e = gitOutput(repo.Path, "worktree", "add", "-b", r.Branch, r.Worktree, "refs/heads/"+repo.Target); e != nil {
			return fail(e)
		}
	}
	if e = s.launch(repo, r); e != nil {
		return fail(e)
	}
	return r, nil
}

// ratName is a recognizable name drawn from a run ID.
func ratName(id string) string {
	names := []string{"Pip", "Moss", "Crumb", "Nettle", "Pebble", "Clover", "Soot", "Thimble"}
	return names[int(id[0])%len(names)] + "-" + id[:4]
}

// pickRatName draws run IDs until the generated name is free. A name carries
// only four hex digits of its ID, so it can match a live run's.
func pickRatName(inUse func(string) bool, next func() string) (string, string, error) {
	for attempt := 0; attempt < 16; attempt++ {
		id := next()
		if name := ratName(id); !inUse(name) {
			return id, name, nil
		}
	}
	return "", "", errors.New("could not pick an unused rat name; pass --name")
}
func (s *Store) launch(repo *Repo, r *Run) error {
	exe, e := os.Executable()
	if e != nil {
		return e
	}
	exe, e = filepath.EvalSymlinks(exe)
	if e != nil {
		return e
	}
	if r.Role == "king" {
		if e = os.MkdirAll(r.Worktree, 0700); e != nil {
			return e
		}
	}
	if e = s.prepare(repo, r, exe); e != nil {
		return e
	}
	r.Process = "starting"
	if r.State == "preparing" || r.State == "startup_failed" {
		r.State = "working"
	}
	r.Error = ""
	r.ExitCode = nil
	r.PID = 0
	r.ChildPID = 0
	if e = s.saveRun(r); e != nil {
		return e
	}
	command := shellCommand([]string{exe, "_run", "--home", s.Root, r.ID, fmt.Sprint(r.Attempt)})
	args := []string{"new-session", "-d", "-s", r.Session, "-c", r.Worktree}
	cmd := exec.Command("tmux", append(args, command)...)
	out, e := cmd.CombinedOutput()
	if e != nil {
		return fmt.Errorf("tmux launch: %w: %s", e, out)
	}
	return nil
}

func (s *Store) Resume(id, message string) (*Run, error) {
	unlock, e := s.lock()
	if e != nil {
		return nil, e
	}
	defer unlock()
	r, e := s.Run(id)
	if e != nil {
		return nil, e
	}
	var repo *Repo
	if r.Role != "king" {
		if repo, e = s.runRepo(r); e != nil {
			return nil, e
		}
	}
	if r.State == "integrated" || r.State == "cleaned" || r.State == "discarded" || r.State == "dismissed" {
		return nil, errors.New("cannot resume a finalized run")
	}
	alive, e := runAlive(s, r)
	if e != nil {
		return nil, e
	}
	exists, e := sessionExists(r.Session)
	if e != nil {
		return nil, e
	}
	if alive || exists {
		return nil, errors.New("run is still alive; stop before resuming")
	}
	all, e := s.Runs()
	if e != nil {
		return nil, e
	}
	active := 0
	for _, other := range all {
		if other.ID == r.ID || !activeState(other) {
			continue
		}
		if r.Role == "king" && other.Role == "king" {
			return nil, errors.New("another king is active")
		}
		if other.RepoID == r.RepoID && other.Role == "rat" {
			active++
		}
	}
	if r.Role == "rat" && active >= repo.MaxRats {
		return nil, errors.New("capacity reached; stop another rat before resuming")
	}
	if r.Role == "rat" {
		if e = claimTask(repo, r); e != nil {
			return nil, e
		}
		r.Claimed = true
		if _, e = os.Stat(r.Worktree); errors.Is(e, os.ErrNotExist) {
			if r.State != "startup_failed" {
				return nil, errors.New("worktree is missing; refusing to reconstruct over potentially lost work")
			}
			if _, e = refOID(repo.Path, "refs/heads/"+r.Branch); e == nil {
				_, e = gitOutput(repo.Path, "worktree", "add", r.Worktree, r.Branch)
			} else {
				_, e = gitOutput(repo.Path, "worktree", "add", "-b", r.Branch, r.Worktree, "refs/heads/"+r.Target)
			}
			if e != nil {
				return nil, e
			}
		}
	}
	if r.Role == "king" {
		// A king launched from a repository before kings left them moves to
		// its own directory; the repository itself is untouched.
		r.RepoID, r.Worktree, r.Branch, r.Target = "", s.kingDir(), "", ""
	}
	r.Attempt++
	r.State = "working"
	r.Summary = message
	r.Commit = ""
	r.Checks = ""
	r.IntegratedCommit = ""
	r.IntegrationCheck = ""
	queue(r, "Resuming existing worktree. "+message, "king")
	if e = s.launch(repo, r); e != nil {
		r.Process = "not_started"
		r.State = "startup_failed"
		r.Error = e.Error()
		s.saveRun(r)
		return r, e
	}
	return r, nil
}

// Dismiss stops the king and retires its run so rk spawn king starts a fresh
// one. The run record and logs are kept.
func (s *Store) Dismiss(id string) (*Run, error) {
	r, e := s.Run(id)
	if e != nil {
		return nil, e
	}
	if r.Role != "king" {
		return nil, errors.New("only the king is dismissed; stop, integrate and clean rats")
	}
	if r.State == "dismissed" {
		return r, nil
	}
	if r, e = s.Stop(id); e != nil {
		return nil, e
	}
	unlock, e := s.lock()
	if e != nil {
		return nil, e
	}
	defer unlock()
	current, e := s.Run(id)
	if e != nil {
		return nil, e
	}
	if current.Attempt != r.Attempt {
		return nil, errors.New("king resumed concurrently; not dismissed")
	}
	current.State = "dismissed"
	return current, s.saveRun(current)
}

// Restart stops a run and resumes it in its existing worktree.
func (s *Store) Restart(id, message string) (*Run, error) {
	if _, e := s.Stop(id); e != nil {
		return nil, e
	}
	return s.Resume(id, message)
}
func (s *Store) Result(id, outcome, summary, checks string) (*Run, error) {
	unlock, e := s.lock()
	if e != nil {
		return nil, e
	}
	defer unlock()
	r, e := s.Run(id)
	if e != nil {
		return nil, e
	}
	if r.Role != "rat" {
		return nil, errors.New("only rats submit task results")
	}
	repo, e := s.runRepo(r)
	if e != nil {
		return nil, e
	}
	if r.State != "working" && r.State != "blocked" && r.State != "ready" {
		return nil, fmt.Errorf("cannot submit from %s", r.State)
	}
	if e = ownTask(repo, r); e != nil {
		return nil, e
	}
	if strings.TrimSpace(summary) == "" {
		return nil, errors.New("summary is required")
	}
	switch outcome {
	case "ready":
		currentBranch, branchErr := gitOutput(r.Worktree, "branch", "--show-current")
		if branchErr != nil {
			return nil, branchErr
		}
		if currentBranch != r.Branch {
			return nil, errors.New("worktree is not on its assigned branch")
		}
		if strings.TrimSpace(checks) == "" {
			return nil, errors.New("--checks must describe validation or why none applies")
		}
		d, e := dirty(r.Worktree)
		if e != nil {
			return nil, e
		}
		if d {
			return nil, errors.New("commit intended work before submitting; worktree has dirty or untracked files")
		}
		r.Commit, e = refOID(r.Worktree, "HEAD")
		if e != nil {
			return nil, e
		}
	case "blocked":
		r.Commit = ""
	default:
		return nil, errors.New("outcome must be ready or blocked")
	}
	r.State = outcome
	r.Summary = summary
	r.Checks = checks
	queue(r, fmt.Sprintf("%s (%s) reports %s. Commit: %s. Checks: %s. %s", r.Name, r.ID, outcome, r.Commit, checks, summary))
	if e = s.saveRun(r); e != nil {
		return nil, e
	}
	de := deliver(repo, r)
	if e = s.saveRun(r); e != nil {
		return nil, e
	}
	if de != nil {
		return r, fmt.Errorf("result saved; comment pending (rk reconcile retries): %w", de)
	}
	return r, nil
}
func (s *Store) Tell(id, text string) error {
	unlock, e := s.lock()
	if e != nil {
		return e
	}
	defer unlock()
	r, e := s.Run(id)
	if e != nil {
		return e
	}
	if r.Task == "" {
		return errors.New("use task comments for rats; attach to the king for instructions")
	}
	repo, e := s.runRepo(r)
	if e != nil {
		return e
	}
	if text == "" {
		return errors.New("message required")
	}
	queue(r, "Coordinator: "+text, "king")
	if e = s.saveRun(r); e != nil {
		return e
	}
	de := deliver(repo, r)
	if e = s.saveRun(r); e != nil {
		return e
	}
	return de
}
func (s *Store) Inbox(id string, ack int) ([]Comment, error) {
	unlock, e := s.lock()
	if e != nil {
		return nil, e
	}
	defer unlock()
	r, e := s.Run(id)
	if e != nil {
		return nil, e
	}
	if r.Task == "" {
		return nil, errors.New("king uses rk list and task comments")
	}
	repo, e := s.runRepo(r)
	if e != nil {
		return nil, e
	}
	cs, e := comments(repo, r.Task)
	if e != nil {
		return nil, e
	}
	max := 0
	for _, c := range cs {
		if c.ID > max {
			max = c.ID
		}
	}
	if ack > max || ack < 0 {
		return nil, errors.New("ack must identify an existing comment cursor")
	}
	if ack > r.Cursor {
		r.Cursor = ack
		if e = s.saveRun(r); e != nil {
			return nil, e
		}
	}
	out := []Comment{}
	for _, c := range cs {
		if c.ID > r.Cursor {
			out = append(out, c)
		}
	}
	return out, nil
}
func (s *Store) Reconcile() ([]*Run, error) {
	unlock, e := s.lock()
	if e != nil {
		return nil, e
	}
	defer unlock()
	rs, e := s.Runs()
	if e != nil {
		return nil, e
	}
	for _, r := range rs {
		if activeState(r) {
			alive, ae := runAlive(s, r)
			exists, te := sessionExists(r.Session)
			if ae != nil || te != nil {
				r.Process = "unknown"
				r.Error = fmt.Sprintf("observation failed: %v %v", ae, te)
			} else if !alive && !exists {
				r.Process = "unknown"
				r.PID = 0
				r.Error = "launcher disappeared without an exit receipt; inspect and stop before resuming; work preserved"
			}
		}
		// Kings have no task, so nothing to deliver or close.
		if r.Role == "king" {
			if e = s.saveRun(r); e != nil {
				return nil, e
			}
			continue
		}
		repo, e := s.runRepo(r)
		if e != nil {
			return nil, e
		}
		if e = deliver(repo, r); e != nil {
			r.Error = "pending comment: " + e.Error()
		}
		if r.State == "integrated" && !r.Closed {
			if e = closeIntegrated(repo, r); e != nil {
				r.Error = e.Error()
			}
		}
		if e = s.saveRun(r); e != nil {
			return nil, e
		}
	}
	return rs, nil
}
func closeIntegrated(repo *Repo, r *Run) error {
	t, e := getTask(repo, r.Task)
	if e != nil {
		return e
	}
	if t.Status == "closed" {
		r.Closed = true
		return nil
	}
	if e = ownTask(repo, r); e != nil {
		return e
	}
	_, e = beads(repo, "king", "close", r.Task, "--reason", "Integrated "+r.IntegratedCommit+" into "+r.Target, "--transition-comment", "Rat Kingdom integration verified: "+r.IntegratedCommit)
	if e == nil {
		r.Closed = true
	}
	return e
}

// CloseTask closes a task without integration, e.g. after rk clean --discard or
// for duplicate or abandoned work. It refuses while any run of the task could
// still hold a claim or unintegrated work, and when the task is already closed.
func (s *Store) CloseTask(repoName, id, reason string) (*Task, error) {
	unlock, e := s.lock()
	if e != nil {
		return nil, e
	}
	defer unlock()
	repo, e := s.Repo(repoName)
	if e != nil {
		return nil, e
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, errors.New("--reason is required to close a task without integration")
	}
	t, e := getTask(repo, id)
	if e != nil {
		return nil, e
	}
	if t.Status == "closed" {
		return nil, fmt.Errorf("task %s is already closed; nothing changed", id)
	}
	rs, e := s.Runs()
	if e != nil {
		return nil, e
	}
	blocking := []string{}
	for _, r := range rs {
		if r.RepoID == repo.ID && r.Task == id && r.State != "discarded" && r.State != "cleaned" {
			blocking = append(blocking, fmt.Sprintf("%s (%s, %s, process %s)", r.ID, r.Name, r.State, r.Process))
		}
	}
	if len(blocking) > 0 {
		return nil, fmt.Errorf("task %s has runs that may hold its claim or work: %s; stop and integrate them, or rk clean RUN --discard them first", id, strings.Join(blocking, ", "))
	}
	if t.Assignee != "" {
		return nil, fmt.Errorf("task %s is %s, claimed by %q outside any open run; release that claim first", id, t.Status, t.Assignee)
	}
	if _, e = beads(repo, "king", "close", id, "--reason", reason, "--transition-comment", "Closed by the king without integration: "+reason); e != nil {
		return nil, e
	}
	return getTask(repo, id)
}
func parseCommand(s string) ([]string, error) {
	var args []string
	if e := json.Unmarshal([]byte(s), &args); e != nil {
		return nil, errors.New("command must be a JSON array, e.g. '[\"claude\",\"{prompt}\"]'")
	}
	if len(args) == 0 {
		return nil, errors.New("command must not be empty")
	}
	return args, nil
}
