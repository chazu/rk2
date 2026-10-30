package kingdom

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

const usage = `rat-kingdom — local swarms, explicit integration, no automatic pushes

rk init [PATH] [--name NAME] [--target BRANCH] [--max-rats N]
        [--command '["claude","{prompt}"]'] [--check '["go","test","./..."]']
rk repos
rk config [--repo NAME] [--command JSON] [--check JSON] [--max-rats N]
rk spawn rat|king [--repo NAME] [--task ID] [--name NAME]
rk list [--repo NAME]                    all runs if --repo is omitted
rk show RUN
rk attach|at RUN                       RUN may be 'king': one king, system-wide
rk logs RUN
rk prime [RUN]                          print role/task instructions
rk stop RUN                            retain all work
rk resume RUN [--message TEXT]         reuse worktree and claim
rk restart RUN [--message TEXT]        stop, then resume
rk dismiss king                        stop and retire the king; spawn a fresh one
rk finish [RUN] --summary TEXT --checks TEXT
rk block [RUN] --summary TEXT
rk tell RUN MESSAGE                    durable task comment
rk inbox [RUN] [--ack COMMENT_ID]        read or acknowledge task comments
rk wait [RUN] [--repo NAME] [--timeout 60s]   wait for results or unread comments
rk watch [--interval 5s]                reconcile and report changes; no typing
rk reconcile                           recover observations and retry comments
rk integrate RUN                       reviewed result -> local target -> close
rk clean RUN [--discard]                discard requires explicit user intent
rk task [--repo NAME] COMMAND...        curated local Beads operations
rk task [--repo NAME] close TASK --reason TEXT   king only: close without integration
rk publish --repo NAME --remote NAME --branch BRANCH --request TEXT

All commands accept --home PATH and --json. No sync or Git-notes functionality.
Default target rk/integration stays unchecked out so your checkout is untouched.
Use rk stop before integration. Local recovery needs no remote.
`

func reorder(fs *flag.FlagSet, args []string) ([]string, error) {
	opts, pos := []string{}, []string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			pos = append(pos, a)
			continue
		}
		name := strings.TrimLeft(a, "-")
		name, _, has := strings.Cut(name, "=")
		f := fs.Lookup(name)
		if f == nil {
			return nil, fmt.Errorf("unknown flag %s", a)
		}
		opts = append(opts, a)
		b, ok := f.Value.(interface{ IsBoolFlag() bool })
		if !has && (!ok || !b.IsBoolFlag()) {
			i++
			if i == len(args) {
				return nil, fmt.Errorf("missing value for %s", a)
			}
			opts = append(opts, args[i])
		}
	}
	return append(opts, pos...), nil
}

// closeArgs parses rk task close TASK --reason TEXT.
func closeArgs(args []string) (id, reason string, err error) {
	const use = "usage: rk task [--repo NAME] close TASK --reason TEXT"
	hasReason := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--json":
		case a == "--reason":
			if i+1 == len(args) {
				return "", "", errors.New(use)
			}
			reason, hasReason = args[i+1], true
			i++
		case strings.HasPrefix(a, "--reason="):
			reason, hasReason = strings.TrimPrefix(a, "--reason="), true
		case strings.HasPrefix(a, "-") || id != "":
			return "", "", errors.New(use)
		default:
			id = a
		}
	}
	if id == "" || !hasReason {
		return "", "", errors.New(use)
	}
	return id, reason, nil
}
func emit(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
func Main(args []string, w io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		_, e := io.WriteString(w, usage)
		return e
	}
	cmd := args[0]
	args = args[1:]
	if cmd == "at" {
		cmd = "attach"
	}
	if cmd == "version" {
		_, e := fmt.Fprintln(w, "rat-kingdom 0.1.0")
		return e
	}
	if cmd == "_check-command" {
		return CheckShell(os.Stdin)
	}
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	home := fs.String("home", "", "state directory")
	repoName := fs.String("repo", "", "repository")
	jsonOut := fs.Bool("json", false, "JSON output")
	var name, task, target, command, check, message, summary, checks, remote, branch, request string
	var max, ack int
	var discard bool
	var interval string
	switch cmd {
	case "init":
		fs.StringVar(&name, "name", "", "repository name")
		fs.StringVar(&target, "target", "", "integration branch")
		fs.StringVar(&command, "command", "", "agent argv JSON")
		fs.StringVar(&check, "check", "", "check argv JSON")
		fs.IntVar(&max, "max-rats", 4, "capacity")
	case "config":
		fs.StringVar(&command, "command", "", "agent argv JSON")
		fs.StringVar(&check, "check", "", "check argv JSON")
		fs.IntVar(&max, "max-rats", 0, "capacity")
	case "spawn":
		fs.StringVar(&name, "name", "", "agent name")
		fs.StringVar(&task, "task", "", "task ID")
	case "resume", "restart":
		fs.StringVar(&message, "message", "", "continuation")
	case "finish", "block":
		fs.StringVar(&summary, "summary", "", "summary")
		fs.StringVar(&checks, "checks", "", "validation evidence")
	case "inbox":
		fs.IntVar(&ack, "ack", 0, "comment cursor")
	case "clean":
		fs.BoolVar(&discard, "discard", false, "explicitly discard local work")
	case "publish":
		fs.StringVar(&remote, "remote", "", "remote")
		fs.StringVar(&branch, "branch", "", "branch")
		fs.StringVar(&request, "request", "", "explicit user request")
	case "wait":
		fs.StringVar(&interval, "timeout", "60s", "bounded wait")
	case "watch":
		fs.StringVar(&interval, "interval", "5s", "polling interval")
	}
	// Task arguments belong to br; extract only our routing flags.
	var taskArgs []string
	if cmd == "task" {
		for i := 0; i < len(args); i++ {
			a := args[i]
			if a == "--repo" || a == "--home" {
				if i+1 == len(args) {
					return errors.New("missing routing flag value")
				}
				if a == "--repo" {
					*repoName = args[i+1]
				} else {
					*home = args[i+1]
				}
				i++
			} else {
				taskArgs = append(taskArgs, a)
			}
		}
	} else {
		ordered, e := reorder(fs, args)
		if e != nil {
			return e
		}
		if e = fs.Parse(ordered); e != nil {
			return e
		}
	}
	s, e := Open(*home)
	if e != nil {
		return e
	}
	pos := fs.Args()
	one := func() (string, error) {
		if len(pos) > 1 {
			return "", errors.New("expected one run ID")
		}
		if len(pos) == 1 {
			return pos[0], nil
		}
		if id := os.Getenv("RK_RUN"); id != "" {
			return id, nil
		}
		return "", errors.New("run ID required")
	}
	// "king" always means the one king, including one spawned under an older name.
	resolve := func(id string) (string, error) {
		if id != "king" {
			return id, nil
		}
		r, e := s.King()
		if e != nil {
			return "", e
		}
		return r.ID, nil
	}
	if os.Getenv("RK_ROLE") == "rat" {
		switch cmd {
		case "finish", "block", "inbox", "prime", "show", "logs", "wait":
			id, e := one()
			if e != nil {
				return e
			}
			r, e := s.Run(id)
			if e != nil {
				return e
			}
			if r.ID != os.Getenv("RK_RUN") {
				return errors.New("rats may only operate on their own run")
			}
		case "list", "task":
		default:
			return fmt.Errorf("rats cannot invoke rk %s; ask the king", cmd)
		}
	}
	var value any
	switch cmd {
	case "init", "config":
		var argv, test []string
		if command != "" {
			argv, e = parseCommand(command)
			if e != nil {
				return e
			}
		}
		if check != "" {
			test, e = parseCommand(check)
			if e != nil {
				return e
			}
		}
		if cmd == "init" {
			path := "."
			if len(pos) > 1 {
				return errors.New("expected one repository path")
			}
			if len(pos) == 1 {
				path = pos[0]
			}
			value, e = s.Init(path, name, target, argv, test, max)
		} else {
			value, e = s.Config(*repoName, argv, test, max)
		}
	case "repos":
		value, e = s.Repos()
	case "spawn":
		if len(pos) != 1 {
			return errors.New("usage: rk spawn rat|king")
		}
		value, e = s.Spawn(*repoName, pos[0], task, name)
	case "list":
		var rs []*Run
		rs, e = s.Runs()
		if e != nil {
			return e
		}
		if *repoName != "" {
			repo, e := s.Repo(*repoName)
			if e != nil {
				return e
			}
			filtered := []*Run{}
			for _, r := range rs {
				if r.RepoID == repo.ID {
					filtered = append(filtered, r)
				}
			}
			rs = filtered
		}
		value = rs
		if !*jsonOut {
			fmt.Fprintln(w, "RUN           NAME                 ROLE  TASK                 RESULT          PROCESS")
			for _, r := range rs {
				fmt.Fprintf(w, "%-13s %-20s %-5s %-20s %-15s %s\n", r.ID, r.Name, r.Role, r.Task, r.State, r.Process)
			}
			return nil
		}
	case "show", "stop", "resume", "restart", "dismiss", "finish", "block", "inbox", "integrate", "clean", "logs", "attach", "prime":
		id, err := one()
		if err == nil {
			id, err = resolve(id)
		}
		if err != nil {
			return err
		}
		switch cmd {
		case "show":
			value, e = s.Run(id)
		case "stop":
			value, e = s.Stop(id)
		case "resume":
			value, e = s.Resume(id, message)
		case "restart":
			value, e = s.Restart(id, message)
		case "dismiss":
			value, e = s.Dismiss(id)
		case "finish":
			value, e = s.Result(id, "ready", summary, checks)
		case "block":
			value, e = s.Result(id, "blocked", summary, checks)
		case "inbox":
			value, e = s.Inbox(id, ack)
		case "integrate":
			value, e = s.Integrate(id)
		case "clean":
			value, e = s.Clean(id, discard)
		case "logs":
			return s.Logs(id, w)
		case "attach":
			r, e := s.Run(id)
			if e != nil {
				return e
			}
			c := exec.Command("tmux", "attach-session", "-t", "="+r.Session)
			c.Stdin = os.Stdin
			c.Stdout = os.Stdout
			c.Stderr = os.Stderr
			return c.Run()
		case "prime":
			r, e := s.Run(id)
			if e != nil {
				return e
			}
			repo, e := s.Repo(r.RepoID)
			if e != nil {
				return e
			}
			text, e := s.Prompt(repo, r)
			if e != nil {
				return e
			}
			_, e = fmt.Fprintln(w, text)
			return e
		}
	case "tell":
		if len(pos) != 2 {
			return errors.New("usage: rk tell RUN MESSAGE")
		}
		id, err := resolve(pos[0])
		if err != nil {
			return err
		}
		e = s.Tell(id, pos[1])
		value = map[string]string{"message": "recorded"}
	case "reconcile":
		value, e = s.Reconcile()
	case "wait":
		id := ""
		if len(pos) > 0 {
			id = pos[0]
		} else if os.Getenv("RK_ROLE") == "rat" {
			id = os.Getenv("RK_RUN")
		}
		return s.Wait(*repoName, id, interval, w)
	case "watch":
		return s.Watch(interval, w)
	case "task":
		if len(taskArgs) == 0 {
			return errors.New("usage: rk task [--repo NAME] create|show|ready|list|comments|dep|close ...")
		}
		if taskArgs[0] == "close" {
			if os.Getenv("RK_ROLE") == "rat" {
				return errors.New("rats cannot close tasks; the king closes them")
			}
			id, reason, err := closeArgs(taskArgs[1:])
			if err != nil {
				return err
			}
			value, e = s.CloseTask(*repoName, id, reason)
			break
		}
		allowed := " create show ready list comments dep blocked graph search stats "
		if !strings.Contains(allowed, " "+taskArgs[0]+" ") {
			return errors.New("unsupported task command; lifecycle owns claims and closure")
		}
		repo, e := s.Repo(*repoName)
		if e != nil {
			return e
		}
		if os.Getenv("RK_ROLE") == "rat" {
			if taskArgs[0] != "comments" && taskArgs[0] != "show" && taskArgs[0] != "ready" && taskArgs[0] != "list" && taskArgs[0] != "create" {
				return errors.New("rat task command denied")
			}
		}
		for _, a := range taskArgs {
			if strings.HasPrefix(a, "--db") || strings.HasPrefix(a, "--actor") || strings.HasPrefix(a, "--status") || strings.HasPrefix(a, "--assignee") {
				return fmt.Errorf("task option %s is reserved", a)
			}
		}
		actor := os.Getenv("RK_RUN")
		if actor == "" {
			actor = "king"
		}
		b, e := beads(repo, actor, taskArgs...)
		if e != nil {
			return e
		}
		_, e = w.Write(b)
		return e
	case "publish":
		e = s.Publish(*repoName, remote, branch, request)
		value = map[string]string{"published": branch, "remote": remote}
	case "_run":
		if len(pos) != 2 {
			return errors.New("launcher requires run and attempt")
		}
		attempt, e := strconv.Atoi(pos[1])
		if e != nil {
			return e
		}
		return s.RunProcess(pos[0], attempt)
	default:
		return fmt.Errorf("unknown command %q; run rk help", cmd)
	}
	if e != nil {
		return e
	}
	return emit(w, value)
}
