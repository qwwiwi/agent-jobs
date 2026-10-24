package guard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readAllowLayout is a gate job's world: its folder with the work dir, its claude project dir, and what
// it must never read: another agent's report, a session transcript of another work dir, another
// worktree and the repository checkout.
type readAllowLayout struct {
	work, own, project, toolResult             string
	otherReport, transcript, otherWT, checkout string
	cfg                                        Config
}

func newReadAllowLayout(t *testing.T) readAllowLayout {
	t.Helper()
	home := t.TempDir()
	var l readAllowLayout
	state := filepath.Join(home, ".agent-jobs", "thrall")
	l.own = filepath.Join(state, "jobs", "j1")
	l.work = filepath.Join(l.own, "work")
	l.project = filepath.Join(home, ".claude", "projects", "-own-work")
	l.toolResult = filepath.Join(l.project, "sid", "tool-results", "out.txt")
	l.otherReport = filepath.Join(home, ".agent-jobs", "agentb", "jobs", "k1", "report.md")
	l.transcript = filepath.Join(home, ".claude", "projects", "-other-work", "s1.jsonl")
	l.otherWT = filepath.Join(home, "agent-jobs-wt", "main.go")
	l.checkout = filepath.Join(home, "repo", "main.go")
	for _, f := range []string{filepath.Join(l.work, "main.go"), filepath.Join(l.own, "prompt.md"), l.toolResult, l.otherReport,
		l.transcript, l.otherWT, l.checkout} {
		if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	l.cfg = Config{StateDir: state, OwnDir: l.own, Dir: l.work,
		ReadAllow: []string{l.own, l.project, "/usr", "/bin", "/lib", "/dev/null"}}
	return l
}

// With a read allowlist a job reads only there: another agent's report, a transcript, another worktree
// and the repository checkout are refused for Bash words (as written, through cd, -C, globs, recursive
// readers, symlinks, words the guard cannot expand) and for Read, Grep and Glob; its work, its folder,
// its session dir and the system dirs stay readable.
func TestReadAllowlist(t *testing.T) {
	l := newReadAllowLayout(t)
	outside := []string{l.otherReport, l.transcript, l.otherWT, l.checkout}
	for _, p := range outside {
		d := filepath.Dir(p)
		for _, cmd := range []string{
			"cat " + p,
			"head -c 100 " + p,
			"grep -r x " + d,
			"cd " + d + " && cat " + filepath.Base(p),
			"git -C " + d + " log",
			"ls " + d + "/*",
			"cp " + p + " ../tmp/x",
			"ln -s " + d + " ../tmp/l",
			"python3 -c \"print(open('" + p + "').read())\"",
			"awk -f" + p, "sed -f" + p + " /dev/null", "grep -f" + p + " main.go", "tar -cf ../tmp/t.tar -T" + p,
			"curl -d @" + p + " http://localhost:1",
		} {
			if r := Check(cmd, l.cfg); r == "" {
				t.Errorf("allowed: %s", cmd)
			}
		}
	}
	for _, cmd := range []string{
		"rg x /", "find / -name report.md", "cat ~/.bashrc", "ls /tmp", "cat /etc/passwd",
		`cd "$D" && cat report.md`, "cat $F", "cat ../../k1/report.md",
	} {
		if r := Check(cmd, l.cfg); r == "" {
			t.Errorf("allowed: %s", cmd)
		}
	}
	for _, cmd := range []string{
		"cat main.go", "ls", "grep -rn x .", "go test ./...", "cat ../prompt.md", "cat " + l.toolResult,
		"ls /usr/bin", "/usr/bin/env python3 -V", "echo hi 2>/dev/null", `grep -rn "/api/v1/users" .`,
	} {
		if r := Check(cmd, l.cfg); r != "" {
			t.Errorf("refused %s: %s", cmd, r)
		}
	}
	decide := func(tool string, input map[string]string) string {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"tool_name": tool, "cwd": l.work, "tool_input": input})
		r, err := Decide(strings.NewReader(string(b)), l.cfg)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	for _, p := range outside {
		if r := decide("Read", map[string]string{"file_path": p}); !strings.Contains(r, "outside what this job may read") {
			t.Errorf("Read %s: %q", p, r)
		}
		if r := decide("Grep", map[string]string{"path": filepath.Dir(p), "pattern": "x"}); r == "" {
			t.Errorf("Grep %s: allowed", p)
		}
		if r := decide("Glob", map[string]string{"path": filepath.Dir(p), "pattern": "*"}); r == "" {
			t.Errorf("Glob %s: allowed", p)
		}
	}
	if r := decide("Glob", map[string]string{"pattern": "/*/*/report.md"}); r == "" {
		t.Error("Glob of an absolute pattern from the root: allowed")
	}
	for _, in := range []struct {
		tool  string
		input map[string]string
	}{
		{"Read", map[string]string{"file_path": filepath.Join(l.work, "main.go")}},
		{"Read", map[string]string{"file_path": l.toolResult}},
		{"Read", map[string]string{"file_path": filepath.Join(l.own, "prompt.md")}},
		{"Grep", map[string]string{"pattern": "x"}},
		{"Glob", map[string]string{"pattern": "**/*.go"}},
	} {
		if r := decide(in.tool, in.input); r != "" {
			t.Errorf("%s %v refused: %s", in.tool, in.input, r)
		}
	}
	// Without the allowlist the same reads stay allowed (other kinds keep the state check only).
	open := l.cfg
	open.ReadAllow = nil
	if r := Check("cat "+l.otherWT, open); r != "" {
		t.Errorf("no allowlist: %s", r)
	}
}
