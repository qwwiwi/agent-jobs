package skills

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// skill is one test catalog entry; body is SKILL.md (a default one when empty).
type skill struct {
	name, path, status, body string
	kinds, projects          []string
	extraKB                  int
}

func writeCatalog(t *testing.T, root string, list []skill) {
	t.Helper()
	var idx []map[string]any
	for _, s := range list {
		p := s.path
		if p == "" {
			p = "skills/" + s.name
		}
		dir := filepath.Join(root, filepath.FromSlash(p))
		if !strings.Contains(p, "..") {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			body := s.body
			if body == "" {
				body = "---\nname: " + s.name + "\ndescription: Does " + s.name + " things. More text.\n---\n\n# " + s.name + "\n\nBody of " + s.name + ".\n"
			}
			if body != "-" {
				if err := os.WriteFile(filepath.Join(dir, SkillFile), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if s.extraKB > 0 {
				if err := os.WriteFile(filepath.Join(dir, "big.txt"), make([]byte, s.extraKB*1024), 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
		st := s.status
		if st == "" {
			st = StatusActive
		}
		idx = append(idx, map[string]any{"name": s.name, "path": p, "status": st, "kinds": s.kinds,
			"projects": s.projects, "last_verified": "2026-10-01", "risk": []string{}})
	}
	b, _ := json.Marshal(map[string]any{"version": 1, "skills": idx, "plugins": []any{}})
	if err := os.WriteFile(filepath.Join(root, IndexFile), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func names(c *Catalog) []string { return Names(c.Entries) }

func TestLoadValidatesEntries(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "evil"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "evil", SkillFile), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeCatalog(t, root, []skill{
		{name: "good"},
		{name: "dotdot", path: "../" + filepath.Base(outside) + "/evil"},
		{name: "nofile", body: "-"},
		{name: "mismatch", path: "skills/other-name"},
		{name: "badstatus", status: "retired"},
	})
	// A symlink inside the root that points outside it.
	if err := os.Symlink(filepath.Join(outside, "evil"), filepath.Join(root, "skills", "evil")); err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	b, _ := os.ReadFile(filepath.Join(root, IndexFile))
	_ = json.Unmarshal(b, &raw)
	raw["skills"] = append(raw["skills"].([]any), map[string]any{"name": "evil", "path": "skills/evil", "status": "active"})
	b, _ = json.Marshal(raw)
	_ = os.WriteFile(filepath.Join(root, IndexFile), b, 0o644)

	c := Load([]string{root, filepath.Join(t.TempDir(), "missing"), "relative/root"})
	if got := names(c); !slices.Equal(got, []string{"good"}) {
		t.Fatalf("entries %v, want only good", got)
	}
	want := []string{"dotdot", "nofile", "mismatch", "badstatus", "evil", "missing", "relative/root"}
	all := strings.Join(c.Warnings, "\n")
	for _, w := range want {
		if !strings.Contains(all, w) {
			t.Errorf("no warning for %s in:\n%s", w, all)
		}
	}
	if e, _ := c.Lookup("good"); e.Description != "Does good things." {
		t.Fatalf("description %q", e.Description)
	}
}

func TestLoadSymlinkInsideTreeLeavingRoot(t *testing.T) {
	root := t.TempDir()
	writeCatalog(t, root, []skill{{name: "a"}})
	if err := os.Symlink("/etc", filepath.Join(root, "skills", "a", "etc")); err != nil {
		t.Fatal(err)
	}
	c := Load([]string{root})
	if len(c.Entries) != 0 || !strings.Contains(strings.Join(c.Warnings, " "), "symlink etc leaves the skill directory") {
		t.Fatalf("entries %v warnings %v", names(c), c.Warnings)
	}
}

func TestDuplicateNamesRootOrder(t *testing.T) {
	r1, r2 := t.TempDir(), t.TempDir()
	writeCatalog(t, r1, []skill{{name: "a", status: StatusCore, kinds: []string{"code"}}})
	writeCatalog(t, r2, []skill{{name: "a", status: StatusDraft}, {name: "b"}})
	c := Load([]string{r1, r2})
	e, _ := c.Lookup("a")
	if e.Status != StatusCore || !strings.HasPrefix(e.Dir, mustReal(t, r1)) {
		t.Fatalf("a from %s (%s), want the first root", e.Dir, e.Status)
	}
	if !slices.Equal(names(c), []string{"a", "b"}) {
		t.Fatalf("names %v", names(c))
	}
}

func mustReal(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func testCatalog(t *testing.T) *Catalog {
	t.Helper()
	root := t.TempDir()
	writeCatalog(t, root, []skill{
		{name: "golang", status: StatusCore, kinds: []string{"code"}},
		{name: "git-workflows", status: StatusCore, kinds: []string{"code", "review"}},
		{name: "review-checklist", status: StatusCore, kinds: []string{"review"}},
		{name: "web-research", status: StatusCore, kinds: []string{"research"}},
		{name: "proj-deploy", status: StatusActive, projects: []string{"myrepo"}},
		{name: "old-thing", status: StatusDeprecated, kinds: []string{"code"}},
		{name: "new-idea", status: StatusDraft, kinds: []string{"code"}},
		{name: "extra", status: StatusActive},
		{name: "huge", status: StatusActive, extraKB: 500},
	})
	return Load([]string{root})
}

func TestDefaultsByKindAndProject(t *testing.T) {
	c := testCatalog(t)
	for _, tc := range []struct {
		kind, repo string
		want       []string
	}{
		{"fix", "", []string{"git-workflows", "golang"}},
		{"general", "", []string{"git-workflows", "golang"}},
		{"review", "", []string{"git-workflows", "review-checklist"}},
		{"research", "", []string{"web-research"}},
		{"research", "myrepo", []string{"proj-deploy", "web-research"}},
	} {
		if got := c.Defaults(tc.kind, tc.repo); !slices.Equal(got, tc.want) {
			t.Errorf("defaults %s/%s = %v, want %v", tc.kind, tc.repo, got, tc.want)
		}
	}
}

func TestSelectEdits(t *testing.T) {
	c := testCatalog(t)
	b := Budget{MaxCount: DefaultMaxPerJob, MaxKB: DefaultMaxKB}
	got, err := c.Select("fix", "myrepo", []string{"+extra", "-golang", "new-idea"}, b)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"extra", "git-workflows", "new-idea", "proj-deploy"}; !slices.Equal(Names(got), want) {
		t.Fatalf("selected %v, want %v", Names(got), want)
	}
	for _, tc := range []struct {
		edits []string
		want  string
	}{
		{[]string{"golnag"}, `unknown skill "golnag" (close: golang)`},
		{[]string{"-nope"}, `unknown skill "nope"`},
		{[]string{"old-thing"}, `"old-thing" is deprecated`},
		{[]string{"huge"}, "skills_max_kb"},
		{[]string{"extra", "review-checklist", "web-research", "new-idea", "proj-deploy"}, "skills_max_per_job"},
	} {
		_, err := c.Select("fix", "", tc.edits, b)
		if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("edits %v: err %v, want %q", tc.edits, err, tc.want)
		}
	}
	// A draft is never a default; a deprecated one never either.
	if d := c.Defaults("fix", ""); slices.Contains(d, "new-idea") || slices.Contains(d, "old-thing") {
		t.Fatalf("defaults %v hold a draft or deprecated skill", d)
	}
	// No catalog: unknown names say so.
	if _, err := Load(nil).Select("fix", "", []string{"golang"}, b); err == nil || !strings.Contains(err.Error(), "skill_roots") {
		t.Fatalf("no catalog: %v", err)
	}
}

func TestSearch(t *testing.T) {
	root := t.TempDir()
	writeCatalog(t, root, []skill{
		{name: "alpha", body: "---\nname: alpha\ndescription: Formatting helpers.\n---\nThe zanzibar protocol lives only here in the body.\n"},
		{name: "beta", body: "---\nname: beta\ndescription: Code review for pull requests.\n---\nreview review review.\n"},
		{name: "gamma", body: "---\nname: gamma\ndescription: Notes mention review once.\n---\nnothing\n"},
		{name: "rus", body: "---\nname: rus\ndescription: >\n  Ревью кода и проверка изменений.\n  Вторая строка.\n---\nТекст.\n"},
		{name: "drafty", status: StatusDraft, body: "---\nname: drafty\ndescription: review draft.\n---\n"},
		{name: "gone", status: StatusDeprecated, body: "---\nname: gone\ndescription: review gone.\n---\n"},
	})
	c := Load([]string{root})
	if h := c.Search("zanzibar", Filter{}, 0); len(h) != 1 || h[0].Name != "alpha" {
		t.Fatalf("body-only match: %+v", h)
	}
	h := c.Search("review", Filter{}, 0)
	if len(h) < 2 || h[0].Name != "beta" || h[1].Name != "gamma" {
		t.Fatalf("ranking: %+v", h)
	}
	for _, x := range h {
		if x.Name == "drafty" || x.Name == "gone" {
			t.Fatalf("draft or deprecated returned without status: %+v", h)
		}
	}
	if h := c.Search("РЕВЬЮ", Filter{}, 0); len(h) != 1 || h[0].Name != "rus" {
		t.Fatalf("cyrillic: %+v", h)
	}
	if h := c.Search("review", Filter{Status: StatusDraft}, 0); len(h) != 1 || h[0].Name != "drafty" {
		t.Fatalf("draft with status=draft: %+v", h)
	}
	if h := c.Search("review", Filter{Status: StatusDeprecated}, 0); len(h) != 0 {
		t.Fatalf("deprecated returned: %+v", h)
	}
	if e, _ := c.Lookup("rus"); e.Description != "Ревью кода и проверка изменений." {
		t.Fatalf("folded description %q", e.Description)
	}
}

func TestOneLineFromBody(t *testing.T) {
	if got := oneLine("", "# Title\n\nFirst para line one\nline two.\n\nSecond."); got != "First para line one line two." {
		t.Fatalf("got %q", got)
	}
}

func TestLinkAndUnlink(t *testing.T) {
	c := testCatalog(t)
	work := t.TempDir()
	// The repository already has git-workflows: it wins.
	own := LinkPath(work, "git-workflows")
	if err := os.MkdirAll(own, 0o755); err != nil {
		t.Fatal(err)
	}
	sel, err := c.Select("fix", "", nil, Budget{MaxCount: 6, MaxKB: 400})
	if err != nil {
		t.Fatal(err)
	}
	d, err := Link(work, sel, 400, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(d.Linked, []string{"golang"}) || !slices.Equal(d.RepoOwned, []string{"git-workflows"}) {
		t.Fatalf("delivery %+v", d)
	}
	e, _ := c.Lookup("golang")
	if target, err := os.Readlink(LinkPath(work, "golang")); err != nil || target != e.Dir {
		t.Fatalf("link -> %q (%v), want %s", target, err, e.Dir)
	}
	if left, err := Unlink(work, d.Linked); len(left) > 0 || err != nil {
		t.Fatalf("left %v (%v)", left, err)
	}
	if _, err := os.Lstat(LinkPath(work, "golang")); !os.IsNotExist(err) {
		t.Fatal("link not removed")
	}
	if _, err := os.Stat(filepath.Join(e.Dir, SkillFile)); err != nil {
		t.Fatal("unlink touched the catalog")
	}
	if _, err := os.Stat(own); err != nil {
		t.Fatal("the repository's own skill was removed")
	}
	// Empty dirs created for the links go; a fresh work dir is left as it was.
	fresh := t.TempDir()
	d, err = Link(fresh, sel[:1], 400, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = Unlink(fresh, d.Linked)
	if _, err := os.Lstat(filepath.Join(fresh, ".claude")); !os.IsNotExist(err) {
		t.Fatal(".claude left behind")
	}
}

func TestLinkRechecksBudgetAndRefusesSymlinkedDirs(t *testing.T) {
	c := testCatalog(t)
	e, _ := c.Lookup("golang")
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(e.Dir, "grown.bin"), make([]byte, 20<<10), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Link(work, []Entry{e}, 10, nil); err == nil || !strings.Contains(err.Error(), "skills_max_kb") {
		t.Fatalf("grown skill linked: %v", err)
	}
	// .claude as a symlink is never entered.
	work2 := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(work2, ".claude")); err != nil {
		t.Fatal(err)
	}
	if _, err := Link(work2, []Entry{e}, 400, nil); err == nil {
		t.Fatal("linked through a symlinked .claude")
	}
}

// loadOne loads a single-root catalog and returns its entry names and warnings.
func loadOne(root string) ([]string, string) {
	c := Load([]string{root})
	return names(c), strings.Join(c.Warnings, " ")
}

func TestLoadRejectsNestedDirectorySymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeCatalog(t, root, []skill{{name: "a"}, {name: "b"}})
	// a/shared points at <root>/shared (inside the root), whose escape entry leads outside it.
	if err := os.MkdirAll(filepath.Join(root, "shared"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "shared", "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "shared"), filepath.Join(root, "skills", "a", "shared")); err != nil {
		t.Fatal(err)
	}
	got, warns := loadOne(root)
	if !slices.Equal(got, []string{"b"}) || !strings.Contains(warns, "symlink shared") {
		t.Fatalf("entries %v warnings %s", got, warns)
	}
}

func TestLoadRejectsSymlinkToSiblingSkill(t *testing.T) {
	root := t.TempDir()
	writeCatalog(t, root, []skill{{name: "a"}, {name: "b"}})
	if err := os.Symlink(filepath.Join(root, "skills", "b"), filepath.Join(root, "skills", "a", "sib")); err != nil {
		t.Fatal(err)
	}
	// A file symlink into the sibling skill leaves b's directory too.
	if err := os.Symlink(filepath.Join(root, "skills", "a", SkillFile), filepath.Join(root, "skills", "b", "other.md")); err != nil {
		t.Fatal(err)
	}
	got, warns := loadOne(root)
	if len(got) != 0 || !strings.Contains(warns, "symlink sib") || !strings.Contains(warns, "symlink other.md") {
		t.Fatalf("entries %v warnings %s", got, warns)
	}
}

func TestMeasureCountsFileSymlinkTargets(t *testing.T) {
	root := t.TempDir()
	writeCatalog(t, root, []skill{{name: "a", extraKB: 20}})
	ref := filepath.Join(root, "skills", "a", "ref")
	if err := os.MkdirAll(ref, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "big.txt"), filepath.Join(ref, "alias.txt")); err != nil {
		t.Fatal(err)
	}
	c := Load([]string{root})
	e, ok := c.Lookup("a")
	if !ok || e.SizeKB < 40 {
		t.Fatalf("a %v size %.1f KB (warnings %v), want the linked file counted", ok, e.SizeKB, c.Warnings)
	}
}

func TestLoadRejectsSymlinkCycle(t *testing.T) {
	root := t.TempDir()
	writeCatalog(t, root, []skill{{name: "a"}})
	dir := filepath.Join(root, "skills", "a")
	if err := os.Symlink("loop2", filepath.Join(dir, "loop1")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("loop1", filepath.Join(dir, "loop2")); err != nil {
		t.Fatal(err)
	}
	if got, warns := loadOne(root); len(got) != 0 || !strings.Contains(warns, "symlink loop") {
		t.Fatalf("entries %v warnings %s", got, warns)
	}
}

func TestUnlinkReportsLeftAndLinkAdopts(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	c := testCatalog(t)
	e, _ := c.Lookup("golang")
	work := t.TempDir()
	d, err := Link(work, []Entry{e}, 400, nil)
	if err != nil || !slices.Equal(d.Linked, []string{"golang"}) {
		t.Fatalf("link %+v %v", d, err)
	}
	dir := filepath.Join(work, filepath.FromSlash(LinkDir))
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	left, err := Unlink(work, d.Linked)
	if !slices.Equal(left, []string{"golang"}) || err == nil {
		t.Fatalf("left %v err %v, want golang reported", left, err)
	}
	// Without adopt the left link looks like the repository's; with it, it is linked again.
	if d, err := Link(work, []Entry{e}, 400, nil); err != nil || !slices.Equal(d.RepoOwned, []string{"golang"}) {
		t.Fatalf("no adopt: %+v %v", d, err)
	}
	if d, err := Link(work, []Entry{e}, 400, left); err != nil || !slices.Equal(d.Linked, []string{"golang"}) || len(d.RepoOwned) > 0 {
		t.Fatalf("adopt: %+v %v", d, err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if left, err := Unlink(work, []string{"golang"}); len(left) > 0 || err != nil {
		t.Fatalf("left %v err %v", left, err)
	}
}

// An uninspectable parent (.claude unreadable) hides whether the links are still there: every name
// stays reported as left, so its caller keeps tracking it.
func TestUnlinkUnreadableParentKeepsNames(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	c := testCatalog(t)
	e, _ := c.Lookup("golang")
	work := t.TempDir()
	if _, err := Link(work, []Entry{e}, 400, nil); err != nil {
		t.Fatal(err)
	}
	claudeDir := filepath.Join(work, ".claude")
	if err := os.Chmod(claudeDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(claudeDir, 0o755) })
	left, err := Unlink(work, []string{"golang", "../bad"})
	if !slices.Equal(left, []string{"golang"}) || err == nil {
		t.Fatalf("left %v err %v, want golang reported", left, err)
	}
	if err := os.Chmod(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(LinkPath(work, "golang")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link gone: %v", err)
	}
	if left, err := Unlink(work, left); len(left) > 0 || err != nil {
		t.Fatalf("left %v err %v", left, err)
	}
}

// writeRawIndex writes index.json as given and a SKILL.md for each name under skills/<name>.
func writeRawIndex(t *testing.T, root, index string, names ...string) {
	t.Helper()
	for _, n := range names {
		dir := filepath.Join(root, "skills", n)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, SkillFile), []byte("# "+n+"\n\nBody.\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, IndexFile), []byte(index), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadInformationalFieldsAreFreeForm(t *testing.T) {
	root := t.TempDir()
	writeRawIndex(t, root, `{"version": 1, "skills": [
		{"name": "obj", "path": "skills/obj", "status": "core", "kinds": ["code"], "projects": [], "risk": [],
		 "origin": {"host": "example.org", "path": "a/b"}, "owner": {"team": "x"}, "place": 3,
		 "last_verified": "2026-10-01", "has_scripts": "yes", "sha256": "abc", "size_kb": "big", "extra": [1]},
		{"name": "str", "path": "skills/str", "status": "active", "kinds": [], "projects": [], "risk": [],
		 "origin": "upstream", "owner": "me", "has_scripts": true, "last_verified": 20261001},
		{"name": "none", "path": "skills/none", "status": "active"}
	]}`, "obj", "str", "none")
	c := Load([]string{root})
	if got := names(c); !slices.Equal(got, []string{"none", "obj", "str"}) {
		t.Fatalf("entries %v, warnings %v", got, c.Warnings)
	}
	if len(c.Warnings) != 0 {
		t.Fatalf("warnings %v", c.Warnings)
	}
	obj, _ := c.Lookup("obj")
	if obj.Origin != "" || obj.Owner != "" || obj.HasScripts || obj.LastVerified != "2026-10-01" {
		t.Fatalf("obj %+v", obj)
	}
	str, _ := c.Lookup("str")
	if str.Origin != "upstream" || str.Owner != "me" || !str.HasScripts || str.LastVerified != "" {
		t.Fatalf("str %+v", str)
	}
	none, _ := c.Lookup("none")
	if none.Origin != "" || none.Kinds == nil || none.Risk == nil {
		t.Fatalf("none %+v", none)
	}
}

func TestLoadSkipsMalformedEntryKeepsRoot(t *testing.T) {
	root := t.TempDir()
	writeRawIndex(t, root, `{"version": 1, "skills": [
		{"name": "good", "path": "skills/good", "status": "core", "kinds": ["code"], "projects": [], "risk": []},
		{"name": "badkinds", "path": "skills/badkinds", "status": "core", "kinds": "code"},
		{"name": "badrisk", "path": "skills/badrisk", "status": "core", "risk": {"level": 1}},
		42,
		{"name": {"x": 1}, "path": "skills/x", "status": "core"},
		{"name": "also-good", "path": "skills/also-good", "status": "active"}
	]}`, "good", "badkinds", "badrisk", "also-good")
	c := Load([]string{root})
	if got := names(c); !slices.Equal(got, []string{"also-good", "good"}) {
		t.Fatalf("entries %v, warnings %v", got, c.Warnings)
	}
	if len(c.Roots) != 1 {
		t.Fatalf("roots %v", c.Roots)
	}
	w := strings.Join(c.Warnings, "\n")
	for _, want := range []string{`entry 1 ("badkinds")`, `entry 2 ("badrisk")`, "entry 3: ", "entry 4: ", "kinds", "risk"} {
		if !strings.Contains(w, want) {
			t.Errorf("warnings %q lack %q", w, want)
		}
	}
	if strings.Contains(w, "parse index.json") || len(c.Warnings) != 4 {
		t.Fatalf("warnings %q", w)
	}
}

// A warning about an invalid entry names it by its index only: a name that fails ValidName (control
// characters, a path, a long text) is not repeated; a valid name is.
func TestLoadWarningOmitsInvalidName(t *testing.T) {
	root := t.TempDir()
	long := strings.Repeat("x", 200)
	writeRawIndex(t, root, `{"version": 1, "skills": [
		{"name": "ok", "path": "skills/ok", "status": "core"},
		{"name": "evil\u001b[31mred\nline", "path": "skills/ok", "status": "core"},
		{"name": "sub/dir-name", "path": "skills/ok", "status": "core"},
		{"name": "`+long+`", "path": "skills/ok", "status": "core"},
		{"name": "bad status", "path": "skills/ok", "status": 7},
		{"name": "named", "path": "skills/named", "status": "nope"}
	]}`, "ok")
	c := Load([]string{root})
	if got := names(c); !slices.Equal(got, []string{"ok"}) {
		t.Fatalf("entries %v, warnings %v", got, c.Warnings)
	}
	w := strings.Join(c.Warnings, "\n")
	for _, bad := range []string{"evil", "red", "\x1b", "dir-name", long[:65], "bad status"} {
		if strings.Contains(w, bad) {
			t.Errorf("warnings %q hold the invalid name part %q", w, bad)
		}
	}
	for _, want := range []string{"entry 1: ", "entry 2: ", "entry 3: ", "entry 4: ", `entry 5 ("named")`} {
		if !strings.Contains(w, want) {
			t.Errorf("warnings %q lack %q", w, want)
		}
	}
	if len(c.Warnings) != 5 {
		t.Fatalf("warnings %q", w)
	}
}
