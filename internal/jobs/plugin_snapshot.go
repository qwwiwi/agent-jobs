package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// A job's plugin view is a snapshot: a copy the daemon makes in the job dir (which the job cannot
// write, guard --protect) of the plugin's regular files and directories, read without following a
// symlink anywhere below the resolved plugin root. The job never gets a path into the live plugin,
// so neither a link to a file outside nor a change to the plugin after the copy reaches what claude
// loads. Without allow_hooks the copy is filtered: no hooks, MCP or LSP files, a manifest reduced to
// known keys, and no markdown file whose front matter is not plain YAML with allowed keys only.

const (
	// maxViewDepth bounds the directory nesting of a snapshot, maxViewEntries the entries it visits,
	// maxViewFileBytes one file and maxViewBytes the whole copy. A plugin over a bound is not loaded.
	maxViewDepth     = 32
	maxViewEntries   = 100000
	maxViewFileBytes = 32 << 20
	maxViewBytes     = 256 << 20
)

// pluginManifestKeys are the plugin.json keys a filtered snapshot keeps: metadata and the component
// paths that hold markdown only. Any other key (hooks and servers, and whatever a later claude adds)
// is left out.
var pluginManifestKeys = []string{"$schema", "name", "version", "description", "author", "homepage",
	"repository", "license", "keywords", "commands", "agents", "skills", "outputStyles"}

// pluginManifestPaths are the manifest keys that name component paths ("./dir" or a list of them).
var pluginManifestPaths = []string{"commands", "agents", "skills", "outputStyles"}

// frontMatterKeys are the root keys a markdown file of a filtered snapshot may set in its front
// matter: what skills, commands, agents and the GitHub templates of superpowers 7.0.2 use, and the
// documented keys that start nothing (no hooks, servers or permission modes).
var frontMatterKeys = []string{"name", "description", "license", "allowed-tools", "version", "metadata",
	"argument-hint", "disable-model-invocation", "user-invocable", "model", "tools", "color",
	"about", "title", "labels", "assignees"}

var (
	// fmKeyLineRe is a root-level "key: value" (or "key:") line of front matter.
	fmKeyLineRe = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9_-]*):(?: (.*))?$`)
	// fmBlockScalarRe is a block scalar header: | or > with optional chomping and indent indicators.
	fmBlockScalarRe = regexp.MustCompile(`^[|>](?:[1-9][+-]?|[+-][1-9]?)?(?: +#.*)?$`)
	// fmQuotedRe is a single-line double- or single-quoted scalar, with an optional comment.
	fmQuotedRe = regexp.MustCompile(`^(?:"(?:[^"\\]|\\.)*"|'(?:[^']|'')*')(?: +#.*)?$`)
)

// fmPlainStart are the characters a plain scalar value may not start with: YAML indicators (flow
// collections, anchors, aliases, tags, block scalars, quotes, directives, reserved, sequence entries,
// explicit keys).
const fmPlainStart = "-?:,[]{}#&*!|>'\"%@`"

// frontMatterProblem is why a markdown file with this content must stay out of a filtered snapshot,
// "" when it may stay. A file that does not start with front matter is fine. Front matter is accepted
// only when it opens with a bare "---" line at the very start of the file (no BOM, no leading blank,
// LF line ends), closes with a bare "---" line, and each line in between is blank, a root-level
// comment, a root-level "key: value" with a key from frontMatterKeys (each once; the value a plain or
// single-line quoted scalar, a block scalar header, or empty) or, after a block scalar header or an
// empty value, an indented line at least as deep as the first one of that value. Whatever else could
// be read as YAML (indented root keys, quoted or explicit keys, flow collections, anchors, aliases,
// tags, merges, tabs, CR) keeps the file out: a root key this check does not see cannot exist.
func frontMatterProblem(b []byte) string {
	s := string(b)
	if !strings.HasPrefix(strings.TrimLeft(strings.TrimPrefix(s, "\xef\xbb\xbf"), " \t\r\n\f\v"), "---") {
		return ""
	}
	if !strings.HasPrefix(s, "---\n") {
		return `front matter does not open with a bare "---" line at the start of the file`
	}
	lines := strings.Split(s[len("---\n"):], "\n")
	seen := map[string]bool{}
	nested, depth := false, 0 // inside an allowed key's block value; the indent of its first line
	for i, line := range lines {
		n := i + 2 // the line number in the file
		if i == len(lines)-1 {
			break // no newline after it: not a closing line
		}
		if line == "---" {
			return ""
		}
		if !utf8.ValidString(line) || strings.IndexFunc(line, fmOddRune) >= 0 {
			return fmt.Sprintf("front matter line %d has a tab, CR, control character, BOM, Unicode line break or invalid UTF-8", n)
		}
		if strings.TrimLeft(line, " ") == "" {
			continue
		}
		if line[0] == ' ' {
			if !nested {
				return fmt.Sprintf("front matter line %d is indented outside a block value of an allowed key", n)
			}
			ind := len(line) - len(strings.TrimLeft(line, " "))
			if depth == 0 {
				depth = ind
			} else if ind < depth {
				return fmt.Sprintf("front matter line %d is less indented than the value it continues", n)
			}
			continue
		}
		nested, depth = false, 0
		if line[0] == '#' {
			continue
		}
		m := fmKeyLineRe.FindStringSubmatch(line)
		if m == nil {
			return fmt.Sprintf("front matter line %d %q is not a plain root-level key", n, truncate(line, 40))
		}
		key := m[1]
		if !slices.Contains(frontMatterKeys, key) {
			return fmt.Sprintf("front matter key %q is not allowed", truncate(key, 40))
		}
		if seen[key] {
			return fmt.Sprintf("front matter key %q is set twice", key)
		}
		seen[key] = true
		v := strings.Trim(m[2], " ")
		switch {
		case v == "" || strings.HasPrefix(v, "#") || fmBlockScalarRe.MatchString(v):
			nested = true
		case strings.ContainsRune(`"'`, rune(v[0])):
			if !fmQuotedRe.MatchString(v) {
				return fmt.Sprintf("front matter key %q has a quoted value that does not close on its line", key)
			}
		case strings.ContainsRune(fmPlainStart, rune(v[0])):
			return fmt.Sprintf("front matter key %q has a value that is not a plain or quoted scalar", key)
		}
	}
	return "front matter has no closing line"
}

// fmOddRune is a rune front matter may not hold: a control character (tab, CR, NUL ...), DEL, a BOM,
// or what YAML 1.1 reads as a line break (NEL, LS, PS), where parsers could split lines differently.
func fmOddRune(r rune) bool {
	return r < ' ' || r == 0x7f || r == 0x85 || r == 0x2028 || r == 0x2029 || r == 0xfeff
}

// pluginSnapshot copies a plugin tree into a job's view.
type pluginSnapshot struct {
	filter  bool // without allow_hooks
	warns   []string
	entries int
	bytes   int64
}

// buildPluginView makes viewDir a snapshot of the plugin ps (see the top of this file); filter is
// for a plugin without allow_hooks. It returns one warning per entry left out.
func buildPluginView(ps *pluginSource, viewDir string, filter bool) ([]string, error) {
	var manifest []byte
	var warns []string
	if filter && ps.manifest != nil {
		var err error
		if manifest, warns, err = sanitizeManifest(ps.manifest); err != nil {
			return nil, err
		}
	}
	if err := os.RemoveAll(viewDir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(viewDir), 0o700); err != nil {
		return nil, err
	}
	// ps.real has its symlinks resolved; below it nothing is followed.
	root, err := openAbsoluteSource(ps.real)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	s := &pluginSnapshot{filter: filter, warns: warns}
	if err := s.copyDir(root, viewDir, "", 0); err != nil {
		return nil, err
	}
	if manifest != nil {
		dir := filepath.Join(viewDir, ".claude-plugin")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		if err := writeSnapshotFile(filepath.Join(dir, "plugin.json"), manifest, 0o400); err != nil {
			return nil, err
		}
	}
	return s.warns, nil
}

// leaveOut records an entry left out of the snapshot.
func (s *pluginSnapshot) leaveOut(rel, why string) {
	s.warns = append(s.warns, fmt.Sprintf("%s left out of the view: %s", rel, why))
}

// copyDir copies the directory open as dir to the new directory dst; rel is its path in the plugin.
func (s *pluginSnapshot) copyDir(dir *os.File, dst, rel string, depth int) error {
	if depth > maxViewDepth {
		return fmt.Errorf("%s: nested deeper than %d directories", rel, maxViewDepth)
	}
	fi, err := dir.Stat()
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s: not a directory", rel)
	}
	if err := os.Mkdir(dst, 0o700); err != nil {
		return err
	}
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return fmt.Errorf("%s: %w", rel, err)
	}
	slices.Sort(names)
	dfd := int(dir.Fd())
	for _, name := range names {
		r := filepath.Join(rel, name)
		if s.entries++; s.entries > maxViewEntries {
			return fmt.Errorf("more than %d entries", maxViewEntries)
		}
		if s.filter && depth == 0 && slices.Contains(pluginExecEntries, name) {
			continue
		}
		if s.filter && r == filepath.Join(".claude-plugin", "plugin.json") {
			continue // replaced by the reduced manifest
		}
		if err := s.copyEntry(dfd, name, filepath.Join(dst, name), r, depth); err != nil {
			return err
		}
	}
	return nil
}

// copyEntry copies the entry name of the directory dfd: a directory or a regular file, opened without
// following a symlink and checked to be the entry that was looked at. Anything else is left out.
func (s *pluginSnapshot) copyEntry(dfd int, name, dst, rel string, depth int) error {
	var st unix.Stat_t
	if err := unix.Fstatat(dfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("%s: %w", rel, err)
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFDIR, unix.S_IFREG:
	case unix.S_IFLNK:
		s.leaveOut(rel, "a symbolic link (a view copies regular files only)")
		return nil
	default:
		s.leaveOut(rel, "not a regular file or directory")
		return nil
	}
	fd, err := unix.Openat(dfd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ELOOP) {
		s.leaveOut(rel, "a symbolic link (a view copies regular files only)")
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s: %w", rel, err)
	}
	f := os.NewFile(uintptr(fd), rel)
	defer f.Close()
	var now unix.Stat_t
	if err := unix.Fstat(fd, &now); err != nil {
		return fmt.Errorf("%s: %w", rel, err)
	}
	if now.Dev != st.Dev || now.Ino != st.Ino || now.Mode&unix.S_IFMT != st.Mode&unix.S_IFMT {
		return fmt.Errorf("%s: changed while the view was built", rel)
	}
	if now.Mode&unix.S_IFMT == unix.S_IFDIR {
		return s.copyDir(f, dst, rel, depth+1)
	}
	if now.Size > maxViewFileBytes {
		return fmt.Errorf("%s: exceeds %d bytes", rel, maxViewFileBytes)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxViewFileBytes+1))
	if err != nil {
		return fmt.Errorf("%s: %w", rel, err)
	}
	if len(b) > maxViewFileBytes {
		return fmt.Errorf("%s: exceeds %d bytes", rel, maxViewFileBytes)
	}
	if s.bytes += int64(len(b)); s.bytes > maxViewBytes {
		return fmt.Errorf("the plugin exceeds %d bytes", maxViewBytes)
	}
	// The check reads the very bytes that are written: what was checked is what the job gets.
	if s.filter && strings.EqualFold(filepath.Ext(name), ".md") {
		if why := frontMatterProblem(b); why != "" {
			s.leaveOut(rel, why+" (allow_hooks is off)")
			return nil
		}
	}
	mode := os.FileMode(0o400)
	if now.Mode&0o111 != 0 {
		mode = 0o500
	}
	return writeSnapshotFile(dst, b, mode)
}

// writeSnapshotFile creates the new file p, read-only for its owner (the job cannot write it anyway:
// the view is protected).
func writeSnapshotFile(p string, b []byte, mode os.FileMode) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// sanitizeManifest is the plugin.json of a filtered snapshot: only pluginManifestKeys, and of the
// component paths only those that stay inside the plugin. A component path into .claude-plugin
// refuses the plugin (claude would load from a directory a view does not treat as components).
func sanitizeManifest(man map[string]json.RawMessage) ([]byte, []string, error) {
	clean := map[string]json.RawMessage{}
	var warns []string
	for _, k := range slices.Sorted(maps.Keys(man)) {
		v := man[k]
		switch {
		case slices.Contains(pluginExecKeys, k):
			continue
		case !slices.Contains(pluginManifestKeys, k):
			warns = append(warns, fmt.Sprintf("manifest key %q left out of the view (not a known component key)", truncate(k, 40)))
			continue
		case slices.Contains(pluginManifestPaths, k):
			paths, ok := manifestPathList(v)
			if !ok {
				warns = append(warns, fmt.Sprintf("manifest key %s left out of the view: not a path or a list of paths", k))
				continue
			}
			leaves := false
			for _, p := range paths {
				inside, intoManifest := manifestPathInside(p)
				if intoManifest {
					return nil, nil, fmt.Errorf("manifest key %s points into .claude-plugin (%q)", k, truncate(p, 80))
				}
				leaves = leaves || !inside
			}
			if leaves {
				warns = append(warns, fmt.Sprintf("manifest key %s left out of the view: a path leaves the plugin", k))
				continue
			}
		}
		clean[k] = v
	}
	b, err := json.MarshalIndent(clean, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	return append(b, '\n'), warns, nil
}

// manifestPathList reads a component path value: a string or a list of strings.
func manifestPathList(v json.RawMessage) ([]string, bool) {
	var one string
	if json.Unmarshal(v, &one) == nil {
		return []string{one}, true
	}
	var list []string
	if json.Unmarshal(v, &list) == nil && len(list) > 0 {
		return list, true
	}
	return nil, false
}

// manifestPathInside reports whether the component path p stays inside the plugin root, and whether
// it leads into the root's .claude-plugin directory.
func manifestPathInside(p string) (inside, intoManifest bool) {
	if p == "" || filepath.IsAbs(p) || strings.ContainsAny(p, "\x00\\") {
		return false, false
	}
	c := filepath.ToSlash(filepath.Clean(p))
	if c == ".." || strings.HasPrefix(c, "../") {
		return false, false
	}
	first := strings.SplitN(c, "/", 2)[0]
	return true, strings.EqualFold(first, ".claude-plugin")
}
