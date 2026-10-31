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

func beadsExecutable() string { return realExecutable("br") }
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

// Beads resolves issue_prefix from the workspace's .beads/config.yaml, then
// user-level config (~/.config/beads, legacy ~/.beads), then the database. A
// workspace without its own prefix silently takes an unrelated project's from
// the user's home, so rk keeps one explicitly in the workspace config.
type RepoView struct {
	Repo
	IssuePrefix       string `json:"issue_prefix"`
	IssuePrefixSource string `json:"issue_prefix_source"`
}

func validPrefix(p string) bool {
	if p == "" || p[0] == '-' || p[len(p)-1] == '-' || strings.Contains(p, "--") {
		return false
	}
	for _, c := range p {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

// defaultPrefix sanitises a repository name: lowercase, runs of characters
// outside [a-z0-9] become one hyphen, and edge hyphens are dropped.
func defaultPrefix(name string) string {
	var b strings.Builder
	dash := false
	for _, c := range strings.ToLower(name) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			b.WriteRune(c)
			dash = false
		} else {
			dash = true
		}
	}
	return b.String()
}

// workspacePrefix reports the issue_prefix set in the workspace's own config
// file, or "" when the workspace inherits one. br has no scoped get, so this
// reads the config file; it never reads or writes the database.
func workspacePrefix(repo *Repo) (string, error) {
	b, e := os.ReadFile(filepath.Join(repo.BeadsDir, "config.yaml"))
	if errors.Is(e, os.ErrNotExist) {
		return "", nil
	}
	if e != nil {
		return "", e
	}
	for _, line := range strings.Split(string(b), "\n") {
		v, ok := strings.CutPrefix(line, "issue_prefix:")
		if !ok {
			continue
		}
		if i := strings.Index(v, " #"); i >= 0 {
			v = v[:i]
		}
		return strings.Trim(strings.TrimSpace(v), `"'`), nil
	}
	return "", nil
}
func effectivePrefix(repo *Repo) (string, error) {
	b, e := beads(repo, "king", "config", "get", "issue_prefix")
	if e != nil {
		return "", e
	}
	var v struct {
		Value any `json:"value"`
	}
	if e = json.Unmarshal(b, &v); e != nil {
		return "", e
	}
	if v.Value == nil {
		return "", nil
	}
	return fmt.Sprint(v.Value), nil
}
func setPrefix(repo *Repo, prefix string) error {
	if !validPrefix(prefix) {
		return fmt.Errorf("invalid issue prefix %q: use lowercase letters, digits and single inner hyphens", prefix)
	}
	if _, e := beads(repo, "king", "config", "set", "issue_prefix", prefix); e != nil {
		return e
	}
	if got, e := workspacePrefix(repo); e != nil || got != prefix {
		return fmt.Errorf("issue_prefix %q was not recorded in %s (found %q, %v)", prefix, filepath.Join(repo.BeadsDir, "config.yaml"), got, e)
	}
	return nil
}

// ensurePrefix gives the workspace an explicit prefix: the requested one, or
// fallback when the workspace has none of its own. An existing explicit prefix
// is left alone unless a different one is requested.
func ensurePrefix(repo *Repo, requested, fallback string) error {
	own, e := workspacePrefix(repo)
	if e != nil {
		return e
	}
	switch {
	case requested != "" && requested != own:
		return setPrefix(repo, requested)
	case requested == "" && own == "":
		if !validPrefix(fallback) {
			return fmt.Errorf("workspace has no issue_prefix and repository name gives no usable default %q; pass --prefix", fallback)
		}
		return setPrefix(repo, fallback)
	}
	return nil
}

// initWorkspace creates the repository's Beads workspace if absent, then makes
// sure it has an explicit prefix. br init --prefix only records the prefix in
// the database, which user-level config overrides, hence ensurePrefix.
func initWorkspace(repo *Repo, requested, fallback string) error {
	if _, e := os.Stat(repo.BeadsDir); errors.Is(e, os.ErrNotExist) {
		if !validPrefix(fallback) {
			return fmt.Errorf("repository name gives no usable issue prefix %q; pass --prefix", fallback)
		}
		if _, e = beads(repo, "king", "init", "--prefix", fallback); e != nil {
			return e
		}
	} else if e != nil {
		return e
	}
	return ensurePrefix(repo, requested, fallback)
}
func viewRepo(repo *Repo) RepoView {
	v := RepoView{Repo: *repo, IssuePrefixSource: "workspace"}
	own, e := workspacePrefix(repo)
	if e != nil {
		v.IssuePrefixSource = "unknown: " + e.Error()
		return v
	}
	if own == "" {
		v.IssuePrefixSource = "inherited (not set in workspace; use rk config --prefix)"
	}
	if v.IssuePrefix, e = effectivePrefix(repo); e != nil {
		v.IssuePrefixSource = "unknown: " + e.Error()
	}
	return v
}
