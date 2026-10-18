package guard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stateReadSetup is the agent state layout: $HOME/.agent-jobs/thrall with jobs.db, config, archive,
// the subagent logs, the job's own folder jobs/j (prompt, report, work) and another job's folder jobs/k.
func stateReadSetup(t *testing.T) (state, own string, cfg Config) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CDPATH", "")
	state = filepath.Join(home, ".agent-jobs", "thrall")
	own = filepath.Join(state, "jobs", "j")
	for _, d := range []string{filepath.Join(state, "subagents"), filepath.Join(own, "work"),
		filepath.Join(state, "jobs", "k"), filepath.Join(state, "archive", "2026-10")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"jobs.db", "config.json", "owner-rules.md", "subagents/j.log", "archive/2026-10/k.tar.gz",
		"jobs/k/report.md", "jobs/k/prompt.md", "jobs/j/prompt.md", "jobs/j/report.md"} {
		if err := os.WriteFile(filepath.Join(state, f), []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(state, "jobs.db"), filepath.Join(own, "work", "lnk")); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(state, "jobs.db")
	cfg = Config{StateDir: state, OwnDir: own, Dir: filepath.Join(own, "work"),
		Protected: []string{filepath.Join(state, "subagents"), db, db + "-wal", db + "-shm", db + "-journal"}}
	return state, own, cfg
}

// Every plain reader, and inline code, is refused on the agent state outside the job's own folder.
func TestStateReadShellReadersRefused(t *testing.T) {
	state, own, cfg := stateReadSetup(t)
	db := filepath.Join(state, "jobs.db")
	other := filepath.Join(state, "jobs", "k", "report.md")
	for _, c := range []string{
		"cat " + db, "grep -a x " + db, "head -c 100 " + db, "tail " + db, "less " + db, "more " + db,
		"od -c " + db, "xxd " + db, "hexdump -C " + db, "base64 " + db, "strings " + db, "cp " + db + " /tmp/x",
		"sqlite3 " + db + " .dump", "dd if=" + db + " of=/tmp/x", "cat " + other, "grep -r token " + state,
		"cat " + filepath.Join(state, "config.json"), "cat " + filepath.Join(state, "owner-rules.md"),
		"tar tzf " + filepath.Join(state, "archive", "2026-10", "k.tar.gz"), "cat " + filepath.Join(state, "subagents", "j.log"),
		"ls " + state, "ls ../..", "cat ../../k/report.md", "cat ../../../jobs.db", "cat ~/.agent-jobs/thrall/jobs.db",
		"cat $HOME/.agent-jobs/thrall/jobs.db", "cat ${HOME}/.agent-jobs/thrall/config.json", "cat < " + db,
		"x=$(< " + db + ")", "< " + db + " wc -c", "while read l; do echo $l; done < " + db,
		"cd ../.. && cat k/report.md", "cd " + state + " && cat jobs.db", "cd $X && cat jobs.db",
		"cat lnk", "grep -a x lnk", "python3 -c \"print(open('" + db + "').read())\"",
		"python3 -c \"print(open('../../../jobs.db').read())\"", "F=" + db + "; cat \"$F\"",
		"python3 - <<'EOF'\nprint(open('" + other + "').read())\nEOF", "rg token ~/.agent-jobs", "grep -rn token ~",
		"grep -R token " + filepath.Dir(state), "cat " + own + "/../k/report.md", "cat " + own + "/work/lnk",
		"cat " + state + "/jobs/*/report.md", "ls ../../j*", "find ~ -name report.md -exec cat {} +",
		"bash -c 'cat " + db + "'", "echo $(cat " + other + ")",
	} {
		if Check(c, cfg) == "" {
			t.Errorf("%q allowed", c)
		}
	}
}

// The job's own folder (prompt, report, work) stays readable; ordinary job work is not affected.
func TestStateReadOwnDirAllowed(t *testing.T) {
	_, own, cfg := stateReadSetup(t)
	for _, c := range []string{
		"cat ../prompt.md", "cat " + filepath.Join(own, "prompt.md"), "cat " + filepath.Join(own, "report.md"),
		"head ../report.md", "ls ..", "ls " + own, "grep -r x ..", "grep -rn x .", "rg x", "cat ~/.bashrc", "ls ~",
		"cd .. && cat prompt.md", "go test ./...", "git status", "echo hi > out.txt", "cat out.txt",
		"python3 -c \"print(open('../prompt.md').read())\"", "cat ~/.agent-jobs/thrall/jobs/j/prompt.md",
		"find . -name '*.go'", "cp ../prompt.md notes.md", "git log --oneline",
	} {
		if r := Check(c, cfg); r != "" {
			t.Errorf("%q refused: %s", c, r)
		}
	}
}

// The Read, Grep and Glob tools are refused on the agent state, allowed in the job's own folder.
func TestStateReadTools(t *testing.T) {
	state, own, cfg := stateReadSetup(t)
	call := func(tool string, input map[string]any) string {
		t.Helper()
		b, err := json.Marshal(map[string]any{"tool_name": tool, "cwd": cfg.Dir, "tool_input": input})
		if err != nil {
			t.Fatal(err)
		}
		r, err := Decide(strings.NewReader(string(b)), cfg)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	for _, c := range []struct {
		tool  string
		input map[string]any
	}{
		{"Read", map[string]any{"file_path": filepath.Join(state, "jobs.db")}},
		{"Read", map[string]any{"file_path": filepath.Join(state, "config.json")}},
		{"Read", map[string]any{"file_path": filepath.Join(state, "jobs", "k", "report.md")}},
		{"Read", map[string]any{"file_path": filepath.Join(state, "archive", "2026-10", "k.tar.gz")}},
		{"Read", map[string]any{"file_path": "../../k/report.md"}},
		{"Read", map[string]any{"file_path": filepath.Join(cfg.Dir, "lnk")}},
		{"Read", map[string]any{"file_path": own + "/../k/report.md"}},
		{"Grep", map[string]any{"pattern": "token", "path": state}},
		{"Grep", map[string]any{"pattern": "token", "path": filepath.Join(state, "jobs.db")}},
		{"Grep", map[string]any{"pattern": "token", "path": filepath.Dir(state)}},
		{"Grep", map[string]any{"pattern": "token", "path": "../.."}},
		{"Glob", map[string]any{"pattern": "*", "path": filepath.Join(state, "jobs")}},
		{"Glob", map[string]any{"pattern": state + "/**/report.md"}},
		{"Glob", map[string]any{"pattern": "../*/report.md", "path": own}},
		{"Glob", map[string]any{"pattern": "**/report.md", "path": filepath.Dir(state)}},
	} {
		if call(c.tool, c.input) == "" {
			t.Errorf("%s %v allowed", c.tool, c.input)
		}
	}
	for _, c := range []struct {
		tool  string
		input map[string]any
	}{
		{"Read", map[string]any{"file_path": filepath.Join(own, "prompt.md")}},
		{"Read", map[string]any{"file_path": filepath.Join(own, "report.md")}},
		{"Read", map[string]any{"file_path": "../prompt.md"}},
		{"Read", map[string]any{"file_path": filepath.Join(cfg.Dir, "main.go")}},
		{"Grep", map[string]any{"pattern": "x"}},
		{"Grep", map[string]any{"pattern": "x", "path": own}},
		{"Glob", map[string]any{"pattern": "**/*.go"}},
		{"Glob", map[string]any{"pattern": "*.md", "path": own}},
	} {
		if r := call(c.tool, c.input); r != "" {
			t.Errorf("%s %v refused: %s", c.tool, c.input, r)
		}
	}
}

// The hook derives the state dir and the job's own folder from the job's subagent log.
func TestStateReadFromSubagentLog(t *testing.T) {
	state, own, cfg := stateReadSetup(t)
	cfg.StateDir, cfg.OwnDir = "", ""
	cfg.SubagentLog = filepath.Join(state, "subagents", "j.log")
	if s, o, ok := StateLayout(cfg.SubagentLog); !ok || s != state || o != own {
		t.Fatalf("StateLayout = %q %q %v", s, o, ok)
	}
	if _, _, ok := StateLayout(filepath.Join(t.TempDir(), "j.log")); ok {
		t.Fatal("a log outside <state>/subagents gave a layout")
	}
	read := func(p string) string {
		b, _ := json.Marshal(map[string]any{"tool_name": "Read", "cwd": cfg.Dir, "tool_input": map[string]any{"file_path": p}})
		r, err := Decide(strings.NewReader(string(b)), cfg)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if read(filepath.Join(state, "jobs.db")) == "" {
		t.Error("Read of jobs.db allowed")
	}
	if r := read(filepath.Join(own, "prompt.md")); r != "" {
		t.Errorf("Read of the own prompt refused: %s", r)
	}
	b, _ := json.Marshal(map[string]any{"tool_name": "Bash", "cwd": cfg.Dir, "tool_input": map[string]any{"command": "cat ../../k/report.md"}})
	if r, err := Decide(strings.NewReader(string(b)), cfg); err != nil || r == "" {
		t.Errorf("cat of another job's report: %q %v", r, err)
	}
}

// A bare redirection (no command word) is still seen: `> f` truncates f, `< f` reads it.
func TestBareRedirection(t *testing.T) {
	state, _, cfg := stateReadSetup(t)
	for _, c := range []string{"> .git/config", "x=$(> .git/config)", "> " + filepath.Join(state, "jobs.db"), "< " + filepath.Join(state, "jobs.db")} {
		if Check(c, cfg) == "" {
			t.Errorf("%q allowed", c)
		}
	}
}
