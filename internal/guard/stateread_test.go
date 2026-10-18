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

// stateRefusal reports a refusal by the state-read rule (not by another check of the guard).
func stateRefusal(r string) bool {
	return strings.Contains(r, "agent-jobs state") || strings.Contains(r, "contains `..`")
}

// Fix loop 1 of PR #24: symlink traversal, wildcards above the state dir, command-specific cwd and
// recursive reads from an ancestor are refused by the state-read rule.
func TestStateReadBypassesRefused(t *testing.T) {
	_, own, cfg := stateReadSetup(t)
	if err := os.Symlink(own, filepath.Join(cfg.Dir, "up")); err != nil { // up/.. is jobs/, not own/
		t.Fatal(err)
	}
	for _, c := range []string{
		// symlinks: `..` after a symlink, or after a component the call may create as one
		"ln -s .. L && cat L/../k/report.md", "ln -s .. L; cat L/../k/report.md", "cat up/../k/new.md",
		"cat up/../k/report.md", "ln -s ~/.agent-jobs A && cat A/thrall/jobs.db", "ln -s ../../../.. A",
		"mkdir -p a/b && ln -s ../../../../k a/b", "ln -s ../../.. S", "ln -sf -t sub ../../../../k",
		"cp -s ~/.agent-jobs/thrall/jobs.db x", "rm -r sub && ln -s up sub && cat sub/../k/report.md",
		"cd up/.. && cat k/report.md", "cd -P up/.. && cat k/report.md",
		// wildcards at or above the state dir
		"cat ~/.agent-jobs/thr*/jobs.db", "cat ~/.agent-*/thrall/config.json", "ls ~/.agent-jobs/*",
		"cat ~/.agent-jobs/thrall/jobs/[k]/report.md", "cat ~/.agent-jobs/thrall/jobs/{j,k}/report.md",
		"cat ~/*/thrall/jobs.db", "cat ../../?/report.md", "cat ../*/../k/report.md", "cat ~/.agent-jobs/thrall/j*",
		// command-specific working directory
		"env -C .. cat ../k/report.md", "env --chdir=.. cat ../k/report.md", "env --chdir .. cat ../k/report.md",
		"env -C ../.. cat k/report.md", "env -C../.. cat k/report.md", "git -C ../.. diff --no-index k/report.md j/prompt.md",
		"git -C .. -C .. diff --no-index k/report.md j/prompt.md", "make -C ../.. -f k/report.md", "env -C $X cat jobs.db",
		"chroot / cat x", "make --directory=../.. -f k/report.md", "go -C ../.. vet ./k", "sudo -D ../.. cat k/report.md",
		// recursive reads from an ancestor of the state dir
		"grep -a -d recurse '' \"$HOME\"", "grep -d recurse x ~", "grep -drecurse x ~", "grep --directories=recurse x ~",
		"grep --directories recurse x ~", "grep --recur x ~", "grep --derefe x ~", "find \"$HOME\" -name report.md | xargs cat",
		"find ~ -type f", "find ~/.agent-jobs", "echo ~/.agent-jobs | xargs -n1 ls",
		"cp -r ~/.agent-jobs /tmp/x", "cp -a ~ /tmp/x", "tar czf /tmp/x.tgz ~/.agent-jobs", "rsync -a ~/ /tmp/x",
		"cd ~ && rg token", "cd ~/.agent-jobs && grep -r token", "ls -R ~", "env -C ~ rg token",
	} {
		if r := Check(c, cfg); !stateRefusal(r) {
			t.Errorf("%q: %q, want a state-read refusal", c, r)
		}
	}
}

// The tightened rule keeps ordinary job work readable: the job's own prompt and report, the worktree,
// go test, git diff, globs inside the job's folder, links inside the worktree.
func TestStateReadNoFalsePositives(t *testing.T) {
	_, own, cfg := stateReadSetup(t)
	if err := os.MkdirAll(filepath.Join(cfg.Dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{
		"cat ../prompt.md; cd ..", "cat ../prompt.md && cd .. && ls", "head ../report.md; cd .. && cat prompt.md",
		"go test -count=1 ./...", "go vet ./...", "git diff", "git diff --stat HEAD~1..HEAD", "git -C . status",
		"git log main...HEAD", "env -C .. cat prompt.md", "env -C sub cat ../main.go", "ls *.go", "cat *.md",
		"cat ../*.md", "ls ../../j/*", "cat " + own + "/*.md", "ln -s ../prompt.md p", "ln -s main.go m.go",
		"ln -s ../prompt.md sub", "grep -r x ..", "grep -d skip x *.go", "find . -name '*.go' | xargs grep x",
		"sed -e 's/a/b/' ../prompt.md", "cat ~/.bashrc", "ls ~/*.md", "cat sub/../main.go", "find .. -name '*.md'",
		"cp -r sub /tmp/x", "rg x ..", "tar czf /tmp/x.tgz .", "ls -R .", "grep -rn TODO --include='*.go' .",
	} {
		if r := Check(c, cfg); r != "" {
			t.Errorf("%q refused: %s", c, r)
		}
	}
}

// Glob tool patterns that can reach the state dir through a wildcard at or above it are refused.
func TestStateReadGlobWildcards(t *testing.T) {
	state, own, cfg := stateReadSetup(t)
	home := filepath.Dir(filepath.Dir(state))
	for _, c := range []struct{ path, pattern string }{
		{filepath.Dir(state), "thr*/jobs/*/report.md"},
		{home, ".agent-jobs/*/jobs.db"},
		{"", home + "/.agent-jobs/*/config.json"},
		{own, "../[k]/report.md"},
		{own, "../*/report.md"},
		{home, "*/thrall/jobs.db"},
		{state, "j*"},
		{filepath.Dir(state), "*"},
	} {
		if r := CheckFileRead("Glob", c.path, c.pattern, cfg); !stateRefusal(r) {
			t.Errorf("Glob %q in %q: %q, want a refusal", c.pattern, c.path, r)
		}
	}
	for _, c := range []struct{ path, pattern string }{
		{"", "**/*.go"}, {own, "*.md"}, {"", "../*.md"}, {home, "*.txt"}, {state + "/jobs/j", "**/*.md"},
	} {
		if r := CheckFileRead("Glob", c.path, c.pattern, cfg); r != "" {
			t.Errorf("Glob %q in %q refused: %s", c.pattern, c.path, r)
		}
	}
}

// A configured subagent log that is not <state>/subagents/<id>.log refuses every tool call instead of
// silently turning the state-read rule off.
func TestStateReadInvalidLayoutRefusesAll(t *testing.T) {
	for _, logPath := range []string{filepath.Join(t.TempDir(), "j.log"), "relative/subagents/j.log", "/x/subagents/.log"} {
		cfg := Config{SubagentLog: logPath, Dir: t.TempDir()}
		for _, in := range []map[string]any{
			{"tool_name": "Bash", "cwd": cfg.Dir, "tool_input": map[string]any{"command": "ls"}},
			{"tool_name": "Read", "cwd": cfg.Dir, "tool_input": map[string]any{"file_path": "/etc/hostname"}},
			{"tool_name": "Agent", "cwd": cfg.Dir, "tool_input": map[string]any{"prompt": "p"}},
		} {
			b, _ := json.Marshal(in)
			r, err := Decide(strings.NewReader(string(b)), cfg)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(r, "not <state>/subagents/<id>.log") {
				t.Errorf("log %q, %v: %q, want the layout refusal", logPath, in["tool_name"], r)
			}
		}
	}
}

// Fix loop 2 of PR #24: an absolute link directory (`ln -s . ~/L`, `-t ~`, `--target-directory=~`)
// is not joined with the working directory, and abbreviated recursive long options count.
func TestStateReadLoop2Refused(t *testing.T) {
	state, own, cfg := stateReadSetup(t)
	home := filepath.Dir(filepath.Dir(state))
	for _, c := range []string{
		// an absolute link directory next to the state: own/../k is another job's folder
		"ln -s -t " + own + " ../k", "ln -st " + own + " ../k", "ln -s --target-directory=" + own + " ../k",
		"ln -s --target-directory " + own + " ../k", "ln -s ../k " + own + "/L", "ln -s ../../k " + own + "/work",
		"ln -s . ~/L", "ln -s . ~/L; cat ~/L/.agent-jobs/thrall/jobs.db", "ln -s . " + home + "/L",
		"ln -s . ~/L && cat ~/L/.agent-jobs/thrall/jobs.db", "ln -s ./.agent-jobs ~/A",
		"ln -st ~ ./.agent-jobs", "ln -s -t ~ ./.agent-jobs", "ln -s -t " + home + " ./.agent-jobs",
		"ln -s --target-directory=" + home + " ./.agent-jobs", "ln -s --target-directory ~ ./.agent-jobs",
		"ln -s . " + home, "ln -s ./.agent-jobs/thrall/jobs.db ~/d",
		"ls --rec ~", "ls --recu ~", "ls --recursive=x ~", "cp --rec ~ /tmp/x", "cp --arch ~ /tmp/x",
		"grep --recurs x ~", "grep --rec x ~", "grep --dereference-rec x ~",
	} {
		if r := Check(c, cfg); !stateRefusal(r) {
			t.Errorf("%q: %q, want a state-read refusal", c, r)
		}
	}
	for _, c := range []string{"ln -s . sub2", "ln -s main.go /tmp/m.go", "ls --reverse ~", "ln -s -t " + own + " work/main.go", "cp --remove-destination a b"} {
		if r := Check(c, cfg); r != "" {
			t.Errorf("%q refused: %s", c, r)
		}
	}
}
