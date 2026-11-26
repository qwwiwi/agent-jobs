package jobs

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// removeFixture is a tree with a directory "keep" two levels down that the tests treat as a mount
// point, with siblings at both levels:
//
//	tree/a/keep/data.txt  tree/a/sibling/f  tree/a/file  tree/b/g
func removeFixture(t *testing.T) (tree, keep string) {
	t.Helper()
	tree = filepath.Join(t.TempDir(), "tree")
	keep = filepath.Join(tree, "a", "keep")
	writeFile(t, filepath.Join(keep, "data.txt"), []byte("mounted\n"), 0o600)
	writeFile(t, filepath.Join(tree, "a", "sibling", "f"), []byte("x"), 0o600)
	writeFile(t, filepath.Join(tree, "a", "file"), []byte("x"), 0o600)
	writeFile(t, filepath.Join(tree, "b", "g"), []byte("x"), 0o600)
	return tree, keep
}

// sameDir says whether the open directory fd is the directory at path.
func sameDir(t *testing.T, fd int, path string) bool {
	t.Helper()
	var a, b unix.Stat_t
	if err := unix.Fstat(fd, &a); err != nil {
		t.Fatal(err)
	}
	if err := unix.Lstat(path, &b); err != nil {
		return false
	}
	return a.Ino == b.Ino && a.Dev == b.Dev
}

// stubOpener makes the opener answer errno for the directory at path (and only for it).
func stubOpener(t *testing.T, path string, errno syscall.Errno) {
	t.Helper()
	old := openDirBeneath
	openDirBeneath = func(dirfd int, name string) (int, error) {
		if name == filepath.Base(path) && sameDir(t, dirfd, filepath.Dir(path)) {
			return -1, errno
		}
		return old(dirfd, name)
	}
	t.Cleanup(func() { openDirBeneath = old })
}

func checkBoundaryKept(t *testing.T, tree, keep string, err error) {
	t.Helper()
	if !errors.Is(err, ErrMountBoundary) {
		t.Fatalf("err = %v, want ErrMountBoundary", err)
	}
	if b, rerr := os.ReadFile(filepath.Join(keep, "data.txt")); rerr != nil || string(b) != "mounted\n" {
		t.Fatalf("content behind the boundary was touched: %q, %v", b, rerr)
	}
	for _, gone := range []string{filepath.Join(tree, "a", "sibling"), filepath.Join(tree, "a", "file"), filepath.Join(tree, "b")} {
		if exists(gone) {
			t.Fatalf("%s: a sibling of the boundary was kept", gone)
		}
	}
}

// #240 High (a): a directory the kernel reports as a mount point (EXDEV from the opener) is neither
// entered nor removed; its ancestors stay, everything else in the tree goes.
func TestRemoveTreeStopsAtMountBoundary(t *testing.T) {
	tree, keep := removeFixture(t)
	stubOpener(t, keep, syscall.EXDEV)
	checkBoundaryKept(t, tree, keep, removeTree(tree))
}

// The second, portable check: a directory on another device is left alone the same way.
func TestRemoveTreeDeviceMismatch(t *testing.T) {
	tree, keep := removeFixture(t)
	old := dirDevice
	dirDevice = func(fd int) (uint64, error) {
		dev, err := old(fd)
		if sameDir(t, fd, keep) {
			dev++
		}
		return dev, err
	}
	t.Cleanup(func() { dirDevice = old })
	checkBoundaryKept(t, tree, keep, removeTree(tree))
}

// Fail closed: without openat2 (ENOSYS) nothing is removed and the error says so.
func TestRemoveTreeFailsClosedWithoutOpenat2(t *testing.T) {
	tree, keep := removeFixture(t)
	old := openDirBeneath
	openDirBeneath = func(int, string) (int, error) { return -1, syscall.ENOSYS }
	t.Cleanup(func() { openDirBeneath = old })
	err := removeTree(tree)
	if !errors.Is(err, syscall.ENOSYS) {
		t.Fatalf("err = %v, want ENOSYS", err)
	}
	for _, p := range []string{filepath.Join(keep, "data.txt"), filepath.Join(tree, "a", "sibling", "f"),
		filepath.Join(tree, "a", "file"), filepath.Join(tree, "b", "g")} {
		if !exists(p) {
			t.Fatalf("%s removed although no directory could be opened safely", p)
		}
	}
}

// A directory swapped for a symlink to an outside directory between the listing and the open is not
// followed: the outside directory keeps its content.
func TestRemoveTreeSymlinkSwap(t *testing.T) {
	tree, victim := removeFixture(t)
	outside := filepath.Join(t.TempDir(), "outside")
	writeFile(t, filepath.Join(outside, "precious"), []byte("p"), 0o600)
	old := openDirBeneath
	openDirBeneath = func(dirfd int, name string) (int, error) {
		if name == "keep" {
			if err := os.Rename(victim, victim+".moved"); err != nil {
				t.Error(err)
			}
			if err := os.Symlink(outside, victim); err != nil {
				t.Error(err)
			}
		}
		return old(dirfd, name)
	}
	t.Cleanup(func() { openDirBeneath = old })
	if err := removeTree(tree); err == nil {
		t.Fatal("a swapped directory went unreported")
	}
	if !exists(filepath.Join(outside, "precious")) {
		t.Fatal("the removal followed a symlink out of the tree")
	}
	openDirBeneath = old
	if err := removeTree(tree); err != nil || exists(tree) {
		t.Fatalf("second call: err %v, tree exists %v", err, exists(tree))
	}
	if !exists(filepath.Join(outside, "precious")) {
		t.Fatal("the link was followed on the second call")
	}
}

// Read-only directories (a job's leftovers, the job dir's 0500 no-hooks/) are removed: each is made
// writable through its own descriptor. A symlink named as the tree is removed as a link.
func TestRemoveTreeReadOnlyDirs(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	tree := filepath.Join(base, "tree")
	writeFile(t, filepath.Join(outside, "precious"), []byte("p"), 0o600)
	writeFile(t, filepath.Join(tree, "a", "b", "f"), []byte("x"), 0o400)
	if err := os.Symlink(outside, filepath.Join(tree, "a", "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(base, "tree-link")); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{outside, filepath.Join(tree, "a", "b"), filepath.Join(tree, "a"), tree} {
		if err := os.Chmod(d, 0o500); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = os.Chmod(outside, 0o700) })
	n, err := removeTreeSized(tree)
	if err != nil || exists(tree) {
		t.Fatalf("err %v, tree exists %v", err, exists(tree))
	}
	if n < 1 {
		t.Fatalf("freed bytes = %d, want the size of what was removed", n)
	}
	if err := removeTree(filepath.Join(base, "tree-link")); err != nil || exists(filepath.Join(base, "tree-link")) {
		t.Fatalf("symlink named as the tree: %v", err)
	}
	fi, err := os.Lstat(outside)
	if err != nil || fi.Mode().Perm() != 0o500 || !exists(filepath.Join(outside, "precious")) {
		t.Fatalf("a symlink was followed: %v %v", fi, err)
	}
	if err := removeTree(filepath.Join(base, "missing")); err != nil {
		t.Fatalf("a missing path is not an error: %v", err)
	}
}

// The walk is bounded: it stops after the entry budget and a later call finishes the removal.
func TestRemoveTreeBudget(t *testing.T) {
	tree := filepath.Join(t.TempDir(), "tree")
	for i := range 40 {
		writeFile(t, filepath.Join(tree, "d", string(rune('a'+i%26))+string(rune('a'+i/26)), "f"), []byte("x"), 0o600)
	}
	oldN, oldB := removeMaxEntries, removeBudget
	removeMaxEntries, removeBudget = 10, time.Minute
	t.Cleanup(func() { removeMaxEntries, removeBudget = oldN, oldB })
	err := removeTree(tree)
	if !errors.Is(err, errRemovalBudget) || !exists(tree) {
		t.Fatalf("err = %v (tree exists %v), want the budget error and a tree left", err, exists(tree))
	}
	for i := 0; i < 20 && err != nil; i++ {
		if err = removeTree(tree); err != nil && !errors.Is(err, errRemovalBudget) {
			t.Fatal(err)
		}
	}
	if err != nil || exists(tree) {
		t.Fatalf("later calls did not finish the removal: %v", err)
	}
	removeBudget = -time.Second
	writeFile(t, filepath.Join(tree, "f"), []byte("x"), 0o600)
	if err := removeTree(tree); !errors.Is(err, errRemovalBudget) {
		t.Fatalf("deadline: err = %v", err)
	}
}

// An empty directory that cannot be opened (mode 0000, a test suite's leftover) is still removed:
// rmdir needs no descriptor. One with content stays and is reported.
func TestRemoveTreeUnreadableDirs(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root opens every directory")
	}
	tree := filepath.Join(t.TempDir(), "tree")
	empty, full := filepath.Join(tree, "empty"), filepath.Join(tree, "full")
	writeFile(t, filepath.Join(full, "f"), []byte("x"), 0o600)
	if err := os.Mkdir(empty, 0); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(full, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(full, 0o700) })
	err := removeTree(tree)
	if !errors.Is(err, syscall.EACCES) || exists(empty) || !exists(full) {
		t.Fatalf("err %v, empty exists %v, full exists %v; want EACCES for full only", err, exists(empty), exists(full))
	}
}
