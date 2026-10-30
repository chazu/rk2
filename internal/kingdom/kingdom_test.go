package kingdom

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestGitGuard(t *testing.T) {
	denied := [][]string{{"push"}, {"-C", "/tmp/repo", "push", "--no-verify"}, {"notes", "add"}, {"p"}, {"-c", "alias.p=push", "p"}, {"--config-env=core.hooksPath=ESCAPE", "commit"}, {"--exec-path=/tmp", "status"}, {"show", "refs/notes/commits"}, {"fetch", "--upload-pack=evil"}, {"rebase", "--exec", "git push"}, {"commit", "--no-verify"}}
	for _, args := range denied {
		if e := GuardGit(args); e == nil {
			t.Errorf("allowed %v", args)
		}
	}
	allowed := [][]string{{"status", "--porcelain"}, {"-C", "/tmp/repo", "commit", "-m", "fix push button"}, {"diff", "HEAD~1"}, {"--no-pager", "log", "-3"}}
	for _, args := range allowed {
		if e := GuardGit(args); e != nil {
			t.Errorf("rejected %v: %v", args, e)
		}
	}
}
func TestProviderGuard(t *testing.T) {
	for _, cmd := range []string{"git push origin main", "/usr/bin/git push", "git -C . push --no-verify", "git notes add", "git config core.hooksPath /dev/null"} {
		b, _ := json.Marshal(map[string]any{"tool_input": map[string]string{"command": cmd}})
		if e := CheckShell(bytes.NewReader(b)); e == nil {
			t.Errorf("allowed %s", cmd)
		}
	}
	if e := CheckShell(strings.NewReader(`{"tool_input":{"command":"go test ./..."}}`)); e != nil {
		t.Fatal(e)
	}
}
func TestStateAtomicReadersAndLock(t *testing.T) {
	s, e := Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	r := &Run{ID: "abc", State: "working"}
	if e = s.saveRun(r); e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	for n := 0; n < 8; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 30; i++ {
				unlock, e := s.lock()
				if e != nil {
					t.Error(e)
					return
				}
				v, e := s.Run("abc")
				if e == nil {
					v.Attempt++
					e = s.saveRun(v)
				}
				unlock()
				if e != nil {
					t.Error(e)
					return
				}
			}
		}()
	}
	for i := 0; i < 80; i++ {
		if _, e = s.Run("abc"); e != nil {
			t.Fatal(e)
		}
	}
	wg.Wait()
	r, e = s.Run("abc")
	if e != nil {
		t.Fatal(e)
	}
	if r.Attempt != 240 {
		t.Fatal(r.Attempt)
	}
	if _, e = s.Run("../escape"); e == nil {
		t.Fatal("accepted traversal")
	}
}
func TestProcessLockSurvivesStateChanges(t *testing.T) {
	s, _ := Open(t.TempDir())
	r := &Run{ID: "abc"}
	s.saveRun(r)
	f, e := processLock(filepath.Join(s.runDir(r.ID), "process.lock"))
	if e != nil {
		t.Fatal(e)
	}
	alive, e := runAlive(s, r)
	if e != nil || !alive {
		t.Fatalf("alive=%v err=%v", alive, e)
	}
	f.Close()
	alive, e = runAlive(s, r)
	if e != nil || alive {
		t.Fatalf("alive=%v err=%v", alive, e)
	}
}
func TestBeadsShapes(t *testing.T) {
	for _, data := range []string{`[{"id":"a"}]`, `{"issues":[{"id":"a"}],"total":1}`, `{"id":"a"}`} {
		rows, e := decodeRows[Task]([]byte(data), "issues")
		if e != nil || len(rows) != 1 || rows[0].ID != "a" {
			t.Fatalf("%s: %v %v", data, rows, e)
		}
	}
	if _, e := decodeRows[Task]([]byte(`{"unexpected":[]}`), "issues"); e == nil {
		t.Fatal("silent schema mismatch")
	}
}
func TestGuardedWorkerCannotControlOtherRuns(t *testing.T) {
	s, _ := Open(t.TempDir())
	s.saveRun(&Run{ID: "mine"})
	s.saveRun(&Run{ID: "other"})
	t.Setenv("RK_ROLE", "rat")
	t.Setenv("RK_RUN", "mine")
	for _, args := range [][]string{{"publish", "--request", "invented"}, {"spawn", "rat"}, {"clean", "mine", "--discard"}, {"show", "other"}, {"stop", "other"}} {
		args = append(args, "--home", s.Root)
		if e := Main(args, &bytes.Buffer{}); e == nil {
			t.Fatalf("allowed %v", args)
		}
	}
}
func TestInheritedGuardIsNotTheRealExecutable(t *testing.T) {
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	guard, real := t.TempDir(), t.TempDir()
	if e = os.Symlink(exe, filepath.Join(guard, "git")); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(real, "git"), []byte("#!/bin/sh\n"), 0700); e != nil {
		t.Fatal(e)
	}
	t.Setenv("PATH", guard+string(os.PathListSeparator)+real)
	t.Setenv("RK_REAL_GIT", filepath.Join(guard, "git"))
	if p := gitExecutable(); p != filepath.Join(real, "git") {
		t.Fatalf("resolved %q", p)
	}
	if e = GuardedGit([]string{"status"}); e == nil || !strings.Contains(e.Error(), "guard itself") {
		t.Fatalf("guard exec'd itself: %v", e)
	}
	if env := strings.Join(launchEnv(), " "); !strings.Contains(env, "-e RK_REAL_GIT="+filepath.Join(real, "git")) {
		t.Fatalf("launch env %q", env)
	}
}
func TestRunPathDropsOtherRunsGuards(t *testing.T) {
	s, _ := Open(t.TempDir())
	king := filepath.Join(s.Root, "runs", "king", "bin")
	t.Setenv("PATH", king+string(os.PathListSeparator)+"/usr/bin")
	got := s.runPath(&Run{ID: "rat"})
	want := filepath.Join(s.Root, "runs", "rat", "bin") + string(os.PathListSeparator) + "/usr/bin"
	if got != want {
		t.Fatalf("PATH %q, want %q", got, want)
	}
}
func TestPublishRequiresRequestBeforeGit(t *testing.T) {
	s, _ := Open(t.TempDir())
	if e := s.Publish("missing", "origin", "main", ""); e == nil || !strings.Contains(e.Error(), "request") {
		t.Fatal(e)
	}
	t.Setenv("RK_ROLE", "rat")
	if e := s.Publish("missing", "origin", "main", "please push"); e == nil || !strings.Contains(e.Error(), "rats") {
		t.Fatal(e)
	}
}
func TestAtomicFileModes(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nested", "state.json")
	if e := atomicFile(p, []byte("hello"), 0600); e != nil {
		t.Fatal(e)
	}
	st, e := os.Stat(p)
	if e != nil {
		t.Fatal(e)
	}
	if st.Mode().Perm() != 0600 {
		t.Fatal(st.Mode())
	}
}

func TestPublicationPinsOneRefAndDoesNotActuallyPush(t *testing.T) {
	s, _ := Open(t.TempDir())
	repo := &Repo{ID: "example", Name: "example", Path: t.TempDir()}
	if e := s.saveRepo(repo); e != nil {
		t.Fatal(e)
	}
	log := filepath.Join(t.TempDir(), "argv")
	fake := filepath.Join(t.TempDir(), "git")
	script := `#!/bin/sh
shift 2
case "$1" in
check-ref-format) exit 0 ;;
remote) if [ "$2" = get-url ]; then echo file:///disposable-placeholder; else echo origin; fi ;;
rev-parse) echo aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa ;;
-c) printf '%s\n' "$@" > "$RK_TEST_LOG" ;;
*) exit 99 ;;
esac
`
	if e := os.WriteFile(fake, []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	t.Setenv("RK_REAL_GIT", fake)
	t.Setenv("RK_TEST_LOG", log)
	if e := s.Publish(repo.ID, "origin", "feature", "Please push feature"); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(log)
	if e != nil {
		t.Fatal(e)
	}
	args := string(b)
	if !strings.Contains(args, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa:refs/heads/feature\n") {
		t.Fatal(args)
	}
	if strings.Contains(args, "--force") || strings.Contains(args, "--all") || strings.Contains(args, "--mirror") {
		t.Fatal(args)
	}
	if !strings.Contains(args, "--recurse-submodules=no") {
		t.Fatal(args)
	}
	files, _ := filepath.Glob(filepath.Join(s.Root, "publication", "*.json"))
	if len(files) != 1 {
		t.Fatal(files)
	}
	var receipt map[string]any
	if e = readJSON(files[0], &receipt); e != nil {
		t.Fatal(e)
	}
	if receipt["status"] != "published" || receipt["request"] != "Please push feature" {
		t.Fatal(receipt)
	}
}

func TestLocalBeadsExclusionPreservesExistingRulesAndIsIdempotent(t *testing.T) {
	common := t.TempDir()
	path := filepath.Join(common, "info", "exclude")
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		t.Fatal(e)
	}
	original := []byte("# user rules\n/private-file")
	if e := os.WriteFile(path, original, 0640); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 2; i++ {
		if e := excludeLocalBeads(common); e != nil {
			t.Fatal(e)
		}
	}
	got, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.HasPrefix(got, append(original, '\n')) {
		t.Fatalf("existing rules changed: %q", got)
	}
	if strings.Count(string(got), "/.beads/\n") != 1 {
		t.Fatalf("duplicated exclusion: %q", got)
	}
	info, e := os.Stat(path)
	if e != nil {
		t.Fatal(e)
	}
	if info.Mode().Perm() != 0640 {
		t.Fatal("changed existing permissions")
	}
}

func TestLocalBeadsExclusionCreatesMissingInfoDirectory(t *testing.T) {
	common := t.TempDir()
	if e := excludeLocalBeads(common); e != nil {
		t.Fatal(e)
	}
	got, e := os.ReadFile(filepath.Join(common, "info", "exclude"))
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(string(got), "\n/.beads/\n") {
		t.Fatalf("missing exclusion: %q", got)
	}
}
func TestOneKingSystemWide(t *testing.T) {
	home := t.TempDir()
	s, _ := Open(home)
	if _, e := s.King(); e == nil || !strings.Contains(e.Error(), "rk spawn king") {
		t.Fatalf("expected spawn hint, got %v", e)
	}
	s.saveRepo(&Repo{ID: "r1", Name: "demo", Command: []string{"true"}, MaxRats: 1})
	s.saveRepo(&Repo{ID: "r2", Name: "other", Command: []string{"true"}, MaxRats: 1})
	now := time.Now()
	// Legacy state may hold several kings; the active one wins over a newer stopped one.
	s.saveRun(&Run{ID: "old", Name: "king-demo", Role: "king", RepoID: "r1", Process: "running", Created: now.Add(-time.Hour)})
	s.saveRun(&Run{ID: "new", Name: "king-other", Role: "king", RepoID: "r2", Process: "exited", Created: now})
	var out bytes.Buffer
	if e := Main([]string{"show", "king", "--home", home}, &out); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(out.String(), `"id": "old"`) {
		t.Fatalf("resolved wrong king: %s", out.String())
	}
	if _, e := s.Spawn("other", "king", "", ""); e == nil || !strings.Contains(e.Error(), "rk attach king") {
		t.Fatalf("second king allowed from another repo: %v", e)
	}
	s.saveRun(&Run{ID: "old", Name: "king-demo", Role: "king", RepoID: "r1", Process: "exited", Created: now.Add(-time.Hour)})
	if _, e := s.Spawn("demo", "king", "", ""); e == nil || !strings.Contains(e.Error(), "rk resume king") {
		t.Fatalf("stopped king not resumed: %v", e)
	}
	if _, e := s.Spawn("demo", "rat", "t1", "king"); e == nil || !strings.Contains(e.Error(), "reserved") {
		t.Fatalf("rat took the king's name: %v", e)
	}
}
func TestDismissedKingMakesRoomForAFreshOne(t *testing.T) {
	// Stop observes tmux; keep it off the user's server. Sockets need a short path.
	sock, e := os.MkdirTemp("/tmp", "rk")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { os.RemoveAll(sock) })
	t.Setenv("TMUX_TMPDIR", sock)
	t.Setenv("TMUX", "")
	home := t.TempDir()
	s, _ := Open(home)
	s.saveRepo(&Repo{ID: "r1", Name: "demo", Command: []string{"true"}, MaxRats: 1})
	s.saveRun(&Run{ID: "k1", Name: "king", Role: "king", RepoID: "r1", State: "working", Process: "exited", Session: "rk-test-absent-k1"})
	s.saveRun(&Run{ID: "rat1", Name: "Pip-rat1", Role: "rat", RepoID: "r1", State: "working", Process: "exited"})
	if _, e := s.Dismiss("rat1"); e == nil {
		t.Fatal("dismissed a rat")
	}
	r, e := s.Dismiss("k1")
	if e != nil {
		t.Fatal(e)
	}
	if r.State != "dismissed" || r.Process != "stopped" {
		t.Fatalf("not dismissed: %+v", r)
	}
	if _, e := s.King(); e == nil || !strings.Contains(e.Error(), "rk spawn king") {
		t.Fatalf("dismissed king still resolves: %v", e)
	}
	if _, e := s.Resume("k1", ""); e == nil || !strings.Contains(e.Error(), "finalized") {
		t.Fatalf("dismissed king resumed: %v", e)
	}
}
