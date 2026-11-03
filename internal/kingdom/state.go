package kingdom

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

type Repo struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Path      string   `json:"path"`
	CommonDir string   `json:"common_dir"`
	BeadsDir  string   `json:"beads_dir"`
	Target    string   `json:"target"`
	Command   []string `json:"command"`
	MaxRats   int      `json:"max_rats"`
	Check     []string `json:"check,omitempty"`
}

type Message struct {
	Author    string    `json:"author"`
	ID        string    `json:"id"`
	Task      string    `json:"task"`
	Text      string    `json:"text"`
	Delivered bool      `json:"delivered"`
	Created   time.Time `json:"created"`
}

type Run struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	Role             string    `json:"role"`
	RepoID           string    `json:"repo_id"`
	Task             string    `json:"task,omitempty"`
	State            string    `json:"state"`
	Worktree         string    `json:"worktree"`
	Branch           string    `json:"branch"`
	Target           string    `json:"target"`
	Session          string    `json:"session"`
	Attempt          int       `json:"attempt"`
	PID              int       `json:"pid,omitempty"`
	ChildPID         int       `json:"child_pid,omitempty"`
	Process          string    `json:"process"`
	ExitCode         *int      `json:"exit_code,omitempty"`
	Created          time.Time `json:"created"`
	Updated          time.Time `json:"updated"`
	Commit           string    `json:"commit,omitempty"`
	IntegratedCommit string    `json:"integrated_commit,omitempty"`
	IntegrationCheck string    `json:"integration_check,omitempty"`
	Summary          string    `json:"summary,omitempty"`
	Checks           string    `json:"checks,omitempty"`
	Error            string    `json:"error,omitempty"`
	Claimed          bool      `json:"claimed"`
	Closed           bool      `json:"closed"`
	Cursor           int       `json:"comment_cursor,omitempty"`
	Messages         []Message `json:"messages,omitempty"`
}

type Store struct{ Root string }

func Open(root string) (*Store, error) {
	if root == "" {
		root = os.Getenv("RK_HOME")
	}
	if root == "" {
		h, e := os.UserHomeDir()
		if e != nil {
			return nil, e
		}
		root = filepath.Join(h, ".rat-kingdom")
	}
	p, e := filepath.Abs(root)
	if e != nil {
		return nil, e
	}
	for _, d := range []string{p, filepath.Join(p, "repos"), filepath.Join(p, "runs"), filepath.Join(p, "worktrees"), filepath.Join(p, "integration")} {
		if e = os.MkdirAll(d, 0700); e != nil {
			return nil, e
		}
	}
	return &Store{Root: p}, nil
}
func (s *Store) lock() (func(), error) {
	f, e := os.OpenFile(filepath.Join(s.Root, "state.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX); e != nil {
		f.Close()
		return nil, e
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}
func atomicJSON(path string, v any) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	b = append(b, '\n')
	return atomicFile(path, b, 0600)
}
func atomicFile(path string, b []byte, mode os.FileMode) error {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".write-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if e = f.Chmod(mode); e == nil {
		_, e = f.Write(b)
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	if e = os.Rename(f.Name(), path); e != nil {
		return e
	}
	d, e := os.Open(filepath.Dir(path))
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
func readJSON(path string, v any) error {
	b, e := os.ReadFile(path)
	if e != nil {
		return e
	}
	return json.Unmarshal(b, v)
}
func safeID(id string) bool {
	return id != "" && filepath.Base(id) == id && id != "." && id != ".." && !strings.ContainsAny(id, "\\\x00")
}
func (s *Store) runDir(id string) string { return filepath.Join(s.Root, "runs", id) }

// King configures the system-wide king, which belongs to no repository.
type King struct {
	Command []string `json:"command"`
}

var defaultCommand = []string{"claude", "{prompt}"}

// kingDir is the king's working directory: its own place under RK_HOME, not
// any registered repository, so it starts from anywhere and outlives them all.
func (s *Store) kingDir() string { return filepath.Join(s.Root, "king") }

// KingConfig reads RK_HOME/king.json, falling back to the default command.
func (s *Store) KingConfig() (*King, error) {
	k := &King{}
	if e := readJSON(filepath.Join(s.Root, "king.json"), k); e != nil && !errors.Is(e, os.ErrNotExist) {
		return nil, e
	}
	if len(k.Command) == 0 {
		k.Command = append([]string{}, defaultCommand...)
	}
	return k, nil
}

// runRepo returns the repository a rat serves. Kings have none; looking one up
// with an empty ID would fall back to the current directory's repository.
func (s *Store) runRepo(r *Run) (*Repo, error) {
	if r.RepoID == "" {
		return nil, fmt.Errorf("run %s belongs to no repository", r.ID)
	}
	return s.Repo(r.RepoID)
}
func (s *Store) saveRun(r *Run) error {
	if !safeID(r.ID) {
		return errors.New("invalid run ID")
	}
	r.Updated = time.Now().UTC()
	return atomicJSON(filepath.Join(s.runDir(r.ID), "run.json"), r)
}
func (s *Store) Run(id string) (*Run, error) {
	if !safeID(id) {
		return nil, errors.New("invalid run ID")
	}
	var r Run
	if e := readJSON(filepath.Join(s.runDir(id), "run.json"), &r); e == nil {
		return &r, nil
	} else if !errors.Is(e, os.ErrNotExist) {
		return nil, e
	}
	rs, e := s.Runs()
	if e != nil {
		return nil, e
	}
	var found *Run
	for _, r := range rs {
		if r.Name == id {
			if found != nil {
				return nil, fmt.Errorf("ambiguous name %q; use run ID", id)
			}
			found = r
		}
	}
	if found == nil {
		return nil, fmt.Errorf("run %q not found", id)
	}
	return found, nil
}

// King returns the system-wide king, ignoring dismissed ones. State from before
// kings were unique may hold several; the active one wins, then the most
// recently created.
func (s *Store) King() (*Run, error) {
	rs, e := s.Runs()
	if e != nil {
		return nil, e
	}
	var found *Run
	for _, r := range rs {
		if r.Role != "king" || r.State == "dismissed" {
			continue
		}
		if found == nil || activeState(r) && !activeState(found) || activeState(r) == activeState(found) && r.Created.After(found.Created) {
			found = r
		}
	}
	if found == nil {
		return nil, errors.New("no king; use rk spawn king")
	}
	return found, nil
}
func (s *Store) Runs() ([]*Run, error) {
	es, e := os.ReadDir(filepath.Join(s.Root, "runs"))
	if e != nil {
		return nil, e
	}
	rs := []*Run{}
	for _, d := range es {
		if !d.IsDir() {
			continue
		}
		var r Run
		if e = readJSON(filepath.Join(s.runDir(d.Name()), "run.json"), &r); errors.Is(e, os.ErrNotExist) {
			continue
		} else if e != nil {
			return nil, e
		}
		rs = append(rs, &r)
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].Created.Before(rs[j].Created) })
	return rs, nil
}
func (s *Store) Repos() ([]Repo, error) {
	es, e := os.ReadDir(filepath.Join(s.Root, "repos"))
	if e != nil {
		return nil, e
	}
	rs := []Repo{}
	for _, d := range es {
		if !strings.HasSuffix(d.Name(), ".json") {
			continue
		}
		var r Repo
		if e = readJSON(filepath.Join(s.Root, "repos", d.Name()), &r); e != nil {
			return nil, e
		}
		rs = append(rs, r)
	}
	return rs, nil
}
func (s *Store) Repo(id string) (*Repo, error) {
	rs, e := s.Repos()
	if e != nil {
		return nil, e
	}
	if id == "" {
		cwd, e := os.Getwd()
		if e != nil {
			return nil, e
		}
		common, e := gitOutput(cwd, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if e != nil {
			return nil, errors.New("use --repo NAME outside a registered repository")
		}
		common, _ = filepath.EvalSymlinks(common)
		for _, r := range rs {
			if r.CommonDir == common {
				return &r, nil
			}
		}
	}
	for _, r := range rs {
		if r.ID == id || r.Name == id {
			return &r, nil
		}
	}
	return nil, fmt.Errorf("repository %q not registered; use rk init", id)
}
func (s *Store) saveRepo(r *Repo) error {
	return atomicJSON(filepath.Join(s.Root, "repos", r.ID+".json"), r)
}
func repoID(path string) string { h := sha256.Sum256([]byte(path)); return hex.EncodeToString(h[:6]) }
func newID() string {
	var b [6]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b[:])
}
func activeState(r *Run) bool {
	return r.Process == "starting" || r.Process == "running" || r.Process == "unknown"
}
func processLock(path string) (*os.File, error) {
	f, e := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		f.Close()
		return nil, e
	}
	return f, nil
}
func runAlive(s *Store, r *Run) (bool, error) {
	f, e := processLock(filepath.Join(s.runDir(r.ID), "process.lock"))
	if errors.Is(e, syscall.EWOULDBLOCK) {
		return true, nil
	}
	if e != nil {
		return false, e
	}
	f.Close()
	return false, nil
}
