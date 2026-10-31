package kingdom

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sessionVars are exported into managed rat and king sessions. Rats and the
// king run `make check` from those sessions, so tests must never inherit them:
// RK_ROLE=rat alone turns king-side code paths into refusals, and RK_HOME
// names the user's real kingdom.
var sessionVars = []string{"RK_ROLE", "RK_RUN", "RK_REPO", "RK_HOME", "RK_REAL_GIT", "RK_REAL_BR", "RK_NUDGE_INTERVAL", "BEADS_DIR", "TMUX", "TMUX_PANE"}

// TestMain strips the managed-session environment before any test runs and
// points RK_HOME at a disposable directory, so a test that forgets --home
// cannot touch ~/.rat-kingdom. Git and Beads overrides (GIT_CONFIG_*,
// core.hooksPath, BEADS_*) and the run's guard directory on PATH go too.
func TestMain(m *testing.M) {
	home, e := os.MkdirTemp("", "rk-test-home")
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	if e = sanitizeEnv(home); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

func sanitizeEnv(home string) error {
	if prev := os.Getenv("RK_HOME"); prev != "" {
		runs := filepath.Join(prev, "runs") + string(filepath.Separator)
		var keep []string
		for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
			if !strings.HasPrefix(dir, runs) {
				keep = append(keep, dir)
			}
		}
		if e := os.Setenv("PATH", strings.Join(keep, string(os.PathListSeparator))); e != nil {
			return e
		}
	}
	for _, v := range os.Environ() {
		k, _, _ := strings.Cut(v, "=")
		if strings.HasPrefix(k, "GIT_") || strings.HasPrefix(k, "BEADS_") || strings.HasPrefix(k, "BD_") || strings.HasPrefix(k, "BR_") {
			if e := os.Unsetenv(k); e != nil {
				return e
			}
		}
	}
	for _, k := range sessionVars {
		if e := os.Unsetenv(k); e != nil {
			return e
		}
	}
	return os.Setenv("RK_HOME", home)
}

// hermetic clears the session variables for one test and returns a fresh
// home, which it also exports as RK_HOME. Tests that exercise role-sensitive
// paths call it so they hold even when run outside TestMain's sanitising, such
// as from TestHermeticInsideRatSession.
func hermetic(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	for _, k := range sessionVars {
		t.Setenv(k, "")
	}
	t.Setenv("RK_HOME", home)
	return home
}

// TestHermeticInsideRatSession replays the tests that once failed inside a rat
// session under that session's environment, so a regression shows up even
// when the suite itself runs from a plain shell.
func TestHermeticInsideRatSession(t *testing.T) {
	sessionHome := filepath.Join(t.TempDir(), "session-kingdom")
	t.Setenv("RK_ROLE", "rat")
	t.Setenv("RK_RUN", "fake0run0id")
	t.Setenv("RK_REPO", "fake0repo")
	t.Setenv("RK_HOME", sessionHome)
	t.Setenv("BEADS_DIR", filepath.Join(sessionHome, "beads"))
	t.Setenv("TMUX", "/nonexistent/rk-test,1,0")
	for _, c := range []struct {
		name string
		f    func(*testing.T)
	}{
		{"PublishRequiresRequestBeforeGit", TestPublishRequiresRequestBeforeGit},
		{"PublicationPinsOneRefAndDoesNotActuallyPush", TestPublicationPinsOneRefAndDoesNotActuallyPush},
		{"OneKingSystemWide", TestOneKingSystemWide},
	} {
		t.Run(c.name, c.f)
	}
	if _, e := os.Stat(sessionHome); !os.IsNotExist(e) {
		t.Fatalf("tests touched the session's RK_HOME: %v", e)
	}
}
