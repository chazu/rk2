package kingdom

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestProviderGuard(t *testing.T) {
	allowed := []string{
		"go test ./...",
		`rk task create --description "no pushes, no git notes"`,
		`rk task create --description "no git notes"`,
		`br comments add x "do not set RK_ROLE=king, unset RK_RUN, or export GIT_CONFIG_COUNT"`,
		`echo 'git push'`,
		`echo "git push --no-verify; git -c core.hooksPath=/x commit"`,
		"cat <<EOF\ngit push origin main\ngit notes add\nEOF",
		"cat <<'EOF' > notes.txt\n$(git push)\nEOF",
		"cat <<-EOF | grep push\n\tgit push\n\tEOF\necho done",
		`grep -rn "git push" .`,
		`grep -n 'RK_REAL_GIT\|core.hooksPath\|refs/notes' internal/kingdom/*.go`,
		`sed -n 's/git notes/x/p' README.md`,
		"# git push\ngo build ./...",
		"echo ok # git push",
		`git commit -m "Explain why no git push happens here"`,
		`git commit -am "fix push button" --author "A <a@b>"`,
		"git commit -m \"$(cat <<'EOF'\nNo git push or git notes; RK_ROLE=king in prose.\nEOF\n)\" && git log -1",
		"git commit -m '-n is just text'",
		"git stash push -u -m tag",
		"if [[ $x =~ ^a(b|c)$ ]]; then echo y; fi",
		"git status --porcelain && git diff HEAD~1",
		"git -C /tmp/repo log -3 --oneline",
		"/usr/bin/git status",
		`git log @{u}..HEAD --grep push`,
		"command -v git",
		"ls | xargs grep push",
		`for f in *.go; do gofmt -l "$f"; done`,
		`case "$x" in push) echo push;; *) echo other;; esac`,
		`echo "$(case $x in (a|b) echo a ;; esac)"`,
		"echo $RK_ROLE $RK_RUN",
		`bash -c 'go vet ./...'`,
		"x=$(git rev-parse HEAD); echo \"$x\"",
		"make check 2>&1 | tail -20",
		`[[ -n "$RK_RUN" && -d .git ]] && echo yes`,
		"find . -name '*.go' -exec gofmt -l {} +",
		`printf '%s\n' RK_ROLE=king`,
		"f() { go test ./...; }; f",
		"arr=(git push); echo ${#arr[@]}",
		"echo $((1 + 2))",
		"while read -r line; do echo \"$line\"; done < file",
	}
	for _, cmd := range allowed {
		if e := checkShellCommand(cmd); e != nil {
			t.Errorf("rejected %q: %v", cmd, e)
		}
	}
	denied := []string{
		"git push",
		"git push origin main",
		"/usr/bin/git push origin x",
		"/usr/bin/Git push",
		"git -C dir push",
		"git -C . push --no-verify",
		"git --git-dir=.git push",
		`bash -c "git push"`,
		"sh -lc 'cd x && git push'",
		`eval "git push"`,
		"env RK_ROLE=king rk task list",
		"export RK_ROLE=king",
		"declare -x RK_RUN=x",
		"RK_ROLE=king rk x",
		"unset RK_RUN",
		"unset GIT_CONFIG_COUNT",
		"env -u RK_ROLE rk x",
		"env -i rk task list",
		"git commit --no-verify -m x",
		"git commit --no-verif -m x",
		"git commit -nm x",
		"git -c core.hooksPath=/x commit",
		"git config core.hooksPath /dev/null",
		"git -c alias.p=push p",
		"GIT_CONFIG_COUNT=0 git commit -m x",
		"$(echo git) push",
		"`echo git` push",
		`"$G" push`,
		"git $SUB",
		"git {push,x}",
		"echo ok && git push",
		"(cd x; git push)",
		"{ git push; }",
		"if true; then git push; fi",
		"! git push",
		"x=$(git push)",
		`echo "$(git push)"`,
		"echo <(git push)",
		"cat <<EOF\n$(git push)\nEOF",
		"git notes add -m x",
		"git fetch origin +refs/notes/commits:refs/notes/commits",
		"git send-pack x",
		"git-receive-pack .",
		"/usr/libexec/git-core/git-send-pack x",
		`"$RK_REAL_GIT" push`,
		"$RK_REAL_GIT status",
		"${RK_REAL_BR} list",
		"sudo -u root git push",
		"nohup git push &",
		"command git push",
		"exec git push",
		"time git push",
		"timeout 5 git push",
		"echo push | xargs git",
		"echo push | xargs -I{} git {}",
		`find . -exec git push \;`,
		"echo git push | bash",
		"bash <<EOF\ngit push\nEOF",
		"bash <<< 'git push'",
		"git rebase -x 'git push' main",
		"for RK_ROLE in king; do rk x; done",
		"echo ${RK_ROLE:=king}",
		"read RK_ROLE <<< king",
		"printf -v RK_ROLE king",
		`$'\x67it' push`,
		`g\it push`,
		`"git" push`,
		"alias g='git push'",
		"trap 'git push' EXIT",
		"echo 'unterminated",
		"echo $(git status",
	}
	for _, cmd := range denied {
		if e := checkShellCommand(cmd); e == nil {
			t.Errorf("allowed %q", cmd)
		}
	}
	b, _ := json.Marshal(map[string]any{"tool_input": map[string]string{"command": "git -C . push"}})
	if e := CheckShell(bytes.NewReader(b)); e == nil {
		t.Error("hook allowed git push")
	}
	if e := CheckShell(strings.NewReader(`{"tool_input":{"command":"rk task create --description \"no git notes\""}}`)); e != nil {
		t.Fatal(e)
	}
	if e := CheckShell(strings.NewReader(`not json`)); e == nil {
		t.Error("hook allowed malformed input")
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
func TestStaleRunBinIsNotTheRealExecutable(t *testing.T) {
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	// An older rk linked br and git to itself in ROOT/runs/ID/bin.
	stale := filepath.Join(t.TempDir(), "runs", "old", "bin")
	real := t.TempDir()
	os.MkdirAll(stale, 0700)
	for _, name := range []string{"br", "git"} {
		if e = os.Symlink(exe, filepath.Join(stale, name)); e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(filepath.Join(real, name), []byte("#!/bin/sh\n"), 0700); e != nil {
			t.Fatal(e)
		}
	}
	t.Setenv("PATH", stale+string(os.PathListSeparator)+real)
	if p := beadsExecutable(); p != filepath.Join(real, "br") {
		t.Fatalf("resolved br %q", p)
	}
	if p := gitExecutable(); p != filepath.Join(real, "git") {
		t.Fatalf("resolved git %q", p)
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
	s, _ := Open(hermetic(t))
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
	s, _ := Open(hermetic(t))
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
	t.Setenv("PATH", filepath.Dir(fake)+string(os.PathListSeparator)+os.Getenv("PATH"))
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
	home := hermetic(t)
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
	if _, e := s.ConfigKing([]string{"true"}); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Spawn("other", "king", "", ""); e == nil || !strings.Contains(e.Error(), "without --repo") {
		t.Fatalf("king bound to a repository: %v", e)
	}
	if _, e := s.Spawn("", "king", "", ""); e == nil || !strings.Contains(e.Error(), "rk attach king") {
		t.Fatalf("second king allowed: %v", e)
	}
	s.saveRun(&Run{ID: "old", Name: "king-demo", Role: "king", RepoID: "r1", Process: "exited", Created: now.Add(-time.Hour)})
	if _, e := s.Spawn("", "king", "", ""); e == nil || !strings.Contains(e.Error(), "rk resume king") {
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
func TestGeneratedRatNamesRetryOnCollision(t *testing.T) {
	ids := []string{"abcd00000001", "abcd00000002", "ef0100000003"}
	next := func() string { id := ids[0]; ids = ids[1:]; return id }
	taken := ratName("abcd99999999")
	id, name, e := pickRatName(func(n string) bool { return n == taken }, next)
	if e != nil || id != "ef0100000003" || name != ratName("ef0100000003") {
		t.Fatalf("got %q %q %v", id, name, e)
	}
	if _, _, e = pickRatName(func(string) bool { return true }, newID); e == nil || !strings.Contains(e.Error(), "--name") {
		t.Fatalf("endless collisions not refused: %v", e)
	}
}
func TestKingConfigIsGlobal(t *testing.T) {
	s, _ := Open(t.TempDir())
	k, e := s.KingConfig()
	if e != nil || strings.Join(k.Command, " ") != "claude {prompt}" {
		t.Fatalf("default king command: %+v %v", k, e)
	}
	if _, e = s.ConfigKing([]string{}); e == nil {
		t.Fatal("empty king command accepted")
	}
	var out bytes.Buffer
	if e = Main([]string{"config", "--king", "--repo", "demo", "--home", s.Root}, &out); e == nil || !strings.Contains(e.Error(), "only --command") {
		t.Fatalf("king config took a repository: %v", e)
	}
	if e = Main([]string{"config", "--king", "--command", `["agent","{prompt_file}"]`, "--home", s.Root}, &out); e != nil {
		t.Fatal(e)
	}
	r := &Run{ID: "k1", Role: "king"}
	repo, command, e := s.agentCommand(r)
	if e != nil || repo != nil || strings.Join(command, " ") != "agent {prompt_file}" {
		t.Fatalf("king launch command: %v %v %v", repo, command, e)
	}
	if _, e = s.runRepo(r); e == nil {
		t.Fatal("king resolved a repository")
	}
}

// The king commits in many repositories, so its hooks run each repository's
// own hook rather than those of the one it was launched from.
func TestKingHooksDispatchToCurrentRepository(t *testing.T) {
	s, _ := Open(t.TempDir())
	king := &Run{ID: "k1", Role: "king", Worktree: s.kingDir()}
	os.MkdirAll(filepath.Join(s.runDir("k1"), "hooks", "stale"), 0700)
	os.WriteFile(filepath.Join(s.runDir("k1"), "hooks", "update"), []byte("#!/bin/sh\nexit 1\n"), 0700)
	dest, e := hooks(s, king, nil, "")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(filepath.Join(dest, "update")); !os.IsNotExist(e) {
		t.Fatal("stale hook from an earlier launch kept")
	}
	home := t.TempDir()
	base := []string{"HOME=" + home, "XDG_CONFIG_HOME=" + home, "GIT_CONFIG_NOSYSTEM=1", "PATH=" + os.Getenv("PATH")}
	session := append(base, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.hooksPath", "GIT_CONFIG_VALUE_0="+dest)
	git := func(dir string, env []string, args ...string) error {
		c := exec.Command("git", args...)
		c.Dir, c.Env = dir, env
		if out, e := c.CombinedOutput(); e != nil {
			return fmt.Errorf("git %v: %v: %s", args, e, out)
		}
		return nil
	}
	repo := func(hook string) string {
		dir := t.TempDir()
		for _, args := range [][]string{{"init", "-q"}, {"config", "user.name", "t"}, {"config", "user.email", "t@t"}} {
			if e := git(dir, base, args...); e != nil {
				t.Fatal(e)
			}
		}
		if hook != "" {
			os.WriteFile(filepath.Join(dir, ".git", "hooks", "pre-commit"), []byte("#!/bin/sh\n"+hook+"\n"), 0700)
		}
		return dir
	}
	marks := repo(`touch "$(git rev-parse --show-toplevel)/.hook-ran"`)
	refuses := repo("exit 1")
	plain := repo("")
	if e = git(marks, session, "commit", "-q", "--allow-empty", "-m", "a"); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(filepath.Join(marks, ".hook-ran")); e != nil {
		t.Fatal("repository's own pre-commit did not run")
	}
	if e = git(refuses, session, "commit", "-q", "--allow-empty", "-m", "b"); e == nil {
		t.Fatal("refusing pre-commit of the current repository was skipped")
	}
	if e = git(plain, session, "commit", "-q", "--allow-empty", "-m", "c"); e != nil {
		t.Fatalf("repository without hooks: %v", e)
	}
	if _, e = os.Stat(filepath.Join(plain, ".hook-ran")); e == nil {
		t.Fatal("another repository's hook ran")
	}
	if _, e = os.Stat(filepath.Join(dest, "pre-push")); e != nil {
		t.Fatal("king lost the pre-push refusal")
	}
}
func TestKingPromptNamesNoRepository(t *testing.T) {
	s, _ := Open(t.TempDir())
	repo := &Repo{ID: "r1", Name: "launchrepo", BeadsDir: "/x/launchrepo/.beads"}
	king := &Run{ID: "k1", Name: "king", Role: "king", RepoID: "r1"}
	text, e := s.Prompt(repo, king)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(text, "launchrepo") || strings.Contains(text, "Repository:") {
		t.Fatalf("king prompt names a repository:\n%s", text)
	}
	for _, want := range []string{"rk repos", "--repo NAME", "Run ID: k1", "Do not push"} {
		if !strings.Contains(text, want) {
			t.Fatalf("king prompt lacks %q:\n%s", want, text)
		}
	}
	for _, kv := range s.runEnv(repo, king) {
		if strings.HasPrefix(kv, "BEADS_DIR=") || strings.HasPrefix(kv, "RK_REPO=") {
			t.Fatalf("king environment binds a repository: %s", kv)
		}
	}
	// A user copy replaces the role instructions but not the shared rules.
	os.MkdirAll(filepath.Join(s.Root, "instructions"), 0700)
	os.WriteFile(filepath.Join(s.Root, "instructions", "king.txt"), []byte("CUSTOM KING\n"), 0600)
	text, e = s.Prompt(repo, king)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(text, "CUSTOM KING") || strings.Contains(text, "RESPONSIBILITIES") || !strings.Contains(text, "Do not push") {
		t.Fatalf("override not applied as expected:\n%s", text)
	}
	rat := &Run{ID: "rat1", Name: "Pip", Role: "rat", RepoID: "r1"}
	env := strings.Join(s.runEnv(repo, rat), "\n")
	if !strings.Contains(env, "BEADS_DIR=/x/launchrepo/.beads") || !strings.Contains(env, "RK_REPO=r1") {
		t.Fatalf("rat lost its repository environment:\n%s", env)
	}
}
func TestTaskCloseArgumentsAndRole(t *testing.T) {
	id, reason, e := closeArgs([]string{"bd-1", "--reason", "duplicate", "--json"})
	if e != nil || id != "bd-1" || reason != "duplicate" {
		t.Fatalf("got %q %q %v", id, reason, e)
	}
	if id, reason, e = closeArgs([]string{"--reason=won't do", "bd-2"}); e != nil || id != "bd-2" || reason != "won't do" {
		t.Fatalf("got %q %q %v", id, reason, e)
	}
	for _, args := range [][]string{{"bd-1"}, {"--reason", "x"}, {"bd-1", "--reason"}, {"bd-1", "bd-2", "--reason", "x"}, {"bd-1", "--reason", "x", "--force"}} {
		if _, _, e := closeArgs(args); e == nil || !strings.Contains(e.Error(), "usage") {
			t.Errorf("accepted %v: %v", args, e)
		}
	}
	home := t.TempDir()
	t.Setenv("RK_ROLE", "rat")
	t.Setenv("RK_RUN", "somerat")
	e = Main([]string{"task", "--home", home, "close", "bd-1", "--reason", "abandoned"}, io.Discard)
	if e == nil || !strings.Contains(e.Error(), "rats cannot close") {
		t.Fatal(e)
	}
	t.Setenv("RK_ROLE", "")
	t.Setenv("RK_RUN", "")
	if e = Main([]string{"task", "--home", home, "close", "bd-1"}, io.Discard); e == nil || !strings.Contains(e.Error(), "usage") {
		t.Fatal(e)
	}
}

func TestIntegrationReportSeparatesOwnCheckFromRatReport(t *testing.T) {
	repo := &Repo{Name: "demo"}
	ratClaim := "NOT RUN: make check acceptance stage"
	r := &Run{Target: "rk/integration", IntegratedCommit: "abc123", Checks: ratClaim,
		IntegrationCheck: "passed: " + checkLabel([]string{"make", "check"})}
	got := integrationReport(repo, r)
	for _, want := range []string{"Integration check: [\"make\",\"check\"] passed.", "Rat-reported checks (not verified by integration): " + ratClaim} {
		if !strings.Contains(got, want) {
			t.Errorf("configured report %q lacks %q", got, want)
		}
	}
	r.IntegrationCheck = noIntegrationCheck
	got = integrationReport(repo, r)
	for _, want := range []string{"Integration check: none configured for this repository; no repository check ran.", "rk config --repo demo --check JSON", "Rat-reported checks (not verified by integration): " + ratClaim} {
		if !strings.Contains(got, want) {
			t.Errorf("unconfigured report %q lacks %q", got, want)
		}
	}
	if strings.Contains(got, "passed") || strings.Index(got, ratClaim) < strings.Index(got, "Rat-reported checks") {
		t.Errorf("rat's checks presented as the integration result: %q", got)
	}
	r.Checks = ""
	if got = integrationReport(repo, r); strings.Contains(got, "Rat-reported") {
		t.Errorf("empty rat report still labelled: %q", got)
	}
}
func TestIssuePrefixRules(t *testing.T) {
	for name, want := range map[string]string{"rk2": "rk2", "My_Project.v2": "my-project-v2", "--Odd  Name--": "odd-name", "___": ""} {
		if got := defaultPrefix(name); got != want {
			t.Errorf("defaultPrefix(%q) = %q, want %q", name, got, want)
		}
	}
	for p, want := range map[string]bool{"rk2": true, "my-proj": true, "": false, "-a": false, "a-": false, "a--b": false, "Bad": false, "a_b": false} {
		if validPrefix(p) != want {
			t.Errorf("validPrefix(%q) != %v", p, want)
		}
	}
	dir := t.TempDir()
	repo := &Repo{BeadsDir: dir}
	for text, want := range map[string]string{
		"# Beads Project Configuration\n# issue_prefix: repo\n": "",
		"claim_exclusive: true\nissue_prefix: mine\n":           "mine",
		"issue_prefix: \"quoted\" # note\n":                     "quoted",
		"nested:\n  issue_prefix: deep\n":                       "",
	} {
		if e := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(text), 0o644); e != nil {
			t.Fatal(e)
		}
		if got, e := workspacePrefix(repo); e != nil || got != want {
			t.Errorf("workspacePrefix(%q) = %q, %v; want %q", text, got, e, want)
		}
	}
}

// Real br against disposable workspaces whose user config is a decoy home
// inside the test directory, never the real one.
func TestWorkspacesGetOwnIssuePrefix(t *testing.T) {
	if _, e := exec.LookPath(beadsExecutable()); e != nil {
		t.Skip("br not installed")
	}
	root := t.TempDir()
	home := filepath.Join(root, "home")
	decoy := filepath.Join(home, ".beads", "config.yaml")
	if e := os.MkdirAll(filepath.Dir(decoy), 0o755); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(decoy, []byte("issue_prefix: decoy\n"), 0o644); e != nil {
		t.Fatal(e)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	workspace := func(name string) *Repo {
		dir := filepath.Join(home, name)
		if e := os.MkdirAll(dir, 0o755); e != nil {
			t.Fatal(e)
		}
		return &Repo{ID: name, Name: name, Path: dir, BeadsDir: filepath.Join(dir, ".beads"), Target: "rk/integration", Command: []string{"true"}, MaxRats: 1}
	}
	create := func(r *Repo, title string) string {
		b, e := beads(r, "king", "create", title)
		if e != nil {
			t.Fatal(e)
		}
		var v struct {
			ID string `json:"id"`
		}
		if e = json.Unmarshal(b, &v); e != nil {
			t.Fatal(e)
		}
		return v.ID
	}
	expect := func(r *Repo, prefix string, ids ...string) {
		t.Helper()
		if v := viewRepo(r); v.IssuePrefix != prefix || v.IssuePrefixSource != "workspace" {
			t.Fatalf("%s: prefix %q from %q, want %q from workspace", r.Name, v.IssuePrefix, v.IssuePrefixSource, prefix)
		}
		if id := create(r, "new"); !strings.HasPrefix(id, prefix+"-") {
			t.Fatalf("%s: new task %s lacks prefix %s", r.Name, id, prefix)
		}
		for _, id := range ids {
			if task, e := getTask(r, id); e != nil || task.ID != id {
				t.Fatalf("%s: existing task %s not kept: %v", r.Name, id, e)
			}
		}
	}

	fresh := workspace("fresh")
	if e := initWorkspace(fresh, "", "fresh"); e != nil {
		t.Fatal(e)
	}
	expect(fresh, "fresh")
	override := workspace("override")
	if e := initWorkspace(override, "ovr", "ovr"); e != nil {
		t.Fatal(e)
	}
	expect(override, "ovr")

	adopted := workspace("adopted")
	if _, e := beads(adopted, "king", "init"); e != nil {
		t.Fatal(e)
	}
	leaked := create(adopted, "before rk")
	if !strings.HasPrefix(leaked, "decoy-") {
		t.Fatalf("fixture did not reproduce the leak: %s", leaked)
	}
	if e := initWorkspace(adopted, "", "adopted"); e != nil {
		t.Fatal(e)
	}
	expect(adopted, "adopted", leaked)
	keeper := workspace("keeper")
	if _, e := beads(keeper, "king", "init"); e != nil {
		t.Fatal(e)
	}
	if _, e := beads(keeper, "king", "config", "set", "issue_prefix", "keep"); e != nil {
		t.Fatal(e)
	}
	if e := initWorkspace(keeper, "", "keeper"); e != nil {
		t.Fatal(e)
	}
	expect(keeper, "keep")

	// A workspace registered before rk managed prefixes, fixed with rk config.
	s, _ := Open(filepath.Join(root, "state"))
	if e := s.saveRepo(adopted); e != nil {
		t.Fatal(e)
	}
	config := filepath.Join(adopted.BeadsDir, "config.yaml")
	b, _ := os.ReadFile(config)
	kept := []string{}
	for _, l := range strings.SplitAfter(string(b), "\n") {
		if !strings.HasPrefix(l, "issue_prefix:") {
			kept = append(kept, l)
		}
	}
	if e := os.WriteFile(config, []byte(strings.Join(kept, "")), 0o644); e != nil {
		t.Fatal(e)
	}
	if v := viewRepo(adopted); v.IssuePrefix != "decoy" || !strings.HasPrefix(v.IssuePrefixSource, "inherited") {
		t.Fatalf("inherited prefix not reported: %+v", v)
	}
	older := create(adopted, "still inherited")
	if _, e := s.Config("adopted", nil, nil, 0, "Bad_X"); e == nil {
		t.Fatal("accepted invalid prefix")
	}
	if v, e := s.Config("adopted", nil, nil, 0, "adopted2"); e != nil || v.IssuePrefix != "adopted2" {
		t.Fatalf("config prefix: %+v %v", v, e)
	}
	expect(adopted, "adopted2", leaked, older)
	if b, _ := os.ReadFile(decoy); string(b) != "issue_prefix: decoy\n" {
		t.Fatalf("decoy home config changed: %q", b)
	}
}
