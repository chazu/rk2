package kingdom

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
)

// The Claude Bash hook judges what a command line would execute, so guarded
// words inside quoted data, heredoc bodies, and comments are allowed. It reads
// common POSIX/bash syntax; it is not a complete shell and not a sandbox.
// Scripts, other interpreters, and functions or aliases defined outside the
// command line are not inspected. Anything it cannot judge is denied.

const unknown = "\x00" // stands for text known only at run time

var (
	assignPrefix = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\[[^\]]*\])?\+?=`)
	arrayPrefix  = regexp.MustCompile(assignPrefix.String() + `$`)
	shellName    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	shellOps     = []string{"&>>", ";;&", "<<<", "<<-", "&&", "||", ";;", ";&", "|&", "<<", ">>", "<&", ">&", "<>", ">|", "&>", "|", "&", ";", "(", ")", "<", ">"}
	reserved     = []string{"if", "then", "else", "elif", "fi", "do", "done", "while", "until", "{", "}", "!", "for", "select", "case", "esac", "[[", "function"}
)

type shWord struct {
	val    string // literal text, with unknown in place of expansions
	unq    string // unquoted source text; quoted parts become \x01
	quoted bool
}

func (w shWord) dynamic() bool { return strings.Contains(w.val, unknown) }
func (w shWord) plain() bool   { return !w.quoted && !w.dynamic() }

// pattern reports unquoted glob or brace expansion.
func (w shWord) pattern() bool {
	if strings.ContainsAny(w.unq, "*?") {
		return true
	}
	if i := strings.IndexByte(w.unq, '['); i >= 0 && strings.IndexByte(w.unq[i:], ']') > 0 {
		return true
	}
	if i := strings.IndexByte(w.unq, '{'); i >= 0 {
		if j := strings.LastIndexByte(w.unq, '}'); j > i {
			in := w.unq[i+1 : j]
			return strings.Contains(in, ",") || strings.Contains(in, "..")
		}
	}
	return false
}
func show(w shWord) string { return fmt.Sprintf("%q", strings.ReplaceAll(w.val, unknown, "$…")) }

type shToken struct {
	op   string // "" for a word
	word shWord
}

type hereDoc struct {
	delim                string
	strip, quoted, ready bool
	body                 string
	cmd                  *shCmd
}

type shCmd struct {
	assigns, words []shWord
	strs           []shWord // here-strings
	piped, shell   bool     // shell: an interpreter reads commands from stdin
	judged         bool
}

type shParser struct {
	src     []rune
	pos     int
	depth   int
	pending []*hereDoc
	docs    []*hereDoc
}

func denied(format string, a ...any) error {
	return fmt.Errorf("managed command denied: "+format, a...)
}
func undecidable(format string, a ...any) error {
	return fmt.Errorf("managed command denied, cannot judge it: "+format, a...)
}
func realName(n string) bool { return n == "RK_REAL_GIT" || n == "RK_REAL_BR" }
func protectedVar(n string) bool {
	return n == "RK_ROLE" || n == "RK_RUN" || realName(n) || strings.HasPrefix(n, "GIT_CONFIG")
}
func nameBefore(s string) string {
	if i := strings.IndexByte(s, '='); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSuffix(s, "+")
	if i := strings.IndexByte(s, '['); i >= 0 {
		s = s[:i]
	}
	return s
}

func checkShellCommand(src string) error { return parseShell(src, 0) }

func parseShell(src string, depth int) error {
	if depth > 16 {
		return undecidable("commands nested too deeply")
	}
	p := &shParser{src: []rune(src), depth: depth}
	if e := p.list(false); e != nil {
		return e
	}
	return p.flushDocs(true)
}

func (p *shParser) at(off int) rune {
	if p.pos+off < len(p.src) {
		return p.src[p.pos+off]
	}
	return -1
}
func (p *shParser) has(s string) bool {
	return p.pos+len(s) <= len(p.src) && string(p.src[p.pos:p.pos+len(s)]) == s
}

func (p *shParser) token() (shToken, error) {
	for {
		c := p.at(0)
		if c == ' ' || c == '\t' {
			p.pos++
		} else if c == '\\' && p.at(1) == '\n' {
			p.pos += 2
		} else if c == '#' {
			for p.at(0) >= 0 && p.at(0) != '\n' {
				p.pos++
			}
		} else {
			break
		}
	}
	c := p.at(0)
	switch {
	case c < 0:
		return shToken{op: "eof"}, nil
	case c == '\n':
		p.pos++
		return shToken{op: "\n"}, p.readDocs()
	case p.has("(("):
		p.pos += 2
		return shToken{op: "arith"}, p.arith()
	case (c == '<' || c == '>') && p.at(1) == '(':
		w, e := p.word()
		return shToken{word: w}, e
	}
	n := 0
	for p.at(n) >= '0' && p.at(n) <= '9' {
		n++
	}
	if n > 0 && (p.at(n) == '<' || p.at(n) == '>') {
		p.pos += n
	}
	for _, op := range shellOps {
		if p.has(op) {
			p.pos += len(op)
			return shToken{op: op}, nil
		}
	}
	w, e := p.word()
	return shToken{word: w}, e
}

func (p *shParser) word() (shWord, error) {
	var val, unq strings.Builder
	w := shWord{}
	dyn := func() { val.WriteString(unknown); unq.WriteByte(1) }
	if c := p.at(0); (c == '<' || c == '>') && p.at(1) == '(' {
		p.pos += 2
		if e := p.nested(); e != nil {
			return w, e
		}
		dyn()
	}
	for {
		c := p.at(0)
		switch {
		case c == '(' && arrayPrefix.MatchString(unq.String()):
			// Array assignment: NAME=(words...)
			p.pos++
			for {
				for p.at(0) == ' ' || p.at(0) == '\t' || p.at(0) == '\n' {
					p.pos++
				}
				if p.at(0) == ')' {
					p.pos++
					break
				}
				if p.at(0) < 0 || strings.ContainsRune(";&|(<>", p.at(0)) {
					return w, undecidable("unsupported array assignment")
				}
				inner, e := p.word()
				if e != nil {
					return w, e
				}
				val.WriteString(inner.val + " ")
				unq.WriteByte(1)
			}
		case c < 0 || strings.ContainsRune(" \t\n;&|()<>", c):
			w.val, w.unq = val.String(), unq.String()
			return w, nil
		case c == '\\':
			p.pos++
			if p.at(0) == '\n' {
				p.pos++
				continue
			}
			if p.at(0) >= 0 {
				val.WriteRune(p.at(0))
				p.pos++
			}
			unq.WriteByte(1)
			w.quoted = true
		case c == '\'':
			p.pos++
			s, e := p.single()
			if e != nil {
				return w, e
			}
			val.WriteString(s)
			unq.WriteByte(1)
			w.quoted = true
		case c == '"' || c == '$' && p.at(1) == '"':
			if c == '$' {
				p.pos++
			}
			p.pos++
			if e := p.dquote(&val); e != nil {
				return w, e
			}
			unq.WriteByte(1)
			w.quoted = true
		case c == '$' && p.at(1) == '\'':
			p.pos += 2
			if e := p.ansiC(&val); e != nil {
				return w, e
			}
			unq.WriteByte(1)
			w.quoted = true
		case c == '$':
			if e := p.dollar(&val, false); e != nil {
				return w, e
			}
			unq.WriteByte(1)
		case c == '`':
			if e := p.backtick(); e != nil {
				return w, e
			}
			dyn()
		default:
			val.WriteRune(c)
			unq.WriteRune(c)
			p.pos++
		}
	}
}

func (p *shParser) single() (string, error) {
	start := p.pos
	for p.at(0) != '\'' {
		if p.at(0) < 0 {
			return "", undecidable("unterminated single quote")
		}
		p.pos++
	}
	p.pos++
	return string(p.src[start : p.pos-1]), nil
}

// dquote reads a double-quoted string after its opening quote.
func (p *shParser) dquote(val *strings.Builder) error {
	for {
		c := p.at(0)
		switch {
		case c < 0:
			return undecidable("unterminated double quote")
		case c == '"':
			p.pos++
			return nil
		case c == '\\':
			n := p.at(1)
			if n == '\n' {
				p.pos += 2
			} else if n >= 0 && strings.ContainsRune("$`\"\\", n) {
				val.WriteRune(n)
				p.pos += 2
			} else {
				val.WriteRune('\\')
				p.pos++
			}
		case c == '$':
			if e := p.dollar(val, true); e != nil {
				return e
			}
		case c == '`':
			if e := p.backtick(); e != nil {
				return e
			}
			val.WriteString(unknown)
		default:
			val.WriteRune(c)
			p.pos++
		}
	}
}

// ansiC decodes $'...' after its opening quote.
func (p *shParser) ansiC(val *strings.Builder) error {
	num := func(base, max int) (rune, bool) {
		var v rune
		n := 0
		for ; n < max; n++ {
			d := strings.IndexRune("0123456789abcdef"[:base], p.at(0)|0x20)
			if p.at(0) < 0 || d < 0 {
				break
			}
			v = v*rune(base) + rune(d)
			p.pos++
		}
		return v, n > 0
	}
	for {
		c := p.at(0)
		if c < 0 {
			return undecidable("unterminated $'...' string")
		}
		p.pos++
		if c == '\'' {
			return nil
		}
		if c != '\\' {
			val.WriteRune(c)
			continue
		}
		n := p.at(0)
		p.pos++
		if r, ok := map[rune]rune{'a': 7, 'b': 8, 'e': 27, 'E': 27, 'f': 12, 'n': 10, 'r': 13, 't': 9, 'v': 11}[n]; ok {
			val.WriteRune(r)
			continue
		}
		switch n {
		case -1:
			return undecidable("unterminated $'...' string")
		case 'x', 'u', 'U':
			if r, ok := num(16, map[rune]int{'x': 2, 'u': 4, 'U': 8}[n]); ok {
				val.WriteRune(r)
			} else {
				val.WriteRune('\\')
				val.WriteRune(n)
			}
		case '0', '1', '2', '3', '4', '5', '6', '7':
			p.pos--
			r, _ := num(8, 3)
			val.WriteRune(r)
		case 'c':
			val.WriteRune(p.at(0) & 0x1f)
			p.pos++
		default:
			val.WriteRune(n)
		}
	}
}

// dollar reads an expansion starting at '$'.
func (p *shParser) dollar(val *strings.Builder, inDQ bool) error {
	n := p.at(1)
	switch {
	case n == '(' && p.at(2) == '(':
		p.pos += 3
		if e := p.arith(); e != nil {
			return e
		}
	case n == '(':
		p.pos += 2
		if e := p.nested(); e != nil {
			return e
		}
	case n == '{':
		p.pos += 2
		if e := p.param(inDQ); e != nil {
			return e
		}
	case n == '_' || n >= 'a' && n <= 'z' || n >= 'A' && n <= 'Z':
		j := 1
		for c := p.at(j); c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'; c = p.at(j) {
			j++
		}
		name := string(p.src[p.pos+1 : p.pos+j])
		p.pos += j
		if realName(name) {
			return denied("expands %s", name)
		}
	case n >= '0' && n <= '9' || n >= 0 && strings.ContainsRune("@*#?$!-", n):
		p.pos += 2
	default:
		val.WriteRune('$')
		p.pos++
		return nil
	}
	val.WriteString(unknown)
	return nil
}

// param reads ${...} after its opening brace.
func (p *shParser) param(inDQ bool) error {
	if p.at(0) == '!' || p.at(0) == '#' {
		p.pos++
	}
	start := p.pos
	for c := p.at(0); c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'; c = p.at(0) {
		p.pos++
	}
	name := string(p.src[start:p.pos])
	if realName(name) {
		return denied("expands %s", name)
	}
	if protectedVar(name) && (p.has("=") || p.has(":=")) {
		return denied("assigns %s", name)
	}
	var sink strings.Builder
	depth := 0
	for {
		c := p.at(0)
		switch {
		case c < 0:
			return undecidable("unterminated ${...}")
		case c == '}':
			p.pos++
			if depth == 0 {
				return nil
			}
			depth--
		case c == '{':
			depth++
			p.pos++
		case c == '\\':
			p.pos += 2
		case c == '\'' && !inDQ:
			p.pos++
			if _, e := p.single(); e != nil {
				return e
			}
		case c == '"':
			p.pos++
			if e := p.dquote(&sink); e != nil {
				return e
			}
		case c == '$':
			if e := p.dollar(&sink, inDQ); e != nil {
				return e
			}
		case c == '`':
			if e := p.backtick(); e != nil {
				return e
			}
		default:
			p.pos++
		}
	}
}

// arith reads arithmetic after its opening "((", judging any substitutions.
func (p *shParser) arith() error {
	var sink strings.Builder
	depth := 0
	for {
		c := p.at(0)
		switch {
		case c < 0:
			return undecidable("unterminated arithmetic")
		case c == '(':
			depth++
			p.pos++
		case c == ')':
			if depth == 0 {
				if p.at(1) != ')' {
					return undecidable("unbalanced arithmetic")
				}
				p.pos += 2
				return nil
			}
			depth--
			p.pos++
		case c == '\\':
			p.pos += 2
		case c == '\'':
			p.pos++
			if _, e := p.single(); e != nil {
				return e
			}
		case c == '"':
			p.pos++
			if e := p.dquote(&sink); e != nil {
				return e
			}
		case c == '$':
			if e := p.dollar(&sink, true); e != nil {
				return e
			}
		case c == '`':
			if e := p.backtick(); e != nil {
				return e
			}
		default:
			p.pos++
		}
	}
}

// nested judges a $(...) or <(...) body in place, up to its closing ')'.
func (p *shParser) nested() error {
	p.depth++
	defer func() { p.depth-- }()
	if p.depth > 16 {
		return undecidable("commands nested too deeply")
	}
	return p.list(true)
}

func (p *shParser) backtick() error {
	p.pos++
	var b strings.Builder
	for {
		c := p.at(0)
		if c < 0 {
			return undecidable("unterminated backquote")
		}
		p.pos++
		if c == '`' {
			return parseShell(b.String(), p.depth+1)
		}
		if c == '\\' && p.at(0) >= 0 && strings.ContainsRune("$`\\", p.at(0)) {
			c = p.at(0)
			p.pos++
		}
		b.WriteRune(c)
	}
}

// readDocs reads pending heredoc bodies after a newline.
func (p *shParser) readDocs() error {
	for _, d := range p.pending {
		var body strings.Builder
		for p.pos < len(p.src) {
			end := p.pos
			for end < len(p.src) && p.src[end] != '\n' {
				end++
			}
			line := string(p.src[p.pos:end])
			p.pos = min(end+1, len(p.src))
			if d.strip {
				line = strings.TrimLeft(line, "\t")
			}
			if line == d.delim {
				break
			}
			body.WriteString(line + "\n")
		}
		d.body, d.ready = body.String(), true
		p.docs = append(p.docs, d)
	}
	p.pending = nil
	return nil
}

// flushDocs judges read heredocs: unquoted bodies are expanded, and bodies a
// shell reads from stdin are commands.
func (p *shParser) flushDocs(all bool) error {
	rest := p.docs[:0]
	for _, d := range p.docs {
		if !all && !d.cmd.judged {
			rest = append(rest, d)
			continue
		}
		if !d.quoted {
			if e := scanExpansions(d.body, p.depth+1); e != nil {
				return e
			}
		}
		if d.cmd.shell {
			if e := parseShell(d.body, p.depth+1); e != nil {
				return e
			}
		}
	}
	p.docs = rest
	return nil
}

func scanExpansions(text string, depth int) error {
	p := &shParser{src: []rune(text), depth: depth}
	var sink strings.Builder
	for p.pos < len(p.src) {
		var e error
		switch p.src[p.pos] {
		case '\\':
			p.pos += 2
		case '$':
			e = p.dollar(&sink, true)
		case '`':
			e = p.backtick()
		default:
			p.pos++
		}
		if e != nil {
			return e
		}
	}
	return nil
}

// list reads commands until EOF, or until an unmatched ')' when stop is set,
// judging each simple command as it ends.
func (p *shParser) list(stop bool) error {
	c := &shCmd{}
	finish := func() error {
		e := p.judge(c)
		c = &shCmd{}
		return e
	}
	mode := "" // for, forvar, case, in, pattern, cond, func
	cases, groups, funcParen := 0, 0, false
	for {
		t, e := p.token()
		if e != nil {
			return e
		}
		switch t.op {
		case "eof":
			if stop || groups > 0 || cases > 0 {
				return undecidable("unterminated command group")
			}
			return finish()
		case "arith":
		case "(":
			switch {
			case mode == "pattern":
			case len(c.words)+len(c.assigns) == 0:
				groups++
			default: // name() function definition
				c, funcParen = &shCmd{}, true
			}
		case ")":
			switch {
			case mode == "pattern":
				mode = ""
			case funcParen:
				funcParen = false
			case groups > 0:
				groups--
				if e = finish(); e != nil {
					return e
				}
			case stop:
				return finish()
			default:
				return undecidable("unexpected ')'")
			}
		case "|", "|&":
			if mode == "pattern" {
				continue
			}
			if e = finish(); e != nil {
				return e
			}
			c.piped = true
		case ";;", ";&", ";;&", ";", "&", "&&", "||", "\n":
			if e = finish(); e != nil {
				return e
			}
			if mode == "for" || mode == "forvar" {
				mode = ""
			}
			if cases > 0 && (t.op == ";;" || t.op == ";&" || t.op == ";;&") {
				mode = "pattern"
			}
			if t.op == "\n" {
				if e = p.flushDocs(false); e != nil {
					return e
				}
			}
		case "":
			w := t.word
			switch {
			case mode == "cond":
				if w.plain() && w.val == "]]" {
					mode = ""
				}
			case mode == "forvar":
				if protectedVar(w.val) {
					return denied("assigns %s", w.val)
				}
				mode = "for"
			case mode == "for":
			case mode == "case":
				mode = "in"
			case mode == "in":
				if w.val != "in" {
					return undecidable("case without 'in'")
				}
				mode = "pattern"
			case mode == "pattern":
				if w.plain() && w.val == "esac" {
					cases--
					mode = ""
				}
			case mode == "func":
				mode = ""
			case len(c.words)+len(c.assigns) == 0 && w.plain() && slices.Contains(reserved, w.val):
				switch w.val {
				case "for", "select":
					mode = "forvar"
				case "case":
					cases++
					mode = "case"
				case "esac":
					cases--
				case "[[":
					mode = "cond"
				case "function":
					mode = "func"
				}
			case len(c.words) == 0 && assignPrefix.MatchString(w.unq):
				c.assigns = append(c.assigns, w)
			default:
				c.words = append(c.words, w)
			}
		default: // redirection
			target, e := p.token()
			if e != nil {
				return e
			}
			if target.op != "" {
				return undecidable("redirection %s without a target", t.op)
			}
			switch t.op {
			case "<<", "<<-":
				if target.word.dynamic() {
					return undecidable("heredoc delimiter built at run time")
				}
				p.pending = append(p.pending, &hereDoc{delim: target.word.val, strip: t.op == "<<-", quoted: target.word.quoted, cmd: c})
			case "<<<":
				c.strs = append(c.strs, target.word)
			}
		}
	}
}

func (p *shParser) judge(c *shCmd) error {
	c.judged = true
	for _, a := range c.assigns {
		if n := nameBefore(a.val); protectedVar(n) {
			return denied("assigns %s", n)
		}
	}
	return p.run(c, c.words, false)
}

// run judges one command. more means arguments are appended at run time.
func (p *shParser) run(c *shCmd, words []shWord, more bool) error {
	if len(words) == 0 {
		return nil
	}
	w := words[0]
	if w.dynamic() || w.pattern() {
		return undecidable("command name %s is built at run time", show(w))
	}
	if strings.Contains(w.val, "RK_REAL_GIT") || strings.Contains(w.val, "RK_REAL_BR") {
		return denied("names the real Git or Beads executable")
	}
	// Case-insensitive file systems run GIT as git.
	name, args := strings.ToLower(path.Base(w.val)), words[1:]
	switch name {
	case "git":
		return p.git(c, args, more)
	case "git-send-pack", "git-receive-pack":
		return denied("runs %s; publication is only through the king's explicit rk publish", name)
	case "env":
		return p.env(c, args, more)
	case "command", "builtin", "nohup", "setsid", "time", "nice", "exec", "doas", "stdbuf":
		opts := map[string]string{"time": "fo", "nice": "n", "exec": "a", "doas": "uC", "stdbuf": "ioe"}[name]
		rest, e := operands(args, opts, "--adjustment", "--input", "--output", "--error")
		if e != nil {
			return e
		}
		return p.run(c, rest, more)
	case "sudo":
		rest, e := operands(args, "CDghpRrtTUu", "--close-from", "--chdir", "--group", "--host", "--prompt", "--chroot", "--role", "--type", "--command-timeout", "--other-user", "--user")
		if e != nil {
			return e
		}
		for len(rest) > 0 && strings.Contains(rest[0].val, "=") && shellName.MatchString(nameBefore(rest[0].val)) {
			if n := nameBefore(rest[0].val); protectedVar(n) {
				return denied("assigns %s", n)
			}
			rest = rest[1:]
		}
		return p.run(c, rest, more)
	case "timeout":
		rest, e := operands(args, "ks", "--kill-after", "--signal")
		if e != nil || len(rest) == 0 {
			return e
		}
		return p.run(c, rest[1:], more)
	case "xargs":
		rest, e := operands(args, "aEdILnPs", "--arg-file", "--delimiter", "--max-args", "--max-procs", "--max-chars", "--process-slot-var")
		if e != nil {
			return e
		}
		rep, opts := "", args[:len(args)-len(rest)]
		for i, o := range opts {
			switch v := o.val; {
			case v == "-I" && i+1 < len(opts):
				rep = opts[i+1].val
			case v == "-i" || v == "--replace":
				rep = "{}"
			case strings.HasPrefix(v, "--replace="):
				rep = v[len("--replace="):]
			case strings.HasPrefix(v, "-I") || strings.HasPrefix(v, "-i"):
				rep = v[2:]
			}
		}
		return p.run(c, runtime(rest, rep), true)
	case "find":
		for i := 0; i < len(args); i++ {
			if v := args[i].val; v == "-exec" || v == "-execdir" || v == "-ok" || v == "-okdir" {
				j := i + 1
				for j < len(args) && args[j].val != ";" && args[j].val != "+" {
					j++
				}
				if e := p.run(c, runtime(args[i+1:j], "{}"), false); e != nil {
					return e
				}
				i = j
			}
		}
	case "eval":
		return p.text(args, "eval")
	case "bash", "sh", "zsh", "dash", "ksh", "mksh", "ash":
		return p.shell(c, args)
	case "export", "declare", "typeset", "local", "readonly":
		for _, a := range args {
			if strings.HasPrefix(a.val, "-") || strings.HasPrefix(a.val, "+") {
				continue
			}
			n := nameBefore(a.val)
			if strings.Contains(n, unknown) {
				return undecidable("%s of a name built at run time", name)
			}
			if protectedVar(n) {
				return denied("%s %s", name, n)
			}
		}
	case "unset":
		for _, a := range args {
			if a.dynamic() {
				return undecidable("unset of a name built at run time")
			}
			if strings.HasPrefix(a.val, "RK_") || protectedVar(a.val) {
				return denied("unsets %s", a.val)
			}
		}
	case "read", "mapfile", "readarray":
		for _, a := range args {
			if protectedVar(a.val) {
				return denied("assigns %s", a.val)
			}
		}
	case "printf":
		for i, a := range args {
			n := ""
			if a.val == "-v" && i+1 < len(args) {
				n = args[i+1].val
			} else if strings.HasPrefix(a.val, "-v") {
				n = a.val[2:]
			}
			if protectedVar(n) {
				return denied("assigns %s", n)
			}
		}
	case "alias":
		for _, a := range args {
			if _, def, ok := strings.Cut(a.val, "="); ok {
				if e := p.text([]shWord{{val: def}}, "alias"); e != nil {
					return e
				}
			}
		}
	case "trap":
		if rest, e := operands(args, ""); e == nil && len(rest) > 0 && rest[0].val != "-" {
			return p.text(rest[:1], "trap")
		}
	}
	return nil
}

// runtime marks words containing a placeholder that is replaced at run time.
func runtime(words []shWord, placeholder string) []shWord {
	out := slices.Clone(words)
	for i := range out {
		if placeholder != "" && strings.Contains(out[i].val, placeholder) {
			out[i].val += unknown
		}
	}
	return out
}

// text judges words that a builtin joins and runs as shell text.
func (p *shParser) text(args []shWord, what string) error {
	parts := make([]string, len(args))
	for i, a := range args {
		if a.dynamic() {
			return undecidable("%s of text built at run time", what)
		}
		parts[i] = a.val
	}
	return parseShell(strings.Join(parts, " "), p.depth+1)
}

// operands skips a wrapper's options. withArg lists short options that take
// a separate value; long lists long options that do.
func operands(args []shWord, withArg string, long ...string) ([]shWord, error) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a.dynamic() {
			return nil, undecidable("option %s is built at run time", show(a))
		}
		v := a.val
		switch {
		case v == "--":
			return args[i+1:], nil
		case len(v) < 2 || v[0] != '-':
			return args[i:], nil
		case strings.HasPrefix(v, "--"):
			if slices.Contains(long, v) {
				i++
			}
		default:
			for j := 1; j < len(v); j++ {
				if strings.IndexByte(withArg, v[j]) >= 0 {
					if j == len(v)-1 {
						i++
					}
					break
				}
			}
		}
	}
	return nil, nil
}

func (p *shParser) env(c *shCmd, args []shWord, more bool) error {
	for i := 0; i < len(args); i++ {
		v, unset := args[i].val, ""
		switch {
		case v == "-" || v == "--ignore-environment" || len(v) > 1 && v[0] == '-' && v[1] != '-' && strings.ContainsRune(v, 'i'):
			return denied("env would clear the managed environment")
		case v == "-u" || v == "--unset":
			if i++; i < len(args) {
				unset = args[i].val
			}
		case strings.HasPrefix(v, "--unset="):
			unset = v[len("--unset="):]
		case strings.HasPrefix(v, "-u"):
			unset = v[2:]
		case strings.HasPrefix(v, "-S") || strings.HasPrefix(v, "--split-string"):
			return undecidable("env -S splits a command string at run time")
		case v == "-C" || v == "-P" || v == "--chdir":
			i++
		case v == "--":
		case strings.HasPrefix(v, "-"):
		case strings.Contains(v, "="):
			n := nameBefore(v)
			if strings.Contains(n, unknown) {
				return undecidable("env assignment to a name built at run time")
			}
			if protectedVar(n) {
				return denied("assigns %s", n)
			}
		default:
			return p.run(c, args[i:], more)
		}
		if strings.Contains(unset, unknown) || strings.HasPrefix(unset, "RK_") || protectedVar(unset) {
			return denied("env unsets %s", strings.ReplaceAll(unset, unknown, "$…"))
		}
	}
	return nil
}

// shell judges an interpreter's -c string, or its stdin when it reads commands
// there. Script files are not inspected.
func (p *shParser) shell(c *shCmd, args []shWord) error {
	cmdString, stdin, k := false, false, 0
	for ; k < len(args); k++ {
		v := args[k].val
		if args[k].dynamic() {
			return undecidable("shell option %s is built at run time", show(args[k]))
		}
		if v == "--" || v == "-" {
			k++
			break
		}
		if v == "--rcfile" || v == "--init-file" {
			k++
			continue
		}
		if strings.HasPrefix(v, "--") {
			continue
		}
		if len(v) < 2 || v[0] != '-' && v[0] != '+' {
			break
		}
		if v[0] == '-' {
			cmdString = cmdString || strings.ContainsRune(v, 'c')
			stdin = stdin || strings.ContainsRune(v, 's')
		}
		if strings.ContainsAny(v[1:], "oO") {
			k++
		}
	}
	switch {
	case cmdString:
		if k >= len(args) {
			return nil
		}
		if args[k].dynamic() {
			return undecidable("shell -c command built at run time")
		}
		return parseShell(args[k].val, p.depth+1)
	case k < len(args) && !stdin:
		return nil
	}
	c.shell = true
	if c.piped {
		return undecidable("a shell reads commands from a pipe")
	}
	for _, s := range c.strs {
		if e := p.text([]shWord{s}, "here-string"); e != nil {
			return e
		}
	}
	return nil
}

func gitConfigKey(kv string) error {
	k := strings.ToLower(nameBefore(kv))
	switch {
	case strings.Contains(k, unknown):
		return undecidable("Git configuration built at run time")
	case k == "core.hookspath":
		return denied("Git configuration %s bypasses the managed hooks", k)
	case strings.HasPrefix(k, "alias."), k == "include.path", strings.HasPrefix(k, "includeif."):
		return denied("Git configuration %s can run arbitrary Git commands", k)
	}
	return nil
}

func notesRef(v string) bool {
	for _, part := range strings.FieldsFunc(v, func(r rune) bool { return r == ':' || r == '=' }) {
		if strings.HasPrefix(strings.TrimLeft(part, "+^"), "refs/notes") {
			return true
		}
	}
	return false
}

func (p *shParser) git(c *shCmd, args []shWord, more bool) error {
	sub := -1
	for i := 0; i < len(args) && sub < 0; i++ {
		a := args[i]
		if a.dynamic() || a.pattern() {
			return undecidable("Git argument %s is built at run time", show(a))
		}
		switch v := a.val; {
		case v == "-C" || v == "--git-dir" || v == "--work-tree" || v == "--namespace" || v == "--super-prefix":
			i++
		case v == "-c" || v == "--config-env":
			if i++; i < len(args) {
				if e := gitConfigKey(args[i].val); e != nil {
					return e
				}
			}
		case strings.HasPrefix(v, "--config-env="):
			if e := gitConfigKey(v[len("--config-env="):]); e != nil {
				return e
			}
		case strings.HasPrefix(v, "-"):
		default:
			sub = i
		}
	}
	if sub < 0 {
		if more {
			return undecidable("Git arguments come from run-time input")
		}
		return nil
	}
	cmd, rest := strings.ToLower(args[sub].val), args[sub+1:]
	switch cmd {
	case "push", "send-pack", "receive-pack", "notes":
		return denied("git %s; publication is only through the king's explicit rk publish", cmd)
	}
	for _, a := range rest {
		switch v := a.val; {
		case strings.HasPrefix(v, "--no-veri"):
			return denied("git %s --no-verify skips the managed hooks", cmd)
		case notesRef(v):
			return denied("Git argument %s refers to refs/notes", show(a))
		case cmd == "config":
			if e := gitConfigKey(v); e != nil {
				return e
			}
		}
	}
	switch cmd {
	case "commit":
		for i := 0; i < len(rest); i++ {
			v := rest[i].val
			if v == "--" {
				break
			}
			if strings.HasPrefix(v, "--") {
				if slices.Contains([]string{"--message", "--file", "--author", "--date", "--template", "--reuse-message", "--reedit-message", "--fixup", "--squash", "--cleanup", "--trailer", "--pathspec-from-file"}, v) {
					i++
				}
				continue
			}
			for j := 1; len(v) > 1 && v[0] == '-' && j < len(v); j++ {
				if v[j] == 'n' {
					return denied("git commit -n skips the managed hooks")
				}
				if strings.IndexByte("mFcCt", v[j]) >= 0 {
					if j == len(v)-1 {
						i++
					}
					break
				}
			}
		}
	case "rebase":
		for i, a := range rest {
			switch v := a.val; {
			case (v == "-x" || v == "--exec") && i+1 < len(rest):
				if e := p.text(rest[i+1:i+2], "git rebase --exec"); e != nil {
					return e
				}
			case strings.HasPrefix(v, "--exec="):
				if e := p.text([]shWord{{val: v[len("--exec="):]}}, "git rebase --exec"); e != nil {
					return e
				}
			case strings.HasPrefix(v, "-x") && len(v) > 2:
				if e := p.text([]shWord{{val: v[2:]}}, "git rebase --exec"); e != nil {
					return e
				}
			}
		}
	case "submodule":
		if i := slices.IndexFunc(rest, func(a shWord) bool { return a.val == "foreach" }); i >= 0 {
			cmds := rest[i+1:]
			for len(cmds) > 0 && strings.HasPrefix(cmds[0].val, "-") {
				cmds = cmds[1:]
			}
			return p.text(cmds, "git submodule foreach")
		}
	case "bisect":
		if len(rest) > 0 && rest[0].val == "run" {
			return p.run(c, rest[1:], false)
		}
	}
	return nil
}
