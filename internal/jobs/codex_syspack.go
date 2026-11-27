package jobs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"
)

// The one system configuration a codex job tolerates in /etc/codex: the host owner's managed-hooks
// pack, `requirements.toml` (only a [hooks] table) next to the `hooks` directory its commands live in.
const (
	codexPackRequirements = "requirements.toml"
	codexPackHooksDir     = "hooks"
	codexPackMaxBytes     = 64 << 10
	// Bounds on the walk of the hooks directory; a pack beyond them refuses.
	codexPackMaxEntries = 20000
	codexPackMaxDepth   = 16
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
// group- or world-writable; so is every entry under the hooks directory, at any depth, each a regular
// file or a directory, so that nothing a hook runs or loads resolves outside the pack or can be
// replaced by anyone but root; requirements.toml sets nothing but hooks that run commands from the
// hooks directory; and every program those commands name is a regular file directly in it. The caller
// has already seen that dir holds exactly these two entries.
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
	programs, err := checkCodexHooksRequirements(string(data), filepath.Join(dir, codexPackHooksDir))
	if err != nil {
		return fmt.Errorf("%s: %w", codexPackRequirements, err)
	}
	seen := 0
	if err := checkCodexPackTree(dir, codexPackHooksDir, 1, &seen); err != nil {
		return err
	}
	for _, prog := range programs {
		rel := codexPackHooksDir + "/" + prog
		// The tree walk has checked mode and owner; a program must also be there and be a file.
		if fi, err := os.Lstat(filepath.Join(dir, rel)); err != nil || !fi.Mode().IsRegular() {
			return fmt.Errorf("%s: handler program is not a regular file in the hooks directory", rel)
		}
	}
	return nil
}

// checkCodexPackTree checks every entry under dir/rel, a directory that has passed itself: a regular
// file or a directory (no symlink, device, socket or fifo), root-owned, not group- or world-writable.
func checkCodexPackTree(dir, rel string, depth int, seen *int) error {
	if depth > codexPackMaxDepth {
		return fmt.Errorf("%s: nested deeper than %d directories", rel, codexPackMaxDepth)
	}
	entries, err := os.ReadDir(filepath.Join(dir, rel))
	if err != nil {
		return fmt.Errorf("%s: %w", rel, err)
	}
	for _, e := range entries {
		if *seen++; *seen > codexPackMaxEntries {
			return fmt.Errorf("%s: more than %d entries", codexPackHooksDir, codexPackMaxEntries)
		}
		sub := rel + "/" + e.Name()
		name := Printable(sub, 256)
		fi, err := os.Lstat(filepath.Join(dir, sub))
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if !fi.IsDir() && !fi.Mode().IsRegular() {
			return fmt.Errorf("%s: not a regular file or directory", name)
		}
		if err := checkCodexPackInfo(fi, name, fi.IsDir()); err != nil {
			return err
		}
		if fi.IsDir() {
			if err := checkCodexPackTree(dir, sub, depth+1, seen); err != nil {
				return err
			}
		}
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

// codexHookEvents are the hook events a pack may list. Codex drops a handler under a name it does not
// know without a word, so an unknown name refuses: a pack that looks armed must be armed.
var codexHookEvents = map[string]bool{
	"SessionStart": true, "UserPromptSubmit": true, "PreToolUse": true, "PostToolUse": true,
	"Stop": true, "PreCompact": true,
}

// codexHookLegacyFailureEvent is the one event whose handlers may still carry `onFailure`: not a
// field codex knows (it ignores the key and loads the file), left over in the host's pack.
const codexHookLegacyFailureEvent = "PreToolUse"

var (
	// A program directly in the hooks directory: one path element, no other path.
	codexHookProgramRe = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]*$`)
	// Plain word arguments: no shell syntax.
	codexHookArgsRe = regexp.MustCompile(`^( [A-Za-z0-9_-]+)*$`)
)

// checkCodexHooksRequirements accepts the text of a requirements.toml only when it is a managed-hooks
// declaration for hooksDir and nothing else, and returns the names of the programs in hooksDir its
// handlers run (sorted, each once).
func checkCodexHooksRequirements(src, hooksDir string) ([]string, error) {
	doc, err := parseStrictTOML(src)
	if err != nil {
		return nil, err
	}
	return validateCodexHooksRequirements(doc, hooksDir)
}

// validateCodexHooksRequirements accepts a parsed requirements.toml only when it is a managed-hooks
// declaration and nothing else: one top-level key `hooks`, in it `managed_dir` (the pack's hooks
// directory) and arrays for known events in the nested form only, `[[hooks.<Event>]]` groups that
// hold nothing but `[[hooks.<Event>.hooks]]` handlers, at least one per group, every handler a command
// from that directory. Any other key anywhere (features, apps, mcp_servers, sandbox, approval, model,
// allowed_*, a matcher, ...) and any other shape (a handler written directly in the event array, an
// event that is a plain table) refuses: a shape codex reads differently could lose a handler silently.
func validateCodexHooksRequirements(doc map[string]any, hooksDir string) ([]string, error) {
	for k := range doc {
		if k != "hooks" {
			return nil, fmt.Errorf("top-level key %q is not allowed (only hooks)", Printable(k, 64))
		}
	}
	hooks, ok := doc["hooks"].(map[string]any)
	if !ok {
		return nil, errors.New("no [hooks] table")
	}
	if md, ok := hooks["managed_dir"].(string); !ok || md != hooksDir {
		return nil, fmt.Errorf("hooks.managed_dir must be %q", hooksDir)
	}
	programs := map[string]bool{}
	for event, v := range hooks {
		if event == "managed_dir" {
			continue
		}
		at := "hooks." + Printable(event, 64)
		if !codexHookEvents[event] {
			return nil, fmt.Errorf("%s: not a known hook event", at)
		}
		groups, ok := v.(*tomlTableArray)
		if !ok {
			return nil, fmt.Errorf("%s: not an array of tables", at)
		}
		for _, g := range groups.items {
			for k := range g {
				if k != "hooks" {
					return nil, fmt.Errorf("%s: key %q is not allowed (only [[%s.hooks]] handlers)", at, Printable(k, 64), at)
				}
			}
			inner, has := g["hooks"]
			if !has {
				return nil, fmt.Errorf("%s: a group without handlers", at)
			}
			// An array built by headers always has an item.
			handlers, ok := inner.(*tomlTableArray)
			if !ok {
				return nil, fmt.Errorf("%s.hooks: not an array of tables", at)
			}
			for _, h := range handlers.items {
				prog, err := validateCodexHookHandler(h, hooksDir, event)
				if err != nil {
					return nil, fmt.Errorf("%s.hooks: %w", at, err)
				}
				programs[prog] = true
			}
		}
	}
	names := make([]string, 0, len(programs))
	for p := range programs {
		names = append(names, p)
	}
	sort.Strings(names)
	return names, nil
}

// validateCodexHookHandler checks one handler table and returns the name of the program it runs.
func validateCodexHookHandler(h map[string]any, hooksDir, event string) (string, error) {
	for k, v := range h {
		switch {
		case k == "type" || k == "command" || k == "statusMessage" || k == "onFailure" && event == codexHookLegacyFailureEvent:
			if _, ok := v.(string); !ok {
				return "", fmt.Errorf("handler %s is not a string", k)
			}
		case k == "timeout":
			if n, ok := v.(int64); !ok || n <= 0 {
				return "", errors.New("handler timeout is not a positive integer")
			}
		case k == "additionalContextLimit" && event == "SessionStart":
			const maxAdditionalContextLimit = 100000
			if n, ok := v.(int64); !ok || n <= 0 || n > maxAdditionalContextLimit {
				return "", fmt.Errorf("handler additionalContextLimit must be an integer between 1 and %d", maxAdditionalContextLimit)
			}
		default:
			return "", fmt.Errorf("handler key %q is not allowed", Printable(k, 64))
		}
	}
	if typ, ok := h["type"].(string); !ok {
		return "", errors.New("handler without a type")
	} else if typ != "command" {
		return "", fmt.Errorf("handler type %q is not allowed (only command)", Printable(typ, 64))
	}
	cmd, _ := h["command"].(string)
	prog, ok := codexHookCommandProgram(cmd, hooksDir)
	if !ok {
		return "", fmt.Errorf("handler command %q is not a program in %s with plain arguments", Printable(cmd, 128), hooksDir)
	}
	return prog, nil
}

// codexHookCommandProgram splits a hook's shell command `<hooksDir>/<program> [word...]`, the path
// bare or in single quotes, and returns the program's name. The path and the arguments are checked
// apart: a quoted path ends at its quote, so `'<hooksDir>/run Stop'` is a program named "run Stop"
// and refuses.
func codexHookCommandProgram(cmd, hooksDir string) (string, bool) {
	var prog, args string
	if quoted, isQuoted := strings.CutPrefix(cmd, "'"); isQuoted {
		rest, ok := strings.CutPrefix(quoted, hooksDir+"/")
		if !ok {
			return "", false
		}
		if prog, args, ok = strings.Cut(rest, "'"); !ok {
			return "", false
		}
	} else {
		rest, ok := strings.CutPrefix(cmd, hooksDir+"/")
		if !ok {
			return "", false
		}
		prog = rest
		if i := strings.IndexByte(rest, ' '); i >= 0 {
			prog, args = rest[:i], rest[i:]
		}
	}
	if !codexHookProgramRe.MatchString(prog) || !codexHookArgsRe.MatchString(args) {
		return "", false
	}
	return prog, true
}

// tomlTableArray is an array of tables built by `[[a.b]]` headers.
type tomlTableArray struct{ items []map[string]any }

// parseStrictTOML parses the small TOML subset a managed-hooks requirements.toml needs and refuses
// everything else, so that nothing it does not understand can pass as harmless. The repository has no
// TOML library, and a guard this narrow is safer with a parser that knows less than codex's.
//
// Accepted: valid UTF-8 without control characters (tab, LF and CR LF aside), also in comments; bare
// keys; `[a.b]` and `[[a.b]]` headers; values that are a one-line basic string without escapes, a
// one-line literal string, a decimal integer or a boolean; comments.
//
// Refused, though TOML allows it: dotted and quoted keys (`hooks.managed_dir = ...`); arrays and
// inline tables, so no table or array of tables exists but the ones headers build, and none can be
// written inline and then extended by a header; a table header for a table that exists already, be it
// from its own header or implied by a child's (`[[hooks.Stop]]` and later `[hooks]`), so a parent
// comes before its children; floats, dates, signed or prefixed integers, multi-line strings. Duplicate
// keys and tables refuse as in TOML.
//
// Values are string, int64, bool, map[string]any and *tomlTableArray.
func parseStrictTOML(src string) (map[string]any, error) {
	if !utf8.ValidString(src) {
		return nil, errors.New("not valid UTF-8")
	}
	line := 1
	for i := 0; i < len(src); i++ {
		switch c := src[i]; {
		case c == '\n':
			line++
		case c == '\r':
			if i+1 >= len(src) || src[i+1] != '\n' {
				return nil, fmt.Errorf("line %d: carriage return without a line feed", line)
			}
		case c < 0x20 && c != '\t' || c == 0x7f:
			return nil, fmt.Errorf("line %d: control character 0x%02x", line, c)
		}
	}
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
	v, err := p.value()
	if err != nil {
		return err
	}
	if _, dup := t[k]; dup {
		return fmt.Errorf("duplicate key %q", k)
	}
	t[k] = v
	return nil
}

func (p *strictTOML) value() (any, error) {
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
	}
	return nil, errors.New("unsupported value (only one-line strings, integers and booleans; no arrays or inline tables)")
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
