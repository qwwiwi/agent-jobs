package guard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A NoRead directory (the shared checklist locks) is out of a job's reach: no Bash word, redirection
// (`exec 9<lock; flock 9`) or recursive read of an ancestor reaches it, nor Read, Grep, Glob, Write or
// Edit; other paths stay usable.
func TestNoReadDirRefused(t *testing.T) {
	home := t.TempDir()
	locks := filepath.Join(home, "checklist-locks")
	if err := os.Mkdir(locks, 0o700); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(locks, strings.Repeat("ab", 32)+".lock")
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(home, "test")
	own := filepath.Join(state, "jobs", "j1")
	work := filepath.Join(own, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := Config{StateDir: state, OwnDir: own, Dir: work, NoRead: []string{locks}}
	for _, cmd := range []string{
		"exec 9<" + lock + "; flock 9; sleep 600",
		"exec 9>" + lock + "; flock 9; sleep 600",
		"flock " + lock + " sleep 600",
		"cat " + lock,
		"ls " + locks,
		"cd " + locks + " && flock x.lock true",
		"python3 -c \"import fcntl; fcntl.flock(open('" + lock + "'), 2)\"",
		"rg x " + home,
		"ls " + locks + "/*",
	} {
		if r := Check(cmd, cfg); r == "" {
			t.Errorf("allowed: %s", cmd)
		}
	}
	for _, cmd := range []string{"ls " + work, "echo ok > " + filepath.Join(work, "x.txt")} {
		if r := Check(cmd, cfg); r != "" {
			t.Errorf("refused %s: %s", cmd, r)
		}
	}
	for _, tc := range []struct {
		tool  string
		input map[string]string
	}{
		{"Read", map[string]string{"file_path": lock}},
		{"Grep", map[string]string{"path": home}},
		{"Glob", map[string]string{"path": locks, "pattern": "*.lock"}},
		{"Write", map[string]string{"file_path": lock}},
		{"Edit", map[string]string{"file_path": filepath.Join(locks, "new.lock")}},
	} {
		b, _ := json.Marshal(map[string]any{"tool_name": tc.tool, "cwd": work, "tool_input": tc.input})
		if r, err := Decide(strings.NewReader(string(b)), cfg); err != nil || r == "" {
			t.Errorf("%s %v: %q %v, want refused", tc.tool, tc.input, r, err)
		}
	}
	// Without a state dir the NoRead check holds on its own.
	if r := Check("cat "+lock, Config{Dir: work, NoRead: []string{locks}}); r == "" {
		t.Error("NoRead without a state dir: allowed")
	}
}
