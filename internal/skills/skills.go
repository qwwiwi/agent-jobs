// Package skills reads skill catalogs (E7): the roots named in config skill_roots, each a directory
// whose index.json lists skill directories (<root>/<path>/SKILL.md). It validates the entries,
// resolves duplicate names by root order, selects the skills of a job (defaults by kind and project,
// explicit +name / -name edits, count and size budgets) and searches the catalog by content. It
// ships no catalog and names none: the roots are whatever the owner configures.
package skills

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// Statuses of a catalog entry. Deprecated is never selected; draft only when named explicitly.
const (
	StatusCore       = "core"
	StatusActive     = "active"
	StatusDraft      = "draft"
	StatusDeprecated = "deprecated"
)

// Statuses lists the valid statuses.
var Statuses = []string{StatusCore, StatusActive, StatusDraft, StatusDeprecated}

// IndexFile is the catalog index at the top of each root.
const IndexFile = "index.json"

// SkillFile is the file every skill directory holds.
const SkillFile = "SKILL.md"

// Budgets of one job: defaults and ceilings of skills_max_per_job and skills_max_kb.
const (
	DefaultMaxPerJob = 6
	CeilingMaxPerJob = 8
	DefaultMaxKB     = 400
)

// maxIndexBytes bounds index.json; maxSkillReadBytes bounds what is read of a SKILL.md.
const (
	maxIndexBytes     = 8 << 20
	maxSkillReadBytes = 64 << 10
)

// reName is a skill name: also the directory name under .claude/skills, so never a path or a flag.
var reName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidName reports whether s can be a skill name.
func ValidName(s string) bool { return reName.MatchString(s) && !strings.Contains(s, "..") }

// rawEntry is one entry of index.json as written by the catalog's own tooling. Only the fields
// agent-jobs needs are typed; the informational ones (owner, origin, last_verified, has_scripts)
// are free-form: a value of another type is ignored, never an error. Other fields (place, sha256,
// size_kb, ...) are not read.
type rawEntry struct {
	Name         string          `json:"name"`
	Path         string          `json:"path"`
	Status       string          `json:"status"`
	Kinds        []string        `json:"kinds"`
	Projects     []string        `json:"projects"`
	Risk         []string        `json:"risk"`
	Owner        json.RawMessage `json:"owner"`
	Origin       json.RawMessage `json:"origin"`
	LastVerified json.RawMessage `json:"last_verified"`
	HasScripts   json.RawMessage `json:"has_scripts"`
}

// rawIndex keeps each entry raw, so one malformed entry is skipped on its own, not with its root.
type rawIndex struct {
	Version json.RawMessage   `json:"version"`
	Skills  []json.RawMessage `json:"skills"`
}

// parseEntry decodes one raw index entry; on error it still returns the entry's name when that is
// a string, for the warning.
func parseEntry(b json.RawMessage) (rawEntry, error) {
	var r rawEntry
	if err := json.Unmarshal(b, &r); err != nil {
		var n struct {
			Name json.RawMessage `json:"name"`
		}
		_ = json.Unmarshal(b, &n)
		var name string
		_ = json.Unmarshal(n.Name, &name)
		return rawEntry{Name: name}, err
	}
	return r, nil
}

// infoString returns an informational field when it is a JSON string, else "".
func infoString(b json.RawMessage) string {
	var s string
	if json.Unmarshal(b, &s) != nil {
		return ""
	}
	return s
}

// Entry is one valid skill of the catalog.
type Entry struct {
	Name         string   `json:"name"`
	Status       string   `json:"status"`
	Kinds        []string `json:"kinds"`
	Projects     []string `json:"projects"`
	Owner        string   `json:"owner,omitempty"`
	Origin       string   `json:"origin,omitempty"`
	LastVerified string   `json:"last_verified,omitempty"`
	HasScripts   bool     `json:"has_scripts,omitempty"`
	Risk         []string `json:"risk"`
	// SizeKB is measured on the skill directory (regular files), not taken from the index.
	SizeKB float64 `json:"size_kb"`
	// Description is the one-line description: the first sentence of the front matter description,
	// else the first paragraph of the body.
	Description string `json:"description"`
	// Root is the catalog root (symlinks resolved); Dir the skill directory (symlinks resolved,
	// inside Root). Never shown outside the daemon (no paths in tool or API output).
	Root string `json:"-"`
	Dir  string `json:"-"`
	// body is the start of SKILL.md after the front matter (search only).
	body string
	// descFull is the whole front matter description (search only).
	descFull string
}

// Catalog is the merged view of every configured root.
type Catalog struct {
	// Entries are the valid entries, sorted by name; a name appears once (the first root wins).
	Entries []Entry
	// Warnings name the roots and entries that were skipped and why (status, doctor).
	Warnings []string
	// Roots are the roots that loaded, symlinks resolved, in config order.
	Roots  []string
	byName map[string]int
}

// Lookup returns the entry of a name.
func (c *Catalog) Lookup(name string) (Entry, bool) {
	if c == nil {
		return Entry{}, false
	}
	i, ok := c.byName[name]
	if !ok {
		return Entry{}, false
	}
	return c.Entries[i], true
}

// Load reads index.json from every root, in order. A missing or invalid root, and an invalid entry,
// is a warning, never an error: the catalog is whatever loaded.
func Load(roots []string) *Catalog {
	c := &Catalog{byName: map[string]int{}}
	for _, root := range roots {
		c.loadRoot(root)
	}
	sort.Slice(c.Entries, func(a, b int) bool { return c.Entries[a].Name < c.Entries[b].Name })
	for i, e := range c.Entries {
		c.byName[e.Name] = i
	}
	return c
}

func (c *Catalog) warn(format string, args ...any) {
	c.Warnings = append(c.Warnings, fmt.Sprintf(format, args...))
}

func (c *Catalog) loadRoot(root string) {
	if !filepath.IsAbs(root) {
		c.warn("skill root %s: not an absolute path, skipped", root)
		return
	}
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		c.warn("skill root %s: %v, skipped", root, err)
		return
	}
	if fi, err := os.Stat(real); err != nil || !fi.IsDir() {
		c.warn("skill root %s: not a directory, skipped", root)
		return
	}
	idx, err := readIndex(filepath.Join(real, IndexFile))
	if err != nil {
		c.warn("skill root %s: %v, skipped", root, err)
		return
	}
	c.Roots = append(c.Roots, real)
	seen := map[string]bool{}
	for i, b := range idx.Skills {
		r, err := parseEntry(b)
		if err != nil {
			c.warn("skill root %s: %s: %v, skipped", root, entryLabel(i, r.Name), err)
			continue
		}
		e, err := validate(real, r)
		if err != nil {
			c.warn("skill root %s: %s: %v, skipped", root, entryLabel(i, r.Name), err)
			continue
		}
		if seen[e.Name] {
			c.warn("skill root %s: duplicate name %q, the first entry is kept", root, e.Name)
			continue
		}
		seen[e.Name] = true
		if slices.ContainsFunc(c.Entries, func(x Entry) bool { return x.Name == e.Name }) {
			continue // an earlier root has this name: root order decides
		}
		c.Entries = append(c.Entries, e)
	}
}

// entryLabel names entry i of an index in a warning: by its index, and by its name only when the
// name is valid (an invalid one may be any text the index holds).
func entryLabel(i int, name string) string {
	if ValidName(name) {
		return fmt.Sprintf("entry %d (%q)", i, name)
	}
	return fmt.Sprintf("entry %d", i)
}

func readIndex(path string) (rawIndex, error) {
	var idx rawIndex
	fi, err := os.Stat(path)
	if err != nil {
		return idx, fmt.Errorf("no %s: %w", IndexFile, err)
	}
	if !fi.Mode().IsRegular() {
		return idx, fmt.Errorf("%s is not a regular file", IndexFile)
	}
	if fi.Size() > maxIndexBytes {
		return idx, fmt.Errorf("%s is %d bytes, limit %d", IndexFile, fi.Size(), maxIndexBytes)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return idx, err
	}
	if err := json.Unmarshal(b, &idx); err != nil {
		return idx, fmt.Errorf("parse %s: %w", IndexFile, err)
	}
	return idx, nil
}

// within reports whether p lies strictly inside dir (both clean, absolute, symlinks resolved).
func within(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != "." && rel != ".." && !filepath.IsAbs(rel) &&
		!strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// validate turns an index entry into an Entry: a valid name and status, a relative path that stays
// inside the root after symlink resolution, a directory named like the skill holding a regular
// SKILL.md, and a tree whose symlinks (if any) name regular files inside the skill directory.
func validate(root string, r rawEntry) (Entry, error) {
	if !ValidName(r.Name) {
		return Entry{}, errors.New("name must be [A-Za-z0-9._-], at most 64, no ..")
	}
	if !slices.Contains(Statuses, r.Status) {
		return Entry{}, fmt.Errorf("status %q must be one of %s", r.Status, strings.Join(Statuses, "|"))
	}
	if r.Path == "" || filepath.IsAbs(r.Path) || strings.ContainsRune(r.Path, 0) {
		return Entry{}, fmt.Errorf("path %q must be relative to the root", r.Path)
	}
	dir, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(r.Path)))
	if err != nil {
		return Entry{}, fmt.Errorf("path %q: %w", r.Path, err)
	}
	if !within(dir, root) {
		return Entry{}, fmt.Errorf("path %q leaves the root", r.Path)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return Entry{}, fmt.Errorf("path %q is not a directory", r.Path)
	}
	if filepath.Base(dir) != r.Name {
		return Entry{}, fmt.Errorf("directory %q is not named %q", filepath.Base(dir), r.Name)
	}
	if fi, err := os.Lstat(filepath.Join(dir, SkillFile)); err != nil || !fi.Mode().IsRegular() {
		return Entry{}, fmt.Errorf("no regular %s in %q", SkillFile, r.Path)
	}
	size, err := Measure(dir)
	if err != nil {
		return Entry{}, err
	}
	e := Entry{
		Name: r.Name, Status: r.Status, Kinds: lower(r.Kinds), Projects: slices.Clone(r.Projects),
		Owner: infoString(r.Owner), Origin: infoString(r.Origin), LastVerified: infoString(r.LastVerified),
		Risk: slices.Clone(r.Risk), SizeKB: size, Root: root, Dir: dir,
	}
	_ = json.Unmarshal(r.HasScripts, &e.HasScripts) // free-form: anything but a bool leaves false
	if e.Kinds == nil {
		e.Kinds = []string{}
	}
	if e.Projects == nil {
		e.Projects = []string{}
	}
	if e.Risk == nil {
		e.Risk = []string{}
	}
	e.descFull, e.body = readSkill(filepath.Join(dir, SkillFile))
	e.Description = oneLine(e.descFull, e.body)
	return e, nil
}

func lower(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, strings.ToLower(strings.TrimSpace(s)))
	}
	return out
}

// Measure returns the size in KB of the files under dir (resolved). A job reads the skill through a
// link and may follow every symlink in it, so a symlink must name a regular file inside dir itself:
// one to a directory (which could lead anywhere, also through further links), outside dir (another
// skill, the rest of the catalog or beyond), dangling or looping is an error. The target of a file
// symlink counts towards the size like a regular file.
func Measure(dir string) (float64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch t := d.Type(); {
		case t.IsRegular():
			fi, err := d.Info()
			if err != nil {
				return err
			}
			total += fi.Size()
		case t&fs.ModeSymlink != 0:
			target, err := filepath.EvalSymlinks(p)
			if err != nil || !within(target, dir) {
				return fmt.Errorf("symlink %s leaves the skill directory (or does not resolve)", filepath.Base(p))
			}
			fi, err := os.Stat(target)
			if err != nil || !fi.Mode().IsRegular() {
				return fmt.Errorf("symlink %s does not point to a regular file", filepath.Base(p))
			}
			total += fi.Size()
		case t.IsDir():
		default:
			return fmt.Errorf("%s is not a regular file or directory", filepath.Base(p))
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return float64(total) / 1024, nil
}

// readSkill returns the front matter description and the start of the body of a SKILL.md.
func readSkill(path string) (desc, body string) {
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	b := make([]byte, maxSkillReadBytes)
	n, _ := f.Read(b)
	text := strings.ReplaceAll(string(b[:n]), "\r\n", "\n")
	front, rest, ok := splitFrontMatter(text)
	if !ok {
		return "", text
	}
	return frontDescription(front), rest
}

// splitFrontMatter splits "---\n<yaml>\n---\n<body>".
func splitFrontMatter(text string) (front, body string, ok bool) {
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		return "", text, false
	}
	front, body, ok = strings.Cut(rest, "\n---")
	if !ok {
		return "", text, false
	}
	_, body, _ = strings.Cut(body, "\n")
	return front, body, true
}

// frontDescription reads the description key of simple YAML front matter: a plain or quoted scalar,
// or a folded/literal block (> or |) of indented lines.
func frontDescription(front string) string {
	lines := strings.Split(front, "\n")
	for i, l := range lines {
		v, ok := strings.CutPrefix(l, "description:")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if v == "" || strings.HasPrefix(v, ">") || strings.HasPrefix(v, "|") {
			var parts []string
			for _, next := range lines[i+1:] {
				if next != "" && next[0] != ' ' && next[0] != '\t' {
					break
				}
				parts = append(parts, strings.TrimSpace(next))
			}
			return strings.TrimSpace(strings.Join(parts, " "))
		}
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		return v
	}
	return ""
}

// MaxDescriptionRunes caps the one-line description.
const MaxDescriptionRunes = 200

// oneLine is the first sentence of desc, else the first paragraph of body (headings skipped).
func oneLine(desc, body string) string {
	s := strings.Join(strings.Fields(desc), " ")
	if s != "" {
		if i := strings.Index(s, ". "); i >= 0 {
			s = s[:i+1]
		}
	} else {
		var para []string
		for _, l := range strings.Split(body, "\n") {
			t := strings.TrimSpace(l)
			switch {
			case t == "" && len(para) > 0:
				return capRunes(strings.Join(para, " "))
			case t == "" || strings.HasPrefix(t, "#"):
				continue
			default:
				para = append(para, t)
			}
		}
		s = strings.Join(para, " ")
	}
	return capRunes(s)
}

func capRunes(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < ' ' || r == 0x7f {
			r = ' '
		}
		b.WriteRune(r)
	}
	rs := []rune(strings.Join(strings.Fields(b.String()), " "))
	if len(rs) > MaxDescriptionRunes {
		return string(rs[:MaxDescriptionRunes-1]) + "…"
	}
	return string(rs)
}
