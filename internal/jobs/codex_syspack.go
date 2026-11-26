package jobs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

// The one system configuration a codex job tolerates in /etc/codex: the host owner's managed-hooks
// pack, `requirements.toml` (only a [hooks] table) next to the `hooks` directory its commands live in.
const (
	codexPackRequirements = "requirements.toml"
	codexPackHooksDir     = "hooks"
	codexPackMaxBytes     = 64 << 10
)

// codexRootOwned reports whether a file belongs to root; a variable so tests can stand in.
var codexRootOwned = codexRootOwnedSys

func codexRootOwnedSys(fi fs.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && st.Uid == 0
}

// isCodexHooksPackListing reports whether the entry names of the system config dir are exactly the
// pack's two; only then is the pack check tried at all.
func isCodexHooksPackListing(names []string) bool {
	if len(names) != 2 {
		return false
	}
	return names[0] == codexPackHooksDir && names[1] == codexPackRequirements ||
		names[0] == codexPackRequirements && names[1] == codexPackHooksDir
}

// checkCodexHooksPack returns nil only when dir holds a recognised managed-hooks pack: dir, its
// requirements.toml and its hooks directory are what they should be (no symlink), root-owned and not
// group- or world-writable, and requirements.toml sets nothing but hooks that run commands from the
// hooks directory. The caller has already seen that dir holds exactly these two entries.
func checkCodexHooksPack(dir string) error {
	if err := checkCodexPackPath(dir, "", true); err != nil {
		return err
	}
	if err := checkCodexPackPath(dir, codexPackHooksDir, true); err != nil {
		return err
	}
	if err := checkCodexPackPath(dir, codexPackRequirements, false); err != nil {
		return err
	}
	// O_NOFOLLOW and a second look at the open file: what is parsed is the file that was checked.
	f, err := os.OpenFile(filepath.Join(dir, codexPackRequirements), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("%s: %w", codexPackRequirements, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("%s: %w", codexPackRequirements, err)
	}
	if err := checkCodexPackInfo(fi, codexPackRequirements, false); err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(f, codexPackMaxBytes+1))
	if err != nil {
		return fmt.Errorf("%s: %w", codexPackRequirements, err)
	}
	if len(data) > codexPackMaxBytes {
		return fmt.Errorf("%s: larger than %d bytes", codexPackRequirements, codexPackMaxBytes)
	}
	doc, err := parseStrictTOML(string(data))
	if err != nil {
		return fmt.Errorf("%s: %w", codexPackRequirements, err)
	}
	if err := validateCodexHooksRequirements(doc, filepath.Join(dir, codexPackHooksDir)); err != nil {
		return fmt.Errorf("%s: %w", codexPackRequirements, err)
	}
	return nil
}

// checkCodexPackPath lstats dir/rel (dir itself for an empty rel) and checks its type, mode and owner.
func checkCodexPackPath(dir, rel string, wantDir bool) error {
	name := rel
	if name == "" {
		name = dir
	}
	fi, err := os.Lstat(filepath.Join(dir, rel))
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return checkCodexPackInfo(fi, name, wantDir)
}

func checkCodexPackInfo(fi fs.FileInfo, name string, wantDir bool) error {
	switch {
	case wantDir && !fi.IsDir():
		return fmt.Errorf("%s: not a directory", name)
	case !wantDir && !fi.Mode().IsRegular():
		return fmt.Errorf("%s: not a regular file", name)
	case fi.Mode().Perm()&0o022 != 0:
		return fmt.Errorf("%s: group or world writable", name)
	case !codexRootOwned(fi):
		return fmt.Errorf("%s: not owned by root", name)
	}
	return nil
}

var (
	codexHookEventRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*$`)
	// A program directly in the hooks directory, then plain word arguments: no other path, no shell syntax.
	codexHookCommandTailRe = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]*( [A-Za-z0-9_-]+)*$`)
)

// validateCodexHooksRequirements accepts a parsed requirements.toml only when it is a managed-hooks
// declaration and nothing else: one top-level key `hooks`, in it `managed_dir` (the pack's hooks
// directory) and event arrays, every handler a command from that directory. Any other key anywhere
// (features, apps, mcp_servers, sandbox, approval, model, allowed_*, a matcher, ...) refuses.
func validateCodexHooksRequirements(doc map[string]any, hooksDir string) error {
	for k := range doc {
		if k != "hooks" {
			return fmt.Errorf("top-level key %q is not allowed (only hooks)", Printable(k, 64))
		}
	}
	hooks, ok := doc["hooks"].(map[string]any)
	if !ok {
		return errors.New("no [hooks] table")
	}
	if md, ok := hooks["managed_dir"].(string); !ok || md != hooksDir {
		return fmt.Errorf("hooks.managed_dir must be %q", hooksDir)
	}
	for event, v := range hooks {
		if event == "managed_dir" {
			continue
		}
		at := "hooks." + Printable(event, 64)
		if !codexHookEventRe.MatchString(event) {
			return fmt.Errorf("%s: not an event array", at)
		}
		groups, ok := tomlTables(v)
		if !ok {
			return fmt.Errorf("%s: not an array of tables", at)
		}
		for _, g := range groups {
			inner, isGroup := g["hooks"]
			if !isGroup {
				// A handler written directly in the event array.
				if err := validateCodexHookHandler(g, hooksDir); err != nil {
					return fmt.Errorf("%s: %w", at, err)
				}
				continue
			}
			for k := range g {
				if k != "hooks" {
					return fmt.Errorf("%s: key %q is not allowed", at, Printable(k, 64))
				}
			}
			handlers, ok := tomlTables(inner)
			if !ok {
				return fmt.Errorf("%s.hooks: not an array of tables", at)
			}
			for _, h := range handlers {
				if err := validateCodexHookHandler(h, hooksDir); err != nil {
					return fmt.Errorf("%s.hooks: %w", at, err)
				}
			}
		}
	}
	return nil
}

func validateCodexHookHandler(h map[string]any, hooksDir string) error {
	for k, v := range h {
		switch k {
		case "type", "command", "onFailure", "statusMessage":
			if _, ok := v.(string); !ok {
				return fmt.Errorf("handler %s is not a string", k)
			}
		case "timeout":
			if n, ok := v.(int64); !ok || n <= 0 {
				return errors.New("handler timeout is not a positive integer")
			}
		default:
			return fmt.Errorf("handler key %q is not allowed", Printable(k, 64))
		}
	}
	if typ, ok := h["type"].(string); !ok {
		return errors.New("handler without a type")
	} else if typ != "command" {
		return fmt.Errorf("handler type %q is not allowed (only command)", Printable(typ, 64))
	}
	cmd, _ := h["command"].(string)
	if !codexHookCommandInDir(cmd, hooksDir) {
		return fmt.Errorf("handler command %q is not a program in %s with plain arguments", Printable(cmd, 128), hooksDir)
	}
	return nil
}

// codexHookCommandInDir reports whether a hook's shell command is `<hooksDir>/<program> [word...]`,
// the path bare or in single quotes.
func codexHookCommandInDir(cmd, hooksDir string) bool {
	prefix := hooksDir + "/"
	if strings.HasPrefix(cmd, "'") {
		rest, ok := strings.CutPrefix(cmd, "'"+prefix)
		if !ok {
			return false
		}
		prog, args, ok := strings.Cut(rest, "'")
		if !ok || args != "" && !strings.HasPrefix(args, " ") {
			return false
		}
		return codexHookCommandTailRe.MatchString(prog + args)
	}
	rest, ok := strings.CutPrefix(cmd, prefix)
	return ok && codexHookCommandTailRe.MatchString(rest)
}

// tomlTables returns v as a list of tables when it is an array of them, from `[[a.b]]` headers or
// inline.
func tomlTables(v any) ([]map[string]any, bool) {
	switch a := v.(type) {
	case *tomlTableArray:
		return a.items, true
	case []any:
		out := make([]map[string]any, len(a))
		for i, e := range a {
			t, ok := e.(map[string]any)
			if !ok {
				return nil, false
			}
			out[i] = t
		}
		return out, true
	}
	return nil, false
}

// tomlTableArray is an array of tables built by `[[a.b]]` headers (an inline array is a []any).
type tomlTableArray struct{ items []map[string]any }

// parseStrictTOML parses the small TOML subset a managed-hooks requirements.toml needs and refuses
// everything else, so that nothing it does not understand can pass as harmless: bare keys (no dotted or
// quoted ones), `[a.b]` and `[[a.b]]` headers, values that are a basic string without escapes, a literal
// string, a decimal integer, a boolean, an array or an inline table; comments. Duplicate keys and tables refuse.
// Values are string, int64, bool, []any, map[string]any and *tomlTableArray. The repository has no TOML
// library, and a guard this narrow is safer with a parser that knows less than codex's.
func parseStrictTOML(src string) (map[string]any, error) {
	p := &strictTOML{src: src, line: 1}
	root := map[string]any{}
	cur := root
	for {
		p.skipSpace(true)
		if p.eof() {
			return root, nil
		}
		var err error
		if p.peek() == '[' {
			cur, err = p.header(root)
		} else {
			err = p.keyValue(cur)
		}
		if err == nil {
			err = p.endOfLine()
		}
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", p.line, err)
		}
	}
}

type strictTOML struct {
	src  string
	pos  int
	line int
}

func (p *strictTOML) eof() bool { return p.pos >= len(p.src) }

func (p *strictTOML) peek() byte {
	if p.eof() {
		return 0
	}
	return p.src[p.pos]
}

func (p *strictTOML) eat(c byte) bool {
	if p.peek() == c && !p.eof() {
		p.pos++
		return true
	}
	return false
}

// skipSpace skips blanks and, with newlines, line ends and comments too.
func (p *strictTOML) skipSpace(newlines bool) {
	for !p.eof() {
		switch c := p.src[p.pos]; {
		case c == ' ' || c == '\t':
			p.pos++
		case newlines && c == '\n':
			p.pos++
			p.line++
		case newlines && c == '\r' && strings.HasPrefix(p.src[p.pos:], "\r\n"):
			p.pos++
		case newlines && c == '#':
			p.skipComment()
		default:
			return
		}
	}
}

func (p *strictTOML) skipComment() {
	for !p.eof() && p.src[p.pos] != '\n' {
		p.pos++
	}
}

// endOfLine requires nothing but blanks and a comment up to the line end.
func (p *strictTOML) endOfLine() error {
	p.skipSpace(false)
	if p.peek() == '#' {
		p.skipComment()
	}
	if p.eof() || p.eat('\n') {
		p.line++
		return nil
	}
	if strings.HasPrefix(p.src[p.pos:], "\r\n") {
		p.pos += 2
		p.line++
		return nil
	}
	return errors.New("unexpected text after a value or header")
}

func (p *strictTOML) bareKey() (string, error) {
	start := p.pos
	for !p.eof() {
		c := p.src[p.pos]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' {
			p.pos++
			continue
		}
		break
	}
	if p.pos == start {
		return "", errors.New("expected a bare key")
	}
	return p.src[start:p.pos], nil
}

// header reads `[a.b]` or `[[a.b]]` and returns the table the following keys go into.
func (p *strictTOML) header(root map[string]any) (map[string]any, error) {
	p.pos++ // '['
	array := p.eat('[')
	var path []string
	for {
		p.skipSpace(false)
		k, err := p.bareKey()
		if err != nil {
			return nil, err
		}
		path = append(path, k)
		p.skipSpace(false)
		if !p.eat('.') {
			break
		}
	}
	if !p.eat(']') || array && !p.eat(']') {
		return nil, errors.New("malformed table header")
	}
	t := root
	for _, k := range path[:len(path)-1] {
		switch v := t[k].(type) {
		case nil:
			child := map[string]any{}
			t[k] = child
			t = child
		case map[string]any:
			t = v
		case *tomlTableArray:
			t = v.items[len(v.items)-1]
		default:
			return nil, fmt.Errorf("key %q is not a table", k)
		}
	}
	last := path[len(path)-1]
	fresh := map[string]any{}
	if !array {
		if _, dup := t[last]; dup {
			return nil, fmt.Errorf("duplicate table %q", strings.Join(path, "."))
		}
		t[last] = fresh
		return fresh, nil
	}
	switch v := t[last].(type) {
	case nil:
		t[last] = &tomlTableArray{items: []map[string]any{fresh}}
	case *tomlTableArray:
		v.items = append(v.items, fresh)
	default:
		return nil, fmt.Errorf("duplicate key %q", strings.Join(path, "."))
	}
	return fresh, nil
}

func (p *strictTOML) keyValue(t map[string]any) error {
	k, err := p.bareKey()
	if err != nil {
		return err
	}
	p.skipSpace(false)
	if !p.eat('=') {
		return fmt.Errorf("expected = after key %q (dotted and quoted keys are not supported)", k)
	}
	p.skipSpace(false)
	v, err := p.value(0)
	if err != nil {
		return err
	}
	if _, dup := t[k]; dup {
		return fmt.Errorf("duplicate key %q", k)
	}
	t[k] = v
	return nil
}

func (p *strictTOML) value(depth int) (any, error) {
	if depth > 4 {
		return nil, errors.New("value nested too deep")
	}
	switch c := p.peek(); {
	case c == '"' || c == '\'':
		return p.str(c)
	case c >= '0' && c <= '9':
		start := p.pos
		for !p.eof() && p.src[p.pos] >= '0' && p.src[p.pos] <= '9' {
			p.pos++
		}
		n, err := strconv.ParseInt(p.src[start:p.pos], 10, 64)
		if err != nil || p.pos-start > 1 && p.src[start] == '0' {
			return nil, errors.New("malformed integer")
		}
		return n, nil
	case strings.HasPrefix(p.src[p.pos:], "true"):
		p.pos += 4
		return true, nil
	case strings.HasPrefix(p.src[p.pos:], "false"):
		p.pos += 5
		return false, nil
	case c == '[':
		p.pos++
		arr := []any{}
		for {
			p.skipSpace(true)
			if p.eat(']') {
				return arr, nil
			}
			v, err := p.value(depth + 1)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
			p.skipSpace(true)
			if !p.eat(',') {
				if p.eat(']') {
					return arr, nil
				}
				return nil, errors.New("malformed array")
			}
		}
	case c == '{':
		p.pos++
		t := map[string]any{}
		p.skipSpace(false)
		if p.eat('}') {
			return t, nil
		}
		for {
			p.skipSpace(false)
			if err := p.keyValueInline(t, depth); err != nil {
				return nil, err
			}
			p.skipSpace(false)
			if p.eat('}') {
				return t, nil
			}
			if !p.eat(',') {
				return nil, errors.New("malformed inline table")
			}
		}
	}
	return nil, errors.New("unsupported value (only strings, integers, booleans, arrays and inline tables)")
}

func (p *strictTOML) keyValueInline(t map[string]any, depth int) error {
	k, err := p.bareKey()
	if err != nil {
		return err
	}
	p.skipSpace(false)
	if !p.eat('=') {
		return fmt.Errorf("expected = after key %q", k)
	}
	p.skipSpace(false)
	v, err := p.value(depth + 1)
	if err != nil {
		return err
	}
	if _, dup := t[k]; dup {
		return fmt.Errorf("duplicate key %q", k)
	}
	t[k] = v
	return nil
}

// str reads a one-line string; a basic one with a backslash is refused rather than unescaped, and a
// multi-line one fails on the text after its first empty string.
func (p *strictTOML) str(quote byte) (string, error) {
	p.pos++
	start := p.pos
	for !p.eof() {
		c := p.src[p.pos]
		switch {
		case c == quote:
			p.pos++
			return p.src[start : p.pos-1], nil
		case c == '\\' && quote == '"':
			return "", errors.New("string escapes are not supported")
		case c < 0x20 && c != '\t' || c == 0x7f:
			return "", errors.New("unterminated string")
		}
		p.pos++
	}
	return "", errors.New("unterminated string")
}
