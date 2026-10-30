# Rat Kingdom

A small local swarm orchestrator: a king, named rats, tmux sessions, isolated Git
worktrees, and a shared Rust Beads task tracker. Rats do one task per lifetime.
The king reviews and integrates their work locally.

**Nothing pushes automatically.** Only `rk publish`, invoked by the king after
an explicit user request, publishes one named branch. There is no steward,
synchronization subsystem, or Git-notes functionality.

## Build and check

Requirements: Go 1.24+, Git, tmux, Rust Beads (`br`), and an agent CLI. The default
agent command is Claude Code. The agent runs directly on its tmux pane's terminal,
as the foreground process group, so it follows pane resizes and receives keyboard
signals; `tmux pipe-pane` records each attempt's log. macOS is acceptance-tested;
Linux support is implemented but has not been exercised on a Linux host.

```sh
make build
./bin/rk help
make check
```

`make check` runs vet, race-enabled Go tests, a build, and acceptance tests using
real Git, Beads, and an isolated tmux server. Acceptance also requires Python 3.
It uses disposable repositories and scripted agents, with no model calls or
remote pushes. `make test` runs just the Go tests.

The executable is `bin/rk`. Put that binary on your PATH if desired; no global
installation is performed by the build.

## Start a kingdom

From a repository with an initial commit:

```sh
rk init --check '["go","test","./..."]'
rk task create "Implement the login validation" --description "Acceptance criteria..."
rk spawn king
rk attach king                       # or: rk at king
```

The king is your point of contact: attach and type to it directly. Detach with
the tmux prefix and `d` (`Ctrl-b d` by default); the king keeps running.
`rk restart king` stops and relaunches it. `rk dismiss king` stops it and
retires the run, keeping its record and logs, so `rk spawn king` starts a
fresh one.

`rk init` registers the repository, excludes the entire `/.beads/` directory in
the common Git directory's `info/exclude`, initializes `.beads` if absent,
enables exclusive claims, and creates an unchecked-out `rk/integration` branch
from HEAD. The exclusion is local to the clone and applies to its linked
worktrees; it does not edit `.gitignore` or create a tracked change. Existing
tracked Beads files remain tracked—initialization never removes them from the
index. It preserves the user's checkout and Git hooks. Use `--name NAME` to pick
a repository name, `--target BRANCH` for another integration target, and
`--max-rats N` to change the default four concurrent rats.

The check is a JSON argument array, executed without shell interpolation. Set
it to the repository's actual integration gate. If no check is configured,
integration relies on the king's review and the worker's recorded evidence;
it does not claim to have run an integration test.

Agent commands are also argument arrays:

```sh
rk config --command '["claude","{prompt}"]'
rk config --check '["make","ci"]' --max-rats 3
```

`{prompt}` expands to the full role/task instructions; `{prompt_file}` expands
to a file containing them. The default is `claude {prompt}`. Claude receives a
session-specific tool hook as an additional guard. Rat Kingdom does not enable
permission bypass flags. Configure the provider's permission mode explicitly
if unattended operation requires it. Alternative agent commands can use the
same lifecycle and environment; only the Claude-specific hook is automatic.

## Work and integration

```sh
rk spawn rat --task TASK_ID
rk spawn rat                         # selects an unassigned ready non-epic task
rk list
rk show RUN
rk logs RUN
rk attach RUN
rk tell RUN "Please also check the empty-input case"
rk wait --timeout 60s                 # bounded wait for actionable results
```

Each run gets a unique ID, a recognizable name, a task claim, a branch, and a
worktree. A directed claim cannot overwrite another owner. Undirected spawning
uses Beads ready ordering; it does not ask another model to schedule tasks.

Inside the worker session:

```sh
rk inbox
rk inbox --ack COMMENT_ID
# Implement, test, and commit intended files locally.
rk finish --summary "Implemented validation" --checks "go test ./... passed"
# Or, if blocked:
rk block --summary "Need a decision about empty usernames"
```

`finish` requires the assigned branch, a clean worktree, and recorded validation.
It marks the result ready for review. It **does not close the task**. `block`
leaves ownership and work intact. The worker must not take another task.

After reviewing the result, the king runs:

```sh
rk stop RUN
rk integrate RUN
rk clean RUN
```

Integration merges the submitted commit in a detached candidate worktree, runs
the configured check, and advances the target with a compare-and-swap update.
A target checked out in another worktree is refused. A successful integration
closes the Beads ticket, allowing its dependents to become ready. The user's
checkout, index, stash, and hooks are not changed.

Failed merges and checks preserve the worker's work and the candidate directory
reported in the error. Inspect that candidate before manually removing it;
rat-kingdom does not automatically discard conflicted candidates.

`clean` requires a stopped run, successful integration, a closed ticket, and no
dirty or unintegrated work. `clean --discard` deliberately deletes that worker's
local work and releases its claim; use it only when the user asked to discard
that work. No cleanup operation deletes remote branches.

## Recovery and messages

```sh
rk reconcile
rk stop RUN
rk resume RUN --message "Continue from the existing files; focus on the failing test"
rk restart RUN --message "..."        # stop, then resume
rk watch
```

Resume reuses the worktree and claim, including untracked files. Each attempt
has its own terminal log. Stopping never commits, merges, pushes, or deletes
files. Exit codes describe the process; they do not establish task completion.
A missing exit receipt is reported as unknown rather than success.

Run state is written atomically under `~/.rat-kingdom` (or `RK_HOME`/`--home`).
A file lock serializes lifecycle mutations. The launcher and its descendants
hold a separate liveness lock. If that lock remains held and stopping cannot be
confirmed, the command fails while preserving the work.

Results and coordinator messages are recorded durably before delivery to Beads.
`reconcile` retries pending comments using stable message markers to avoid
reposting after an interrupted delivery. It also retries ticket closure after
successful integration. Rats read task comments with `inbox` and explicitly
acknowledge the highest processed comment ID. Reading does not acknowledge.

`wait` returns actionable runs to the king, or unread task comments when given
a run ID. It returns after at most 60 seconds, making it suitable for an agent's
tool loop. `watch` displays changes continuously for a human.

Agents also get notices typed into their own session, so one idle at its
prompt still learns of new work. Each launcher checks every 5 seconds
(`RK_NUDGE_INTERVAL`). It sends `Ctrl-A` `Ctrl-K` to cut any unsent draft on
the input line (restore it with `Ctrl-Y`), types one `[rk]` line, then Enter:

- A rat is told when task comments from others arrive past its acknowledged
  cursor, such as an `rk tell` from the king.
- The king is told when a rat becomes ready, blocked, startup-failed, or its
  process exits; it then reviews with `rk wait`.

Notices are text for the agent to read, never commands rk runs. A notice is
held while someone attached to that session has typed within the interval, so
it does not land in the middle of your input, and each change is announced
once per launch. A busy agent sees the notice as its next message.

All Beads operations target one canonical local workspace per repository.
Automatic JSONL import and export are disabled for managed operations. There
are no tracker-copying jobs, sync branches, or cross-machine coordination.
Do not use stale per-worktree task databases.

## Multiple repositories

```sh
rk init /path/to/other-project --name other --check '["make","test"]'
rk repos
rk spawn rat --repo other --task TASK_ID
rk task --repo other ready
rk list --repo other
```

Commands resolve the registered canonical repository rather than assuming the
current working directory is the target. Run IDs also identify their repository.
There is one king, system-wide, and it manages every registered repository
using `--repo`. `rk spawn king` refuses while a king exists; resume a stopped
king with `rk resume king`, or `rk dismiss king` to replace it. `king` works as a run name in any command, and rats
cannot use it.

The king's prompt names no repository. It tells the king to discover
repositories with `rk repos` and to pass `--repo NAME`. Its working directory
is the repository it was launched from, but its environment carries no
`BEADS_DIR` or `RK_REPO`, so it reaches tasks through `rk task --repo NAME`.
Rats keep both, since each one serves a single repository.

The king's role instructions are embedded in `rk`. To customize them, put a
replacement at `$RK_HOME/instructions/king.txt` (default
`~/.rat-kingdom/instructions/king.txt`). The identity line and shared safety
rules are always prepended and cannot be overridden. A prompt is written when a
run launches, so `rk restart king` picks up a change. `rk prime king` prints the
result.

## Publication and the guard boundary

Only after the user specifically asks the king to push:

```sh
rk publish --repo other --remote origin --branch rk/integration \
  --request "User asked to push rk/integration to origin"
```

This resolves the exact local commit and sends it to that branch on one named
remote. It rejects remotes with multiple push URLs and disables implicit tag,
mirror, and submodule publication. It does not force-push or delete refs. A local
receipt records the request, commit, destination, and outcome. The request is
required anew for each invocation; it is an audit record, not machine proof of
human consent. The king must faithfully follow the user's actual instruction.

Managed sessions receive:

- A Git command wrapper that rejects pushes, notes, aliases, configuration
  overrides, and selected execution escapes.
- A session-only pre-push hook, without rewriting repository configuration.
  Other existing hooks remain available.
- For Claude, a shell-tool hook that rejects common direct Git bypasses.
- Role checks preventing rats from invoking publication and coordinator commands.

These are executable operational guards, **not a hostile-code sandbox**. An
agent with unrestricted shell access under your account can deliberately evade
PATH/environment guards, use a native Git binary with verification disabled,
or use a different network client. Alternative providers do not automatically
receive the Claude tool hook. Do not treat the guards as a security boundary
against malicious code; that would require OS/container isolation and a
separate credential/network policy. Normal managed command paths reject pushes
regardless of whether the agent remembers its prompt.

## Validation scope

The acceptance suite exercises real claims, launch failure after claiming,
concurrent claim attempts, process stop and launcher-crash recovery, nonzero
exit receipts, untracked-file preservation, blocked outcomes, comment retry and
deduplication, integration conflicts, failed checks, ticket-closure retry,
checked-out-target protection, and normal command guards.

The publication command's authorized argument construction is tested against a
fake Git executable, so validation performs no actual pushes. Live model
behavior and a real authorized remote publication remain untested.

See [DESIGN.md](DESIGN.md) for the original assessment and scope.
