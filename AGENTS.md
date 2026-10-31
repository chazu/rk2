# Rat Kingdom

Read DESIGN.md and README.md before changing lifecycle behavior.

- No pushes unless the user explicitly asks the king to push a specific result.
  Building, testing, committing, completing work, and cleanup do not authorize a push.
- No synchronization subsystem, Git notes, or steward role.
- Preserve worktrees, branches, untracked files, and handoffs on failure.
- Keep blocked, ready, integrated, and process-exit states distinct.
- Shared Beads workspace per repository; use the CLI adapter, not database internals.
- Preserve the user's checkout, index, stash, hooks, and unrelated work.
- Run `make check` before claiming implementation is ready. It needs git, tmux,
  br, and python3, and runs only disposable local acceptance fixtures.
- Do not test against live agent sessions or publish real refs for acceptance tests.
- Git is not wrapped in managed sessions. Pushes are refused by the session-only
  pre-push hook and the Claude Bash hook; `br` and role checks remain. Keep these
  guards' limitations explicit. They are not a sandbox.
