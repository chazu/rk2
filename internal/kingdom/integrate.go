package kingdom

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func targetFree(repo *Repo, branch string) error {
	out, e := gitOutput(repo.Path, "worktree", "list", "--porcelain")
	if e != nil {
		return e
	}
	for _, line := range strings.Split(out, "\n") {
		if line == "branch refs/heads/"+branch {
			return fmt.Errorf("target %s is checked out; refusing to change its HEAD/index; use an unchecked-out integration branch", branch)
		}
	}
	return nil
}

const noIntegrationCheck = "none configured"

func checkLabel(check []string) string {
	b, _ := json.Marshal(check)
	return string(b)
}

// integrationReport states what integration itself verified, separately from
// the rat's unverified --checks text.
func integrationReport(repo *Repo, r *Run) string {
	text := "Integrated locally into " + r.Target + " at " + r.IntegratedCommit + ". "
	if r.IntegrationCheck == noIntegrationCheck {
		text += "Integration check: none configured for this repository; no repository check ran. " + configureCheckHint(repo) + " "
	} else {
		text += "Integration check: " + strings.TrimPrefix(r.IntegrationCheck, "passed: ") + " passed. "
	}
	if strings.TrimSpace(r.Checks) != "" {
		text += "Rat-reported checks (not verified by integration): " + r.Checks
	}
	return strings.TrimSpace(text)
}
func configureCheckHint(repo *Repo) string {
	return "Configure one with: rk config --repo " + repo.Name + " --check JSON"
}
func (s *Store) Integrate(id string) (*Run, error) {
	unlock, e := s.lock()
	if e != nil {
		return nil, e
	}
	defer unlock()
	r, e := s.Run(id)
	if e != nil {
		return nil, e
	}
	repo, e := s.Repo(r.RepoID)
	if e != nil {
		return nil, e
	}
	if r.Role != "rat" {
		return nil, errors.New("only rats have integrable results")
	}
	if r.State == "integrated" {
		e = closeIntegrated(repo, r)
		se := s.saveRun(r)
		if e != nil {
			return r, e
		}
		return r, se
	}
	if r.State != "ready" && r.State != "integrating" {
		return nil, errors.New("run is not ready for review/integration")
	}
	alive, e := runAlive(s, r)
	if e != nil {
		return nil, e
	}
	session, e := sessionExists(r.Session)
	if e != nil {
		return nil, e
	}
	if alive || session || r.Process == "starting" {
		return nil, errors.New("stop the rat before integrating its result")
	}
	if e = ownTask(repo, r); e != nil {
		return nil, e
	}
	target, e := refOID(repo.Path, "refs/heads/"+r.Target)
	if e != nil {
		return nil, e
	}
	// Recover a crash after target update but before recording completion.
	if r.IntegratedCommit != "" && ancestor(repo.Path, r.IntegratedCommit, target) {
		r.State = "integrated"
		e = closeIntegrated(repo, r)
		se := s.saveRun(r)
		if e != nil {
			return r, e
		}
		return r, se
	}
	head, e := refOID(r.Worktree, "HEAD")
	if e != nil {
		return nil, e
	}
	if head != r.Commit {
		return nil, errors.New("worker HEAD changed since submission; resubmit the result")
	}
	if d, e := dirty(r.Worktree); e != nil {
		return nil, e
	} else if d {
		return nil, errors.New("worker worktree is dirty; preserve and resubmit")
	}
	if e = targetFree(repo, r.Target); e != nil {
		return nil, e
	}
	dir := filepath.Join(s.Root, "integration", r.ID+"-"+newID())
	if _, e = gitOutput(repo.Path, "worktree", "add", "--detach", dir, target); e != nil {
		return nil, e
	}
	fail := func(err error) (*Run, error) {
		r.State = "ready"
		r.Error = fmt.Sprintf("integration candidate preserved at %s: %v", dir, err)
		s.saveRun(r)
		return r, errors.New(r.Error)
	}
	if _, e = gitOutput(dir, "merge", "--no-ff", "--no-edit", r.Commit); e != nil {
		return fail(e)
	}
	r.IntegrationCheck = noIntegrationCheck
	if len(repo.Check) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, repo.Check[0], repo.Check[1:]...)
		cmd.Dir = dir
		cmd.Env = s.runEnv(repo, r)
		f, e := os.OpenFile(filepath.Join(s.runDir(r.ID), "integration-"+filepath.Base(dir)+".log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
		if e != nil {
			return fail(e)
		}
		cmd.Stdout = f
		cmd.Stderr = f
		e = cmd.Run()
		f.Close()
		if e != nil {
			r.IntegrationCheck = "failed: " + checkLabel(repo.Check) + ": " + e.Error()
			return fail(fmt.Errorf("integration check: %w", e))
		}
		r.IntegrationCheck = "passed: " + checkLabel(repo.Check)
	}
	candidate, e := refOID(dir, "HEAD")
	if e != nil {
		return fail(e)
	}
	if d, e := dirty(dir); e != nil {
		return fail(e)
	} else if d {
		return fail(errors.New("integration check left dirty or untracked files; target unchanged"))
	}
	r.IntegratedCommit = candidate
	r.State = "integrating"
	if e = s.saveRun(r); e != nil {
		return r, e
	}
	if e = targetFree(repo, r.Target); e != nil {
		return fail(e)
	}
	if _, e = gitOutput(repo.Path, "update-ref", "refs/heads/"+r.Target, candidate, target); e != nil {
		return fail(e)
	}
	r.State = "integrated"
	r.Error = ""
	queue(r, integrationReport(repo, r), "king")
	if e = s.saveRun(r); e != nil {
		return r, e
	}
	closeErr := closeIntegrated(repo, r)
	deliveryErr := deliver(repo, r)
	if e = s.saveRun(r); e != nil {
		return r, e
	}
	_, cleanupErr := gitOutput(repo.Path, "worktree", "remove", dir)
	if closeErr != nil {
		return r, fmt.Errorf("integrated; ticket closure pending (rk reconcile retries): %w", closeErr)
	}
	if deliveryErr != nil {
		return r, fmt.Errorf("integrated; handoff pending (rk reconcile retries): %w", deliveryErr)
	}
	if cleanupErr != nil {
		return r, fmt.Errorf("integrated; candidate cleanup retained at %s: %w", dir, cleanupErr)
	}
	return r, nil
}
func (s *Store) Clean(id string, discard bool) (*Run, error) {
	unlock, e := s.lock()
	if e != nil {
		return nil, e
	}
	defer unlock()
	r, e := s.Run(id)
	if e != nil {
		return nil, e
	}
	repo, e := s.Repo(r.RepoID)
	if e != nil {
		return nil, e
	}
	if r.Role != "rat" {
		return nil, errors.New("king has no disposable worktree")
	}
	alive, e := runAlive(s, r)
	if e != nil {
		return nil, e
	}
	exists, e := sessionExists(r.Session)
	if e != nil {
		return nil, e
	}
	if alive || exists || r.Process == "starting" {
		return nil, errors.New("stop the rat before cleanup")
	}
	if r.State == "cleaned" || r.State == "discarded" {
		return r, nil
	}
	if !discard {
		if r.State != "integrated" || !r.Closed {
			return nil, errors.New("cleanup requires integration and ticket closure; work preserved")
		}
		head := r.Commit
		if branchHead, refErr := refOID(repo.Path, "refs/heads/"+r.Branch); refErr == nil {
			head = branchHead
		}
		if _, statErr := os.Stat(r.Worktree); statErr == nil {
			currentBranch, branchErr := gitOutput(r.Worktree, "branch", "--show-current")
			if branchErr != nil {
				return nil, branchErr
			}
			if currentBranch != r.Branch {
				return nil, errors.New("worktree branch changed; refusing cleanup")
			}
			var headErr error
			head, headErr = refOID(r.Worktree, "HEAD")
			if headErr != nil {
				return nil, headErr
			}
			if d, e := dirty(r.Worktree); e != nil {
				return nil, e
			} else if d {
				return nil, errors.New("dirty worktree; refusing cleanup")
			}
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return nil, statErr
		}
		if head == "" || !ancestor(repo.Path, head, "refs/heads/"+r.Target) {
			return nil, errors.New("worker commits are not integrated")
		}

	}
	if _, e = os.Stat(r.Worktree); e == nil {
		args := []string{"worktree", "remove"}
		if discard {
			args = append(args, "--force")
		}
		args = append(args, r.Worktree)
		if _, e = gitOutput(repo.Path, args...); e != nil {
			return nil, e
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return nil, e
	}
	if _, e = refOID(repo.Path, "refs/heads/"+r.Branch); e == nil {
		head, e := refOID(repo.Path, "refs/heads/"+r.Branch)
		if e != nil {
			return nil, e
		}
		if !discard && !ancestor(repo.Path, head, "refs/heads/"+r.Target) {
			return nil, errors.New("branch changed during cleanup; retained")
		}
		if _, e = gitOutput(repo.Path, "update-ref", "-d", "refs/heads/"+r.Branch, head); e != nil {
			return nil, e
		}
	}
	r.State = "cleaned"
	if discard {
		r.State = "discarded"
		if r.Claimed {
			t, e := getTask(repo, r.Task)
			if e != nil {
				return nil, e
			}
			if t.Assignee == r.ID && t.Status == "in_progress" {
				if _, e = beads(repo, "king", "update", r.Task, "--status", "open", "--assignee", ""); e != nil {
					return nil, e
				}
			}
		}
		queue(r, "Work explicitly discarded by coordinator; ticket released.", "king")
	}
	if e = s.saveRun(r); e != nil {
		return nil, e
	}
	de := deliver(repo, r)
	if e = s.saveRun(r); e != nil {
		return nil, e
	}
	return r, de
}
func (s *Store) Publish(repoName, remote, branch, request string) error {
	if os.Getenv("RK_ROLE") == "rat" {
		return errors.New("rats cannot publish")
	}
	if strings.TrimSpace(request) == "" {
		return errors.New("--request must record the user's explicit push request")
	}
	repo, e := s.Repo(repoName)
	if e != nil {
		return e
	}
	if remote == "" || branch == "" {
		return errors.New("--remote and --branch are required")
	}
	if e = validateBranch(repo.Path, branch); e != nil {
		return e
	}
	remotes, e := gitOutput(repo.Path, "remote")
	if e != nil {
		return e
	}
	found := false
	for _, r := range strings.Fields(remotes) {
		if r == remote {
			found = true
		}
	}
	if !found {
		return errors.New("remote must be an existing named remote")
	}
	urls, e := gitOutput(repo.Path, "remote", "get-url", "--push", "--all", remote)
	if e != nil {
		return e
	}
	if len(strings.Split(urls, "\n")) != 1 {
		return errors.New("remote has multiple push URLs; select a single-destination remote")
	}
	oid, e := refOID(repo.Path, "refs/heads/"+branch)
	if e != nil {
		return e
	}
	receipt := filepath.Join(s.Root, "publication", newID()+".json")
	record := map[string]any{"repo": repo.ID, "remote": remote, "branch": branch, "commit": oid, "request": request, "status": "requested", "time": time.Now().UTC()}
	if e = atomicJSON(receipt, record); e != nil {
		return e
	}
	// Exact commit and destination; no force, deletion, wildcard, implicit tags or extra refs.
	_, e = gitOutput(repo.Path, "-c", "push.followTags=false", "-c", "remote."+remote+".mirror=false", "push", "--no-follow-tags", "--recurse-submodules=no", remote, oid+":refs/heads/"+branch)
	record["status"] = "published"
	if e != nil {
		record["status"] = "failed"
		record["error"] = e.Error()
	}
	se := atomicJSON(receipt, record)
	if e != nil {
		return e
	}
	return se
}
