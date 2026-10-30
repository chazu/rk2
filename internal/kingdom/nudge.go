package kingdom

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Notices are short lines typed into a run's own tmux session, so an agent
// sitting at its prompt learns of new messages or results. They are text for
// the agent to read; rk never types commands for it to run.
const noticePrefix = "[rk] "

func nudgeInterval() time.Duration {
	if d, e := time.ParseDuration(os.Getenv("RK_NUDGE_INTERVAL")); e == nil && d > 0 {
		return d
	}
	return 5 * time.Second
}

// nudgeLoop runs in the launcher while the agent lives. A notice that cannot
// be typed yet is retried on the next tick rather than marked delivered.
func (s *Store) nudgeLoop(id string, attempt int, done <-chan struct{}) {
	interval := nudgeInterval()
	// Give the agent's interface time to draw before typing into it.
	select {
	case <-done:
		return
	case <-time.After(2 * interval):
	}
	seen := map[string]string{}
	cursor := 0
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		r, e := s.Run(id)
		if e != nil || r.Attempt != attempt {
			return
		}
		var text string
		var commit func()
		switch r.Role {
		case "rat":
			text, commit = s.ratNotice(r, cursor, func(c int) { cursor = c })
		case "king":
			text, commit = s.kingNotice(seen)
		}
		if text != "" && !recentlyTyped(r.Session, interval) && typeNotice(r.Session, text) == nil {
			commit()
		}
		select {
		case <-done:
			return
		case <-tick.C:
		}
	}
}

// ratNotice reports task comments from others newer than both the rat's
// acknowledged cursor and the last comment it was told about.
func (s *Store) ratNotice(r *Run, told int, setTold func(int)) (string, func()) {
	repo, e := s.Repo(r.RepoID)
	if e != nil || r.Task == "" {
		return "", nil
	}
	cs, e := comments(repo, r.Task)
	if e != nil {
		return "", nil
	}
	newest, n := 0, 0
	for _, c := range cs {
		if c.ID <= r.Cursor || c.ID <= told || c.Author == r.ID || c.Author == r.Name {
			continue
		}
		n++
		if c.ID > newest {
			newest = c.ID
		}
	}
	if n == 0 {
		return "", nil
	}
	text := fmt.Sprintf("%s%d new message(s) on task %s. Run: rk inbox, act on them, then rk inbox --ack %d", noticePrefix, n, r.Task, newest)
	return text, func() { setTold(newest) }
}

// kingNotice reports rats whose result or process state needs the king and
// that have changed since the last notice.
func (s *Store) kingNotice(seen map[string]string) (string, func()) {
	rs, e := s.Runs()
	if e != nil {
		return "", nil
	}
	next := map[string]string{}
	news := []string{}
	for _, r := range rs {
		if r.Role != "rat" || !needsKing(r) {
			continue
		}
		key := fmt.Sprintf("%d:%s:%s", r.Attempt, r.State, r.Process)
		next[r.ID] = key
		if seen[r.ID] == key {
			continue
		}
		status := r.State
		if r.State == "working" {
			status = "process " + r.Process
		}
		repo := r.RepoID
		if rp, e := s.Repo(r.RepoID); e == nil {
			repo = rp.Name
		}
		news = append(news, fmt.Sprintf("%s (%s) %s", r.Name, repo, status))
	}
	if len(news) == 0 {
		return "", nil
	}
	sort.Strings(news)
	text := noticePrefix + "Rat updates: " + strings.Join(news, "; ") + ". Review with: rk wait --timeout 1s"
	return text, func() {
		for k := range seen {
			delete(seen, k)
		}
		for k, v := range next {
			seen[k] = v
		}
	}
}

// needsKing matches the runs rk wait returns to the king.
func needsKing(r *Run) bool {
	return r.State == "ready" || r.State == "blocked" || r.State == "startup_failed" || (r.State == "working" && (r.Process == "exited" || r.Process == "unknown"))
}

// recentlyTyped reports whether a person attached to the session pressed a
// key within the window, so a notice would land in the middle of their input.
func recentlyTyped(session string, window time.Duration) bool {
	out, e := exec.Command("tmux", "list-clients", "-t", "="+session, "-F", "#{client_activity}").Output()
	if e != nil {
		return false
	}
	for _, line := range strings.Fields(string(out)) {
		if t, e := strconv.ParseInt(line, 10, 64); e == nil && time.Since(time.Unix(t, 0)) < window {
			return true
		}
	}
	return false
}

// typeNotice cuts any draft on the input line into the kill ring (restore it
// with C-y), types one line literally, then submits it separately so the
// agent does not read the Enter as part of a paste.
func typeNotice(session, text string) error {
	text = strings.ReplaceAll(text, "\n", " ")
	if out, e := exec.Command("tmux", "send-keys", "-t", "="+session+":", "C-a", "C-k").CombinedOutput(); e != nil {
		return fmt.Errorf("clear input: %w: %s", e, out)
	}
	if out, e := exec.Command("tmux", "send-keys", "-t", "="+session+":", "-l", text).CombinedOutput(); e != nil {
		return fmt.Errorf("type notice: %w: %s", e, out)
	}
	time.Sleep(300 * time.Millisecond)
	if out, e := exec.Command("tmux", "send-keys", "-t", "="+session+":", "Enter").CombinedOutput(); e != nil {
		return fmt.Errorf("submit notice: %w: %s", e, out)
	}
	return nil
}
