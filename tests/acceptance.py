#!/usr/bin/env python3
"""Real git, br and tmux acceptance in disposable local directories. Never pushes."""
import json
import os
from pathlib import Path
import shutil
import signal
import concurrent.futures
import subprocess
import tempfile
import time

BINARY = str((Path(__file__).resolve().parent.parent / 'bin/rk'))
for tool in ('git', 'br', 'tmux'):
    if not shutil.which(tool):
        raise SystemExit(f'required acceptance dependency missing: {tool}')
root = Path(tempfile.mkdtemp(prefix='rk-accept-', dir='/tmp'))
env = {k: v for k, v in os.environ.items()
       if not k.startswith(('RK_', 'GIT_', 'BEADS_', 'BD_', 'BR_')) and k != 'TMUX'}
env.update(RK_HOME=str(root / 'state'), TMUX_TMPDIR=str(root), RUST_LOG='error',
           RK_NUDGE_INTERVAL='300ms')

def command(args, cwd=None, ok=True, extra=None):
    p = subprocess.run(args, cwd=cwd, env={**env, **(extra or {})},
                       capture_output=True, text=True, timeout=40)
    if ok and p.returncode:
        raise AssertionError(f'{args}: {p.returncode}\n{p.stdout}\n{p.stderr}')
    return p

def rk(*args, ok=True, extra=None):
    return command([BINARY, *args], ok=ok, extra=extra)

def data(*args):
    return json.loads(rk(*args).stdout)

def git(repo, *args):
    return command(['git', '-C', str(repo), *args]).stdout.strip()

def task(repo, title):
    return data('task', '--repo', repo, 'create', title)['id']

def runstate(run):
    return data('show', run['id'])

def wait_exit(run):
    until = time.monotonic() + 15
    while time.monotonic() < until:
        r = runstate(run)
        if r['process'] == 'exited':
            return r
        time.sleep(.1)
    raise AssertionError(f'launcher did not exit: {runstate(run)}')

def stopped(run):
    rk('stop', run['id'])

def fixture(name, check='["/bin/sh","-c","test -f base.txt"]'):
    repo = root / name
    repo.mkdir()
    git(repo, 'init', '-b', 'main')
    git(repo, 'config', 'user.name', 'Acceptance')
    git(repo, 'config', 'user.email', 'acceptance@example.invalid')
    git(repo, 'config', 'commit.gpgsign', 'false')
    (repo / '.gitignore').write_text('*.local\n')
    (repo / 'base.txt').write_text('base\n')
    git(repo, 'add', '.gitignore', 'base.txt')
    git(repo, 'commit', '-m', 'initial')
    exclude = repo / '.git/info/exclude'
    previous_exclude = exclude.read_text() + '\n# User exclusions\n/user-local/'
    exclude.write_text(previous_exclude)
    rk('init', str(repo), '--name', name, '--command', '["/bin/sh","-c","exit 0"]',
       *(('--check', check) if check else ()))
    assert (repo / '.gitignore').read_text() == '*.local\n'
    assert exclude.read_text().startswith(previous_exclude + '\n')
    assert exclude.read_text().splitlines().count('/.beads/') == 1
    assert git(repo, 'status', '--porcelain', '--untracked-files=all') == ''
    return repo

try:
    repo = fixture('demo')
    main_before = git(repo, 'rev-parse', 'main')
    issue = task('demo', 'implement result')
    assert git(repo, 'status', '--porcelain', '--untracked-files=all') == ''
    (repo / 'user-dirty.txt').write_text('must survive\n')
    run = data('spawn', 'rat', '--repo', 'demo', '--task', issue)
    wait_exit(run)
    wt = Path(run['worktree'])
    (wt / '.beads').mkdir()
    (wt / '.beads/ignore-probe').write_text('local data')
    assert git(wt, 'check-ignore', '.beads/ignore-probe') == '.beads/ignore-probe'
    (wt / '.beads/ignore-probe').unlink()
    (wt / '.beads').rmdir()
    print('PASS Beads initialization and task writes stay locally ignored across worktrees')
    (wt / 'new.txt').write_text('new untracked content\n')
    assert rk('clean', run['id'], ok=False).returncode
    assert rk('finish', run['id'], '--summary', 'done', '--checks', 'checked', ok=False).returncode
    assert (wt / 'new.txt').exists()
    stopped(run)
    rk('resume', run['id'], '--message', 'preserve new.txt')
    wait_exit(run)
    assert (wt / 'new.txt').read_text() == 'new untracked content\n'
    prompt = (root / 'state/runs' / run['id'] / 'prompt.txt').read_text()
    assert 'preserve new.txt' in prompt
    git(wt, 'add', 'new.txt')
    git(wt, 'commit', '-m', 'implement task')
    rk('finish', run['id'], '--summary', 'implemented', '--checks', 'file verified')
    assert data('task', '--repo', 'demo', 'show', issue)[0]['status'] == 'in_progress'
    rk('tell', run['id'], 'review feedback')
    inbox = data('inbox', run['id'])
    assert any('review feedback' in c['text'] and c['author'] == 'king' for c in inbox)
    rk('inbox', run['id'], '--ack', str(max(c['id'] for c in inbox)))
    assert data('inbox', run['id']) == []
    assert data('wait', run['id'], '--timeout', '1s')['status'] == 'timeout'
    stopped(run)
    result = data('integrate', run['id'])
    assert result['state'] == 'integrated' and result['closed']
    assert result['integration_check'] == 'passed: ["/bin/sh","-c","test -f base.txt"]'
    assert runstate(run)['integration_check'] == result['integration_check']
    report = [c['text'] for c in data('task', '--repo', 'demo', 'comments', 'list', issue)
              if c['text'].startswith('Integrated locally')]
    assert len(report) == 1, report
    assert 'Integration check: ["/bin/sh","-c","test -f base.txt"] passed.' in report[0]
    assert report[0].endswith('Rat-reported checks (not verified by integration): file verified')
    assert data('task', '--repo', 'demo', 'ready', '--json') is not None
    assert git(repo, 'rev-parse', 'main') == main_before
    assert git(repo, 'branch', '--show-current') == 'main'
    assert (repo / 'user-dirty.txt').read_text() == 'must survive\n'
    assert 'new.txt' in git(repo, 'ls-tree', '--name-only', 'rk/integration')
    git(repo, 'worktree', 'remove', str(wt))
    rk('clean', run['id'])
    assert not wt.exists()
    print('PASS lifecycle, local integration, ticket closure, checkout preservation, cleanup')

    issue = task('demo', 'blocked result')
    run = data('spawn', 'rat', '--repo', 'demo', '--task', issue)
    wait_exit(run)
    rk('block', run['id'], '--summary', 'needs human decision')
    assert rk('integrate', run['id'], ok=False).returncode
    assert data('task', '--repo', 'demo', 'show', issue)[0]['status'] == 'in_progress'
    stopped(run)
    rk('clean', run['id'], '--discard')
    assert data('task', '--repo', 'demo', 'show', issue)[0]['status'] == 'open'
    print('PASS blocked outcome does not close ticket; explicit discard releases claim')

    # The king closes a discarded task without integration; nothing else may.
    def close(task_id, *args, extra=None):
        return rk('task', '--repo', 'demo', 'close', task_id, *args, ok=False, extra=extra)
    assert 'usage' in close(issue).stderr
    assert '--reason is required' in close(issue, '--reason', '  ').stderr
    denied = close(issue, '--reason', 'abandoned', extra={'RK_ROLE': 'rat', 'RK_RUN': run['id']})
    assert denied.returncode and 'rats cannot close' in denied.stderr
    assert data('task', '--repo', 'demo', 'show', issue)[0]['status'] == 'open'
    live = data('spawn', 'rat', '--repo', 'demo', '--task', task('demo', 'ready result'))
    wait_exit(live)
    rk('finish', live['id'], '--summary', 'no changes', '--checks', 'fixture')
    refused = close(live['task'], '--reason', 'not wanted')
    assert refused.returncode and live['id'] in refused.stderr and 'discard' in refused.stderr
    assert data('task', '--repo', 'demo', 'show', live['task'])[0]['status'] == 'in_progress'
    stopped(live); rk('clean', live['id'], '--discard')
    closed = data('task', '--repo', 'demo', 'close', issue, '--reason', 'abandoned after discard')
    assert closed['status'] == 'closed', closed
    notes = data('task', '--repo', 'demo', 'comments', 'list', issue)
    assert any('without integration: abandoned after discard' in c['text'] for c in notes), notes
    again = close(issue, '--reason', 'again')
    assert again.returncode and 'already closed' in again.stderr
    assert data('task', '--repo', 'demo', 'show', issue)[0]['status'] == 'closed'
    print('PASS king closes discarded task with reason; live runs, rats, no reason, re-close refused')

    # Fail startup after ownership has been acquired, then resume that same run.
    issue = task('demo', 'recover failed startup')
    (root / 'badpath').mkdir()
    fake = root / 'badpath/tmux'
    fake.write_text('#!/bin/sh\nexit 42\n'); fake.chmod(0o700)
    failed = rk('spawn', 'rat', '--repo', 'demo', '--task', issue,
                ok=False, extra={'PATH': str(fake.parent) + ':' + env['PATH']})
    assert failed.returncode
    run = next(r for r in data('list', '--json') if r.get('task') == issue)
    assert run['state'] == 'startup_failed' and run['claimed']
    rk('resume', run['id'], '--message', 'continue after tmux failure')
    wait_exit(run); stopped(run)
    rk('clean', run['id'], '--discard')
    print('PASS claim-to-launch failure retains recoverable ownership')

    # Managed shell commands and absolute Git calls with inherited hook config.
    issue = task('demo', 'guards')
    hookdir = repo / '.git/hooks'
    (hookdir / 'resources').mkdir()
    (hookdir / 'resources/marker').write_text('existing hook resource')
    hook = hookdir / 'pre-commit'
    hook.write_text('#!/bin/sh\ntest -f "$(dirname "$0")/resources/marker"\n')
    hook.chmod(0o700)
    probe = root / 'guard-probe.sh'
    probe.write_text('''#!/bin/sh
set -eu
if git push origin main; then exit 31; fi
if git push --no-verify origin main; then exit 32; fi
if git -c alias.p=push p; then exit 33; fi
if git notes add -m forbidden; then exit 34; fi
if rk publish --remote origin --branch main --request invented; then exit 35; fi
git status --porcelain
printf 'hook-check\\n' > hook-check.txt
git add hook-check.txt
git commit -m 'verify original hook context'
printf 'guards-passed\\n' > guard-evidence.txt
''')
    probe.chmod(0o700)
    rk('config', '--repo', 'demo', '--command', json.dumps(['/bin/sh', str(probe)]))
    run = data('spawn', 'rat', '--repo', 'demo', '--task', issue)
    final = wait_exit(run)
    assert final['exit_code'] == 0, final
    assert (Path(run['worktree']) / 'guard-evidence.txt').read_text() == 'guards-passed\n'
    assert 'denied' in rk('logs', run['id']).stdout
    # Neither shared config nor main's hook lookup was overwritten.
    assert not command(['git', '-C', str(repo), 'config', '--get', 'core.hooksPath'], ok=False).stdout
    stopped(run); rk('clean', run['id'], '--discard')
    assert rk('publish', '--repo', 'demo', '--remote', 'origin', '--branch', 'main', ok=False).returncode
    print('PASS managed command guards, logs, no hook config changes, missing push authorization')

    # Conflicting results retain the source and candidate; target stays unchanged.
    rk('config', '--repo', 'demo', '--command', '["/bin/sh","-c","exit 0"]')
    a = data('spawn', 'rat', '--repo', 'demo', '--task', task('demo', 'conflict a'))
    b = data('spawn', 'rat', '--repo', 'demo', '--task', task('demo', 'conflict b'))
    for r, content in ((a, 'a\n'), (b, 'b\n')):
        wait_exit(r)
        (Path(r['worktree']) / 'base.txt').write_text(content)
        git(r['worktree'], 'add', 'base.txt'); git(r['worktree'], 'commit', '-m', content.strip())
        rk('finish', r['id'], '--summary', content.strip(), '--checks', 'fixture')
        stopped(r)
    rk('integrate', a['id'])
    before = git(repo, 'rev-parse', 'rk/integration')
    assert rk('integrate', b['id'], ok=False).returncode
    assert git(repo, 'rev-parse', 'rk/integration') == before
    assert Path(b['worktree']).exists()
    assert git(repo, 'rev-parse', b['branch'])
    assert 'preserved' in runstate(b)['error']
    assert rk('clean', b['id'], ok=False).returncode
    print('PASS merge conflict retains work and leaves integration target unchanged')

    # Actual process supervision, stop and resume; no shell-screen heuristics.
    rk('config', '--repo', 'demo', '--command', '["/bin/sh","-c","sleep 300"]', '--max-rats', '1')
    run = data('spawn', 'rat', '--repo', 'demo', '--task', task('demo', 'stop running worker'))
    until = time.monotonic() + 10
    while runstate(run)['process'] != 'running' and time.monotonic() < until:
        time.sleep(.1)
    assert runstate(run)['process'] == 'running'
    assert rk('spawn', 'rat', '--repo', 'demo', '--task', task('demo', 'capacity'), ok=False).returncode
    stopped(run)
    assert runstate(run)['process'] == 'stopped'
    assert Path(run['worktree']).exists()
    print('PASS process supervision, bounded concurrency, non-destructive stop')
    # Crash the launcher itself and verify retained work/reconciliation.
    rk('resume', run['id'], '--message', 'crash probe')
    until = time.monotonic() + 10
    while runstate(run)['process'] != 'running' and time.monotonic() < until:
        time.sleep(.1)
    crash_state = runstate(run)
    os.kill(crash_state['pid'], signal.SIGKILL)
    time.sleep(.3)
    rk('reconcile')
    assert Path(run['worktree']).exists()
    stopped(run)
    rk('config', '--repo', 'demo', '--command', '["/bin/sh","-c","exit 7"]')
    rk('resume', run['id'], '--message', 'resume after launcher crash')
    final = wait_exit(run)
    assert final['exit_code'] == 7, final
    assert final['state'] == 'working', final
    stopped(run)
    rk('clean', run['id'], '--discard')
    print('PASS launcher crash recovery and real nonzero exit receipt')

    # The agent owns the pane's terminal: it sees resizes and receives SIGWINCH.
    resize = ("trap 'stty size > tty-size.local; exit 0' WINCH; : > ready.local; "
              "i=0; while [ $i -lt 100 ]; do sleep .1; i=$((i+1)); done; exit 3")
    rk('config', '--repo', 'demo', '--command', json.dumps(['/bin/sh', '-c', resize]), '--max-rats', '4')
    run = data('spawn', 'rat', '--repo', 'demo', '--task', task('demo', 'follow pane size'))
    ready = Path(run['worktree']) / 'ready.local'
    until = time.monotonic() + 10
    while not ready.exists() and time.monotonic() < until:
        time.sleep(.1)
    assert ready.exists(), runstate(run)
    command(['tmux', 'resize-window', '-t', '=' + run['session'], '-x', '132', '-y', '40'])
    final = wait_exit(run)
    assert final['exit_code'] == 0, final
    assert (Path(run['worktree']) / 'tty-size.local').read_text().split() == ['40', '132']
    stopped(run); rk('clean', run['id'], '--discard')
    print('PASS agent terminal follows tmux pane resize')

    rk('config', '--repo', 'demo', '--command', '["/bin/sh","-c","exit 0"]')
    issue = task('demo', 'exclusive claim')
    def compete(_):
        return rk('spawn', 'rat', '--repo', 'demo', '--task', issue, ok=False)
    with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
        attempts = list(pool.map(compete, range(4)))
    assert sum(p.returncode == 0 for p in attempts) == 1
    winning = json.loads(next(p.stdout for p in attempts if p.returncode == 0))
    wait_exit(winning); stopped(winning)
    print('PASS four concurrent rk claim attempts produce one winner')

    # Delivery failure saves the result, then retries without duplicate comments.
    realbr = shutil.which('br')
    fault = root / 'fault-br.sh'
    fault.write_text('#!/bin/sh\nif [ "$1" = comments ]; then exit 77; fi\nexec ' + realbr + ' "$@"\n')
    fault.chmod(0o700)
    assert rk('finish', winning['id'], '--summary', 'delivery probe', '--checks', 'no changes required',
              ok=False, extra={'RK_REAL_BR': str(fault)}).returncode
    saved = runstate(winning)
    assert saved['state'] == 'ready' and any(not m['delivered'] for m in saved['messages'])
    rk('reconcile'); rk('reconcile')
    saved = runstate(winning)
    assert all(m['delivered'] for m in saved['messages'])
    comments = data('task', '--repo', 'demo', 'comments', 'list', issue)
    assert sum('delivery probe' in c['text'] for c in comments) == 1
    print('PASS durable comment retry and deduplication')

    # Failed checks and closure retries cannot silently discard or re-merge work.
    before = git(repo, 'rev-parse', 'rk/integration')
    rk('config', '--repo', 'demo', '--check', '["/bin/sh","-c","exit 9"]')
    assert rk('integrate', winning['id'], ok=False).returncode
    assert git(repo, 'rev-parse', 'rk/integration') == before
    assert Path(winning['worktree']).exists()
    rk('config', '--repo', 'demo', '--check', '["/bin/sh","-c","exit 0"]')
    fault.write_text('#!/bin/sh\nif [ "$1" = close ]; then exit 78; fi\nexec ' + realbr + ' "$@"\n')
    assert rk('integrate', winning['id'], ok=False, extra={'RK_REAL_BR': str(fault)}).returncode
    saved = runstate(winning)
    assert saved['state'] == 'integrated' and not saved['closed']
    integrated = git(repo, 'rev-parse', 'rk/integration')
    rk('reconcile')
    assert runstate(winning)['closed']
    assert git(repo, 'rev-parse', 'rk/integration') == integrated
    print('PASS failed checks preserve target; closure retry does not repeat integration')

    # A checked-out target is never advanced behind another checkout's index.
    other = root / 'target-checkout'
    git(repo, 'worktree', 'add', str(other), 'rk/integration')
    issue = task('demo', 'checked out target')
    run = data('spawn', 'rat', '--repo', 'demo', '--task', issue)
    wait_exit(run); stopped(run)
    rk('finish', run['id'], '--summary', 'no changes', '--checks', 'fixture')
    assert rk('integrate', run['id'], ok=False).returncode
    assert git(other, 'status', '--porcelain') == ''
    print('PASS checked-out integration target is protected')

    # Without a configured check, integration still works but says plainly
    # that no repository check ran; the rat's claim is labelled as its own.
    fixture('nocheck', check=None)
    issue = task('nocheck', 'unchecked integration')
    run = data('spawn', 'rat', '--repo', 'nocheck', '--task', issue)
    wait_exit(run); stopped(run)
    wt = Path(run['worktree'])
    (wt / 'unchecked.txt').write_text('unchecked\n')
    git(wt, 'add', 'unchecked.txt')
    git(wt, 'commit', '-m', 'unchecked work')
    rk('finish', run['id'], '--summary', 'done', '--checks', 'make check passed')
    p = rk('integrate', run['id'])
    result = json.loads(p.stdout)
    assert result['state'] == 'integrated' and result['integration_check'] == 'none configured'
    assert 'no repository check ran' in p.stderr and 'rk config --repo nocheck --check JSON' in p.stderr
    assert runstate(run)['integration_check'] == 'none configured'
    report = [c['text'] for c in data('task', '--repo', 'nocheck', 'comments', 'list', issue)
              if c['text'].startswith('Integrated locally')]
    assert len(report) == 1, report
    assert 'Integration check: none configured for this repository; no repository check ran.' in report[0]
    assert 'rk config --repo nocheck --check JSON' in report[0]
    assert 'passed' not in report[0].split('Rat-reported checks (not verified by integration): ')[0]
    assert report[0].endswith('Rat-reported checks (not verified by integration): make check passed')
    print('PASS integration reports its own check, or that none ran, apart from the rat\'s claim')

    # Scripted agents record every line typed into their terminal.
    echo = root / 'echo-agent.py'
    echo.write_text('import os, sys\n'
                    f'path = {str(root)!r} + "/" + os.environ["RK_RUN"] + ".typed"\n'
                    'while True:\n'
                    '    line = sys.stdin.readline()\n'
                    '    if not line:\n'
                    '        break\n'
                    '    with open(path, "a") as f:\n'
                    '        f.write(line)\n')
    def typed(run, text):
        path = root / f"{run['id']}.typed"
        until = time.monotonic() + 15
        while time.monotonic() < until:
            if path.exists() and text in path.read_text():
                return path.read_text()
            time.sleep(.1)
        raise AssertionError(f'{text!r} never typed into {run["name"]}: {path.read_text() if path.exists() else ""}')
    nudge = fixture('nudge')
    rk('config', '--repo', 'nudge', '--command', json.dumps(['python3', str(echo)]))
    king = data('spawn', 'king', '--repo', 'nudge')
    rat = data('spawn', 'rat', '--repo', 'nudge', '--task', task('nudge', 'nudged work'))
    # An unsent draft is cut to the start-of-line and kill-to-end keys before
    # the notice. The scripted agent's cooked terminal records them literally.
    until = time.monotonic() + 15
    while runstate(rat)['process'] != 'running' and time.monotonic() < until:
        time.sleep(.1)
    time.sleep(1)  # script flushes pending input when it sets up its terminal
    command(['tmux', 'send-keys', '-t', f"={rat['session']}:", '-l', 'half-typed draft'])
    rk('tell', rat['id'], 'use the second approach')
    assert 'half-typed draft\x01\x0b[rk] 1 new message(s)' in typed(rat, 'rk inbox')
    wt = Path(rat['worktree'])
    (wt / 'work.txt').write_text('work\n')
    git(wt, 'add', 'work.txt')
    git(wt, 'commit', '-m', 'work')
    rk('finish', rat['id'], '--summary', 'done', '--checks', 'none needed')
    typed(king, f"{rat['name']} (nudge) ready")
    stopped(rat)
    print('PASS notices typed into rat and king sessions reach the agent')
    restarted = data('restart', 'king')
    assert restarted['id'] == king['id'] and restarted['attempt'] == 2
    dismissed = data('dismiss', 'king')
    assert dismissed['state'] == 'dismissed' and dismissed['process'] == 'stopped'
    fresh = data('spawn', 'king', '--repo', 'nudge')
    assert fresh['id'] != king['id'] and data('show', 'king')['id'] == fresh['id']
    data('dismiss', 'king')
    print('PASS king restart, dismissal and fresh spawn')
    print(f'Acceptance artifacts: {root}')
finally:
    # Isolated server only: TMUX_TMPDIR points inside this test directory.
    command(['tmux', 'kill-server'], ok=False)
