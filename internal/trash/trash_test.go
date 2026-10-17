package trash

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fixture struct {
	t     *Trash
	root  string // allowed root
	state string // protected state dir (holds the trash)
	out   string // outside every root
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := fixture{root: filepath.Join(base, "root"), state: filepath.Join(base, "state"), out: filepath.Join(base, "outside")}
	for _, d := range []string{f.root, f.state, f.out} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	f.t = &Trash{Dir: filepath.Join(f.state, "trash"), Roots: []string{f.root}, Protected: []string{f.state}}
	return f
}

func write(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func gone(p string) bool {
	_, err := os.Lstat(p)
	return os.IsNotExist(err)
}

// trash_move then trash_restore puts a file, a directory and a symlink back unchanged; the symlink
// is moved as the link, its target is never touched.
func TestMoveRestoreRoundTrip(t *testing.T) {
	f := newFixture(t)
	file := filepath.Join(f.root, "a.txt")
	write(t, file, "file body")
	dir := filepath.Join(f.root, "proj", "build")
	write(t, filepath.Join(dir, "deep", "x.bin"), "xx")
	target := filepath.Join(f.out, "target.txt")
	write(t, target, "outside")
	link := filepath.Join(f.root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ path, kind string }{{file, "file"}, {dir, "dir"}, {link, "symlink"}} {
		e, err := f.t.Move(c.path)
		if err != nil {
			t.Fatalf("move %s: %v", c.path, err)
		}
		if e.Type != c.kind || e.OriginalPath != c.path || !gone(c.path) {
			t.Fatalf("move %s: %+v (gone=%v)", c.path, e, gone(c.path))
		}
		if _, err := os.Lstat(e.TrashPath); err != nil {
			t.Fatalf("item not in trash: %v", err)
		}
		list, err := f.t.List(0)
		if err != nil || len(list) == 0 || list[0].ID != e.ID {
			t.Fatalf("list: %+v %v", list, err)
		}
		r, err := f.t.Restore(e.ID)
		if err != nil {
			t.Fatalf("restore %s: %v", e.ID, err)
		}
		if r.OriginalPath != c.path || gone(c.path) {
			t.Fatalf("restore %s: %+v", c.path, r)
		}
		if !gone(filepath.Join(f.t.Dir, e.ID)) {
			t.Fatal("restored entry left behind in the trash")
		}
	}
	if read(t, file) != "file body" || read(t, filepath.Join(dir, "deep", "x.bin")) != "xx" || read(t, target) != "outside" {
		t.Fatal("content changed by the round trip")
	}
	if got, err := os.Readlink(link); err != nil || got != target {
		t.Fatalf("symlink not restored as a link: %q %v", got, err)
	}
}

func TestMoveRefusals(t *testing.T) {
	f := newFixture(t)
	write(t, filepath.Join(f.out, "secret"), "s")
	write(t, filepath.Join(f.root, "sub", "ok"), "o")
	// A symlinked dir inside the root that points outside: a path through it resolves outside.
	if err := os.Symlink(f.out, filepath.Join(f.root, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ path, want string }{
		{"relative/path", "not an absolute path"},
		{f.root + "/sub/../../outside/secret", "`..`"}, // not via filepath.Join: it would clean the `..` away
		{filepath.Join(f.out, "secret"), "outside the allowed roots"},
		{filepath.Join(f.root, "escape", "secret"), "outside the allowed roots"},
		{f.root, "allowed root"},
		{filepath.Dir(f.root), "allowed root"},
		{"/", "names no file"},
		{filepath.Join(f.root, "missing"), "no such file"},
	} {
		_, err := f.t.Move(c.path)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("move %q: got %v, want %q", c.path, err, c.want)
		}
	}
	if read(t, filepath.Join(f.out, "secret")) != "s" {
		t.Fatal("outside file touched")
	}
	// The protected state dir (and the trash inside it) is refused even when it lies in a root.
	g := newFixture(t)
	g.t.Roots = []string{filepath.Dir(g.state)}
	write(t, filepath.Join(g.state, "jobs.db"), "db")
	for _, p := range []string{g.state, filepath.Join(g.state, "jobs.db"), g.t.Dir} {
		if _, err := g.t.Move(p); err == nil || !strings.Contains(err.Error(), "protected") {
			t.Fatalf("move %s: got %v, want protected refusal", p, err)
		}
	}
	// The escape symlink itself (the last component) is fine to move: it is moved as a link.
	e, err := f.t.Move(filepath.Join(f.root, "escape"))
	if err != nil || e.Type != "symlink" {
		t.Fatalf("move the link itself: %+v %v", e, err)
	}
	if read(t, filepath.Join(f.out, "secret")) != "s" {
		t.Fatal("link target touched")
	}
	// No roots configured: nothing can be moved.
	none := &Trash{Dir: f.t.Dir, Protected: f.t.Protected}
	if _, err := none.Move(filepath.Join(f.root, "sub", "ok")); err == nil || !strings.Contains(err.Error(), "no allowed roots") {
		t.Fatalf("no roots: %v", err)
	}
}

// Protected files (owner rules, the binary) are refused as named, through a link that names them,
// and when only their resolved target is listed; so is any path with a .git component.
func TestMoveRefusesProtectedFilesAndGit(t *testing.T) {
	f := newFixture(t)
	rules := filepath.Join(f.root, "rules", "owner-rules.md")
	write(t, rules, "r")
	realBin := filepath.Join(f.root, "opt", "agent-jobs-1.0")
	write(t, realBin, "bin")
	binLink := filepath.Join(f.root, "bin", "agent-jobs")
	if err := os.MkdirAll(filepath.Dir(binLink), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realBin, binLink); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(f.root, "other-link")
	if err := os.Symlink(realBin, other); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(f.root, "repo", ".git", "config"), "c")
	write(t, filepath.Join(f.root, "wt", ".git"), "gitdir: x")
	f.t.Protected = append(f.t.Protected, rules, binLink)
	for _, p := range []string{rules, filepath.Dir(rules), binLink, realBin, other, filepath.Dir(binLink),
		filepath.Join(f.root, "repo", ".git"), filepath.Join(f.root, "repo", ".git", "config"), filepath.Join(f.root, "wt", ".git")} {
		_, err := f.t.Move(p)
		if err == nil || !strings.Contains(err.Error(), "refused") {
			t.Fatalf("move %s: got %v, want a refusal", p, err)
		}
	}
	for _, p := range []string{rules, binLink, realBin, filepath.Join(f.root, "repo", ".git", "config")} {
		if gone(p) {
			t.Fatalf("%s moved", p)
		}
	}
	// The repository around .git is the user's to trash.
	if _, err := f.t.Move(filepath.Join(f.root, "repo")); err != nil {
		t.Fatalf("move the repo: %v", err)
	}
}

func TestRestoreRefusals(t *testing.T) {
	f := newFixture(t)
	p := filepath.Join(f.root, "dir", "f.txt")
	write(t, p, "v1")
	e, err := f.t.Move(p)
	if err != nil {
		t.Fatal(err)
	}
	// Occupied: never overwritten.
	write(t, p, "new")
	if _, err := f.t.Restore(e.ID); err == nil || !strings.Contains(err.Error(), "exists") {
		t.Fatalf("restore over an existing file: %v", err)
	}
	if read(t, p) != "new" {
		t.Fatal("existing file overwritten")
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	// Parent gone: refused, the item stays in the trash.
	if err := os.Remove(filepath.Dir(p)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.t.Restore(e.ID); err == nil {
		t.Fatal("restore into a missing parent succeeded")
	}
	// Parent replaced by a symlink out of the root: refused.
	if err := os.Symlink(f.out, filepath.Dir(p)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.t.Restore(e.ID); err == nil || !strings.Contains(err.Error(), "outside the allowed roots") {
		t.Fatalf("restore through a symlink out: %v", err)
	}
	if !gone(filepath.Join(f.out, "f.txt")) {
		t.Fatal("item restored outside the root")
	}
	if err := os.Remove(filepath.Dir(p)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := f.t.Restore(e.ID); err != nil || read(t, p) != "v1" {
		t.Fatalf("restore after fixing the parent: %v", err)
	}
	for _, id := range []string{"", "../x", "261017-120000-zzzzzzzz"} {
		if _, err := f.t.Restore(id); err == nil {
			t.Fatalf("restore %q succeeded", id)
		}
	}
	if _, err := f.t.Restore("261017-120000-0000abcd"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unknown id: %v", err)
	}
}
