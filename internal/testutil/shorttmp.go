// Package testutil holds helpers the tests of several packages share. Production code does not
// import it.
package testutil

import (
	"os"
	"os/exec"
	"testing"
)

// ShortRoot is where ShortTempDir creates its directories: /tmp is short on every supported host,
// while TMPDIR may not be (a job's TMPDIR is <state>/jobs/<id>/tmp, and macOS's is under /var/folders).
const ShortRoot = "/tmp"

// ShortTempDir returns a new directory under ShortRoot, removed when the test ends, for tests that
// create unix sockets below it: a socket path holds at most 104 bytes on macOS and 108 on Linux,
// which a t.TempDir() under a long TMPDIR already exceeds. The production socket paths are not
// changed; the tests only put the state dir somewhere short.
func ShortTempDir(t testing.TB) string {
	t.Helper()
	dir, err := os.MkdirTemp(ShortRoot, "aj")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Tests may leave read-only directories behind (a frozen worktree): make them removable first.
		_ = exec.Command("chmod", "-R", "u+w", dir).Run()
		_ = os.RemoveAll(dir)
	})
	return dir
}
