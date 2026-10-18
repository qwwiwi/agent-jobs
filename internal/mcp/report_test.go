package mcp

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// reportFixture is a state dir with jobs/<id>/report.md and an outside file a job could point at.
func reportFixture(t *testing.T) (state, id, outside string) {
	t.Helper()
	root := t.TempDir()
	state = filepath.Join(root, "state")
	id = "261018-100000-aaaa"
	if err := os.MkdirAll(filepath.Join(state, "jobs", id), 0o700); err != nil {
		t.Fatal(err)
	}
	outside = filepath.Join(root, "outside")
	if err := os.MkdirAll(filepath.Join(outside, id), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, id, "report.md"), []byte("OUTSIDE-SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	return state, id, outside
}

func TestReadJobReport(t *testing.T) {
	state, id, _ := reportFixture(t)
	p := filepath.Join(state, "jobs", id, "report.md")
	if _, _, err := ReadJobReport(state, id, 10); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing report: %v, want ErrNotExist", err)
	}
	if err := os.WriteFile(p, []byte("## Summary\nok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	text, truncated, err := ReadJobReport(state, id, 1000)
	if err != nil || text != "## Summary\nok\n" || truncated {
		t.Fatalf("report %q %v %v", text, truncated, err)
	}
	text, truncated, err = ReadJobReport(state, id, 4)
	if err != nil || text != "## S" || !truncated {
		t.Fatalf("capped report %q %v %v", text, truncated, err)
	}
	for _, bad := range []string{"", ".", "..", "../x", "a/b"} {
		if _, _, err := ReadJobReport(state, bad, 10); err == nil {
			t.Errorf("job id %q accepted", bad)
		}
	}
}

// TestReadJobReportContainment: a job may replace anything inside its job dir, and a compromised
// state could swap the jobs dir; neither a symlinked directory nor a hard link leads outside.
func TestReadJobReportContainment(t *testing.T) {
	t.Run("symlinked job dir", func(t *testing.T) {
		state, id, outside := reportFixture(t)
		dir := filepath.Join(state, "jobs", id)
		if err := os.Remove(dir); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(outside, id), dir); err != nil {
			t.Fatal(err)
		}
		assertRefused(t, state, id)
	})
	t.Run("symlinked jobs dir", func(t *testing.T) {
		state, id, outside := reportFixture(t)
		if err := os.RemoveAll(filepath.Join(state, "jobs")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(state, "jobs")); err != nil {
			t.Fatal(err)
		}
		assertRefused(t, state, id)
	})
	t.Run("symlinked report", func(t *testing.T) {
		state, id, outside := reportFixture(t)
		if err := os.Symlink(filepath.Join(outside, id, "report.md"), filepath.Join(state, "jobs", id, "report.md")); err != nil {
			t.Fatal(err)
		}
		assertRefused(t, state, id)
	})
	t.Run("hard link", func(t *testing.T) {
		state, id, outside := reportFixture(t)
		if err := os.Link(filepath.Join(outside, id, "report.md"), filepath.Join(state, "jobs", id, "report.md")); err != nil {
			t.Skipf("hard link: %v", err)
		}
		assertRefused(t, state, id)
	})
	t.Run("other owner", func(t *testing.T) {
		state, id, _ := reportFixture(t)
		if err := os.WriteFile(filepath.Join(state, "jobs", id, "report.md"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		real := getuid
		getuid = func() int { return real() + 1 }
		t.Cleanup(func() { getuid = real })
		assertRefused(t, state, id)
	})
	t.Run("fifo", func(t *testing.T) {
		state, id, _ := reportFixture(t)
		if err := mkfifo(filepath.Join(state, "jobs", id, "report.md")); err != nil {
			t.Skipf("mkfifo: %v", err)
		}
		assertRefused(t, state, id)
	})
}

func assertRefused(t *testing.T, state, id string) {
	t.Helper()
	text, _, err := ReadJobReport(state, id, 1000)
	if strings.Contains(text, "OUTSIDE-SECRET") {
		t.Fatalf("read a file outside the job dir: %q", text)
	}
	if !errors.Is(err, errNotRegular) {
		t.Fatalf("err %v, want errNotRegular", err)
	}
}

func mkfifo(path string) error { return syscall.Mkfifo(path, 0o600) }
