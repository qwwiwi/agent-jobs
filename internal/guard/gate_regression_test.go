package guard

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func gate72Decision(t *testing.T, cfg Config, tool, p string) string {
	t.Helper()
	input := map[string]string{"file_path": p}
	if tool == "Bash" {
		input = map[string]string{"command": p}
	}
	b, _ := json.Marshal(map[string]any{"tool_name": tool, "cwd": cfg.Dir, "tool_input": input})
	reason, err := Decide(strings.NewReader(string(b)), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return reason
}

func TestGate72FdJoinedExec(t *testing.T) {
	root := t.TempDir()
	skills := filepath.Join(root, "skills")
	work := filepath.Join(root, "work")
	for _, d := range []string{skills, work} {
		if err := os.Mkdir(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	victim := filepath.Join(skills, "victim")
	if err := os.WriteFile(victim, []byte("protected skill\n"), 0400); err != nil {
		t.Fatal(err)
	}
	old := time.Unix(1, 0)
	if err := os.Chtimes(victim, old, old); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Dir: work, Protected: []string{skills}}
	for _, opt := range []string{"-xtouch", "-Xtouch", "-Hxtouch"} {
		cmd := "fd victim " + root + " " + opt
		reason := gate72Decision(t, cfg, "Bash", cmd)
		t.Logf("%s => %q", cmd, reason)
		if reason == "" {
			t.Errorf("REGRESSION: fd exec spelling %s bypasses protected parent", opt)
		}
	}
	// Validate the command's real effect on this throwaway file with official fd 10.2.0.
	if fd := os.Getenv("GATE_FD"); fd != "" {
		c := exec.Command(fd, "victim", root, "-xtouch")
		c.Dir = work
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("fd: %v: %s", err, out)
		}
		fi, err := os.Stat(victim)
		if err != nil {
			t.Fatal(err)
		}
		if fi.ModTime().Equal(old) {
			t.Fatal("fd did not change protected fixture")
		}
		t.Log("actual fd -xtouch changed the protected file's timestamp")
	}
}

func TestGate72NoReadResolutionOverflow(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"d", "real", "work"} {
		if err := os.Mkdir(filepath.Join(root, d), 0700); err != nil {
			t.Fatal(err)
		}
	}
	lock := filepath.Join(root, "real", "a.lock")
	if err := os.WriteFile(lock, []byte("lock\n"), 0600); err != nil {
		t.Fatal(err)
	}
	pad := strings.Repeat("d/../", 600)
	for i := 1; i <= 4; i++ {
		target := "real"
		if i < 4 {
			target = fmt.Sprintf("l%d", i+1)
		}
		if err := os.Symlink(pad+target, filepath.Join(root, fmt.Sprintf("l%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(root, "l1")
	a, err := os.Stat(filepath.Join(alias, "a.lock"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.Stat(lock)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(a, b) {
		t.Fatal("alias does not reach the real lock")
	}
	t.Log("kernel resolves all four links: 4804 symlink-target components")
	t.Run("read_target", func(t *testing.T) {
		cfg := Config{Dir: filepath.Join(root, "work"), NoRead: []string{filepath.Join(root, "real")}}
		reason := gate72Decision(t, cfg, "Read", filepath.Join(alias, "a.lock"))
		t.Logf("Read l1/a.lock, NoRead=real => %q", reason)
		if reason == "" {
			t.Error("REGRESSION: file read resolution overflow allowed")
		}
	})
	t.Run("write_root", func(t *testing.T) {
		cfg := Config{Dir: filepath.Join(root, "work"), NoRead: []string{alias}}
		reason := gate72Decision(t, cfg, "Write", lock)
		t.Logf("Write real/a.lock, NoRead=l1 => %q", reason)
		if reason == "" {
			t.Error("REGRESSION: NoRead root resolution overflow allowed a file write")
		}
	})
}
