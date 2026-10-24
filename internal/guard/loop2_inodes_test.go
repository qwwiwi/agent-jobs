package guard

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// Fix loop 2, item 4: a recursive change or a copy into a directory reaches every file below it, so a
// hard link of a protected file anywhere in the destination tree (also below a symlinked directory)
// is refused; so is a protected file reached through a symlink inside a protected directory.
func TestRecursiveDestinationsWithHardLinks(t *testing.T) {
	gc, _, cfg := jobFolderSetup(t)
	aliases := filepath.Join(cfg.Dir, "aliases")
	if err := os.MkdirAll(filepath.Join(aliases, "deep"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(gc, filepath.Join(aliases, "deep", "gitconfig")); err != nil {
		t.Skipf("hard links not supported here: %v", err)
	}
	other := t.TempDir()
	if err := os.Link(gc, filepath.Join(other, "gc")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(cfg.Dir, "via"), 0o700); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, other, filepath.Join(cfg.Dir, "via", "sub"))
	for _, d := range []string{"payload", "build"} {
		if err := os.MkdirAll(filepath.Join(cfg.Dir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	refused(t, cfg, "chmod -R u+w aliases", "chmod -R u+w aliases && cp -r payload/. aliases", "cp -r payload/. aliases",
		"cp -a payload/. aliases/", "rsync -a payload/ aliases/", "tar -xf a.tar -C aliases", "unzip -o a.zip -d aliases",
		"chown -R me via", "cp -r payload/. via")
	allowed(t, cfg, "chmod -R u+w build", "cp -r payload/. build", "rsync -a payload/ build/", "rm -rf build")

	// A protected directory holding a symlink to a file outside: a hard link of that file is refused.
	cards := filepath.Join(cfg.StateDir, "cards")
	ext := t.TempDir()
	card := filepath.Join(ext, "card.md")
	if err := os.WriteFile(card, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cards, 0o700); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, card, filepath.Join(cards, "p.md"))
	if err := os.Link(card, filepath.Join(cfg.Dir, "card-alias.md")); err != nil {
		t.Fatal(err)
	}
	cfg.Protected = append(cfg.Protected, cards)
	if r := CheckFileWrite("Write", filepath.Join(cfg.Dir, "card-alias.md"), cfg); r == "" {
		t.Error("Write to a hard link of a symlinked card allowed")
	}
	refused(t, cfg, "chmod u+w card-alias.md", "chmod -R u+w .")
}

// Fix loop 2, item 4: past the scan bound a destination tree that may hold a hard link of a
// protected file is refused (fail closed).
func TestRecursiveDestinationScanBound(t *testing.T) {
	gc, _, cfg := jobFolderSetup(t)
	if err := os.Link(gc, filepath.Join(t.TempDir(), "gc")); err != nil {
		t.Skipf("hard links not supported here: %v", err)
	}
	old := maxInodeScan
	maxInodeScan = 50
	t.Cleanup(func() { maxInodeScan = old })
	big := filepath.Join(cfg.Dir, "big")
	if err := os.Mkdir(big, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := range 60 {
		if err := os.WriteFile(filepath.Join(big, "f"+strconv.Itoa(i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	refused(t, cfg, "chmod -R u+w big")
	allowed(t, cfg, "chmod u+w big/f1")
}
