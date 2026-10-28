package testutil

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The directory is short enough for a daemon's socket below a state dir, whatever TMPDIR is, and it
// is gone after the test.
func TestShortTempDirHoldsSockets(t *testing.T) {
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), strings.Repeat("long-tmpdir-", 8)))
	var dir string
	t.Run("inner", func(t *testing.T) {
		dir = ShortTempDir(t)
		if !strings.HasPrefix(dir, ShortRoot+string(os.PathSeparator)) {
			t.Fatalf("dir %s not under %s", dir, ShortRoot)
		}
		state := filepath.Join(dir, ".agent-jobs", "accept-0123abcd")
		if err := os.MkdirAll(state, 0o700); err != nil {
			t.Fatal(err)
		}
		l, err := net.Listen("unix", filepath.Join(state, "http-api.sock"))
		if err != nil {
			t.Fatal(err)
		}
		_ = l.Close()
		if err := os.Chmod(state, 0o500); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("dir %s left behind: %v", dir, err)
	}
}
