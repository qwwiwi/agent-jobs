package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestTrashRootsConfig(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) {
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"trash_roots": ["relative/dir"]}`)
	if _, err := LoadConfig(dir); err == nil || !strings.Contains(err.Error(), "trash_roots") {
		t.Fatalf("relative root accepted: %v", err)
	}
	write(`{"trash_roots": ["/srv/a", "/srv/b"]}`)
	c, err := LoadConfig(dir)
	if err != nil || strings.Join(c.TrashRootsOrDefault(), ",") != "/srv/a,/srv/b" {
		t.Fatalf("roots: %v %v", c.TrashRoots, err)
	}
	home, _ := os.UserHomeDir()
	if got := (Config{}).TrashRootsOrDefault(); len(got) != 1 || got[0] != home {
		t.Fatalf("default roots %v, want the home dir", got)
	}
}

// TailFile returns the last lines and refuses what the job could plant instead of a log.
func TestTailFileRefusesNonRegular(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "log")
	if err := os.WriteFile(p, []byte("a\nb\nc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := TailFile(p, 2); err != nil || got != "b\nc" {
		t.Fatalf("tail: %q %v", got, err)
	}
	if err := os.Symlink(p, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := TailFile(filepath.Join(dir, "link"), 2); err == nil {
		t.Fatal("symlink followed")
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := TailFile(fifo, 2); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("fifo: %v", err)
	}
}
