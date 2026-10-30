package kingdom

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

type Task struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Status      string `json:"status"`
	Assignee    string `json:"assignee"`
	Type        string `json:"issue_type"`
}
type Comment struct {
	ID     int    `json:"id"`
	Author string `json:"author"`
	Text   string `json:"text"`
}

func beadsExecutable() string { return realExecutable("RK_REAL_BR", "br") }
func beads(repo *Repo, actor string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	opts := []string{}
	for _, a := range args {
		if a != "--json" && a != "--no-auto-import" && a != "--no-auto-flush" {
			opts = append(opts, a)
		}
	}
	opts = append(opts, "--json", "--actor", actor, "--no-auto-import", "--no-auto-flush")
	cmd := exec.CommandContext(ctx, beadsExecutable(), opts...)
	cmd.Dir = repo.Path
	cmd.Env = cleanEnv(map[string]string{"BEADS_DIR": repo.BeadsDir, "RUST_LOG": "error"})
	out, e := cmd.CombinedOutput()
	if e != nil {
		return nil, fmt.Errorf("br %s: %w: %s", strings.Join(args, " "), e, strings.TrimSpace(string(out)))
	}
	return out, nil
}
func decodeRows[T any](data []byte, key string) ([]T, error) {
	var rows []T
	if e := json.Unmarshal(data, &rows); e == nil {
		return rows, nil
	}
	var obj map[string]json.RawMessage
	if e := json.Unmarshal(data, &obj); e != nil {
		return nil, e
	}
	if v, ok := obj[key]; ok {
		if e := json.Unmarshal(v, &rows); e != nil {
			return nil, e
		}
		return rows, nil
	}
	if _, ok := obj["id"]; ok {
		var one T
		if e := json.Unmarshal(data, &one); e != nil {
			return nil, e
		}
		return []T{one}, nil
	}
	return nil, fmt.Errorf("unsupported br JSON shape: expected array, object with %q, or issue", key)
}
func getTask(repo *Repo, id string) (*Task, error) {
	b, e := beads(repo, "rk", "show", id)
	if e != nil {
		return nil, e
	}
	ts, e := decodeRows[Task](b, "issues")
	if e != nil {
		return nil, e
	}
	if len(ts) != 1 {
		return nil, errors.New("expected one task")
	}
	return &ts[0], nil
}
func readyTasks(repo *Repo) ([]Task, error) {
	b, e := beads(repo, "rk", "ready", "--unassigned", "--limit", "0")
	if e != nil {
		return nil, e
	}
	return decodeRows[Task](b, "issues")
}
func comments(repo *Repo, id string) ([]Comment, error) {
	b, e := beads(repo, "rk", "comments", "list", id)
	if e != nil {
		return nil, e
	}
	return decodeRows[Comment](b, "comments")
}
func claimTask(repo *Repo, r *Run) error {
	t, e := getTask(repo, r.Task)
	if e != nil {
		return e
	}
	if t.Assignee == r.ID && t.Status == "in_progress" {
		return nil
	}
	_, e = beads(repo, r.ID, "update", r.Task, "--claim")
	if e != nil {
		return e
	}
	t, e = getTask(repo, r.Task)
	if e != nil {
		return e
	}
	if t.Assignee != r.ID || t.Status != "in_progress" {
		return errors.New("claim did not establish expected ownership")
	}
	return nil
}
func ownTask(repo *Repo, r *Run) error {
	t, e := getTask(repo, r.Task)
	if e != nil {
		return e
	}
	if t.Assignee != r.ID || t.Status != "in_progress" {
		return fmt.Errorf("task %s is %s, owned by %q; expected in_progress owned by %s", r.Task, t.Status, t.Assignee, r.ID)
	}
	return nil
}
func deliver(repo *Repo, r *Run) error {
	for i := range r.Messages {
		m := &r.Messages[i]
		if m.Delivered {
			continue
		}
		cs, e := comments(repo, m.Task)
		if e != nil {
			return e
		}
		marker := "[rk:" + m.ID + "]"
		found := false
		for _, c := range cs {
			if strings.Contains(c.Text, marker) {
				found = true
				break
			}
		}
		if !found {
			if _, e = beads(repo, messageAuthor(m, r.ID), "comments", "add", m.Task, marker+" "+m.Text); e != nil {
				return e
			}
		}
		m.Delivered = true
	}
	return nil
}
func messageAuthor(m *Message, fallback string) string {
	if m.Author != "" {
		return m.Author
	}
	return fallback
}
func queue(r *Run, text string, author ...string) {
	actor := r.ID
	if len(author) > 0 {
		actor = author[0]
	}
	if r.Task != "" {
		r.Messages = append(r.Messages, Message{Author: actor, ID: newID(), Task: r.Task, Text: text, Created: time.Now().UTC()})
	}
}
func GuardedBeads(args []string) error {
	if len(args) == 0 {
		return errors.New("Beads subcommand required")
	}
	allowed := " ready show list blocked dep graph search stats comments create "
	if !strings.Contains(allowed, " "+args[0]+" ") {
		return fmt.Errorf("br %s denied in managed session; use rk lifecycle commands", args[0])
	}
	if args[0] == "dep" && (len(args) < 2 || (args[1] != "list" && args[1] != "tree")) {
		return errors.New("worker dependency changes require the king")
	}
	for _, a := range args {
		if strings.HasPrefix(a, "--db") || strings.HasPrefix(a, "--actor") || strings.HasPrefix(a, "--status") || strings.HasPrefix(a, "--assignee") || a == "--force" {
			return fmt.Errorf("Beads option %q denied", a)
		}
	}
	p := os.Getenv("RK_REAL_BR")
	if p == "" {
		return errors.New("missing managed Beads executable")
	}
	if isGuard(p) {
		return errors.New("managed Beads executable is the rk guard itself; restart the run")
	}
	args = append(args, "--no-auto-import", "--no-auto-flush", "--actor", os.Getenv("RK_RUN"))
	cmd := exec.Command(p, args...)
	cmd.Env = os.Environ()
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
