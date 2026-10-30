package kingdom

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// Watch reports durable state/comments. It never injects text into a shell.
func (s *Store) Watch(interval string, w io.Writer) error {
	d, e := time.ParseDuration(interval)
	if e != nil || d < time.Second {
		return fmt.Errorf("interval must be at least 1s")
	}
	ticker := time.NewTicker(d)
	defer ticker.Stop()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	seen := map[string]string{}
	for {
		runs, e := s.Reconcile()
		if e != nil {
			return e
		}
		for _, r := range runs {
			key := r.State + ":" + r.Process + ":" + r.Error
			if seen[r.ID] != key {
				fmt.Fprintf(w, "%s %s %s/%s %s\n", time.Now().Format(time.RFC3339), r.Name, r.State, r.Process, r.Error)
				seen[r.ID] = key
			}
			if r.Task != "" && r.State != "cleaned" && r.State != "discarded" {
				cs, e := s.Inbox(r.ID, 0)
				if e != nil {
					fmt.Fprintf(w, "%s inbox error: %v\n", r.ID, e)
					continue
				}
				for _, c := range cs {
					k := fmt.Sprintf("comment:%s:%d", r.ID, c.ID)
					if seen[k] == "" {
						fmt.Fprintf(w, "%s comment %d by %s: %s\n", r.Name, c.ID, c.Author, c.Text)
						seen[k] = "shown"
					}
				}
			}
		}
		select {
		case <-sig:
			return nil
		case <-ticker.C:
		}
	}
}

// Wait gives agents a bounded blocking operation instead of typing into their TUI.
// Results and unacknowledged comments remain durable until acted upon.
func (s *Store) Wait(repoName, id, timeout string, w io.Writer) error {
	d, e := time.ParseDuration(timeout)
	if e != nil || d < time.Second || d > time.Minute {
		return fmt.Errorf("timeout must be between 1s and 60s")
	}
	deadline := time.Now().Add(d)
	for {
		runs, e := s.Reconcile()
		if e != nil {
			return e
		}
		repoID := ""
		if repoName != "" {
			repo, e := s.Repo(repoName)
			if e != nil {
				return e
			}
			repoID = repo.ID
		}
		if id != "" {
			r, e := s.Run(id)
			if e != nil {
				return e
			}
			cs, e := s.Inbox(r.ID, 0)
			if e != nil {
				return e
			}
			if len(cs) > 0 {
				return emit(w, map[string]any{"comments": cs})
			}
		} else {
			results := []*Run{}
			active := 0
			for _, r := range runs {
				if repoID != "" && r.RepoID != repoID {
					continue
				}
				if r.Role != "rat" {
					continue
				}
				if activeState(r) {
					active++
				}
				if r.State == "ready" || r.State == "blocked" || r.State == "startup_failed" || (r.State == "working" && (r.Process == "exited" || r.Process == "unknown")) {
					results = append(results, r)
				}
			}
			if len(results) > 0 {
				return emit(w, map[string]any{"runs": results})
			}
			if active == 0 {
				return emit(w, map[string]string{"status": "idle"})
			}
		}
		if time.Now().After(deadline) {
			return emit(w, map[string]string{"status": "timeout"})
		}
		time.Sleep(250 * time.Millisecond)
	}
}
