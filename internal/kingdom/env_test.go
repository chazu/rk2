package kingdom

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// sessionVars identify a managed rat or king session. Rats and the king run
// `make check` from those sessions, so tests must not inherit them: a rat role
// alone turns king-side code paths into refusals, and RK_HOME names the user's
// real kingdom. The guard environment (the br PATH shim, RK_REAL_BR, GIT_* and
// BEADS_* overrides) is deliberately left alone; tests that need a particular
// Git or br set it themselves with t.Setenv.
var sessionVars = []string{"RK_ROLE", "RK_RUN", "RK_REPO", "RK_HOME", "RK_NUDGE_INTERVAL", "BEADS_DIR", "TMUX", "TMUX_PANE"}

// TestMain clears the session identity before any test runs and points
// RK_HOME at a disposable directory, so a test that forgets --home cannot
// touch ~/.rat-kingdom.
func TestMain(m *testing.M) {
	home, e := os.MkdirTemp("", "rk-test-home")
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	if e = clearSession(home); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

func clearSession(home string) error {
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
