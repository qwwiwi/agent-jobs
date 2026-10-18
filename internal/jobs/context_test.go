package jobs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	agentjobs "github.com/qwwiwi/agent-jobs"
	"github.com/qwwiwi/agent-jobs/internal/guard"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// hookCommand returns the guard hook command as written into a job's settings.json.
func hookCommand(t *testing.T, dir string) (string, []string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Permissions struct {
			Deny []string `json:"deny"`
		} `json:"permissions"`
		Hooks struct {
			PreToolUse []struct {
				Hooks []struct {
					Command string `json:"command"`
				} `json:"hooks"`
			} `json:"PreToolUse"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	return s.Hooks.PreToolUse[0].Hooks[0].Command, s.Permissions.Deny
}

func runHook(t *testing.T, command string, payload map[string]any) int {
	t.Helper()
	in, _ := json.Marshal(payload)
	c := exec.Command("sh", "-c", command)
	c.Stdin = bytes.NewReader(in)
	var ee *exec.ExitError
	if err := c.Run(); errors.As(err, &ee) {
		return ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return 0
}

// Owner 2026-10-17: jobs may use subagents, at most three per job, inside the job's own limits; no
// nested jobs. The brief states it, the subagent tool is not denied, and the guard hook as written
// into settings.json holds the cap and keeps refusing job control.
func TestJobBriefAndLaunchCarrySubagentRules(t *testing.T) {
	m := newManager(t)
	// Two running jobs: the counter lives in the running row.
	j := enqueue(t, m, "x FAKE_SLEEP=30", nil)
	j2 := enqueue(t, m, "y FAKE_SLEEP=30", nil)
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Cancel(j.ID); _ = m.Cancel(j2.ID) })
	for _, id := range []string{j.ID, j2.ID} {
		if got := get(t, m, id); got.Status != store.StatusRunning {
			t.Fatalf("job %s: %s %q", id, got.Status, got.Error)
		}
	}
	dir := m.JobDir(j.ID)
	prompt, _ := os.ReadFile(filepath.Join(dir, "prompt.md"))
	for _, s := range []string{"## Subagents", "at most three subagents", "No nested jobs", "budget", "no `isolation`"} {
		if !strings.Contains(string(prompt), s) {
			t.Fatalf("prompt.md lacks %q", s)
		}
	}
	// The subagent rules come right after the preamble, before the task.
	if strings.Index(string(prompt), "## Subagents") > strings.Index(string(prompt), "## Task:") {
		t.Fatal("subagent rules must precede the task")
	}
	cmd, deny := hookCommand(t, dir)
	for _, d := range deny {
		if d == "Agent" || d == "Task" || strings.HasPrefix(d, "Agent(") || strings.HasPrefix(d, "Task(") {
			t.Fatalf("subagent tool denied (%s): jobs may use subagents", d)
		}
	}
	if !strings.Contains(cmd, "--subagent-log '"+m.SubagentLogPath(j.ID)+"'") {
		t.Fatalf("guard hook lacks the job's subagent log: %s", cmd)
	}
	if within := strings.HasPrefix(m.SubagentLogPath(j.ID), dir+string(filepath.Separator)); within {
		t.Fatal("the subagent log must lie outside the job directory")
	}
	// The guard derives the state dir and the job's folder from the subagent log: the job database
	// is unreadable through the hook, the job's own prompt stays readable.
	if s, o, ok := guard.StateLayout(m.SubagentLogPath(j.ID)); !ok || s != m.StateDir || o != dir {
		t.Fatalf("guard.StateLayout = %q %q %v, want %q %q", s, o, ok, m.StateDir, dir)
	}
	readDB := map[string]any{"tool_name": "Read", "tool_input": map[string]any{"file_path": filepath.Join(m.StateDir, "jobs.db")}}
	if code := runHook(t, cmd, readDB); code != 2 {
		t.Fatalf("Read of jobs.db: exit %d, want 2", code)
	}
	readPrompt := map[string]any{"tool_name": "Read", "tool_input": map[string]any{"file_path": filepath.Join(dir, "prompt.md")}}
	if code := runHook(t, cmd, readPrompt); code != 0 {
		t.Fatalf("Read of the job's own prompt: exit %d, want 0", code)
	}
	agent := map[string]any{"tool_name": "Agent", "tool_input": map[string]any{"description": "d", "prompt": "p"}}
	for i := 1; i <= 3; i++ {
		if code := runHook(t, cmd, agent); code != 0 {
			t.Fatalf("subagent %d: exit %d, want 0", i, code)
		}
	}
	if code := runHook(t, cmd, agent); code != 2 {
		t.Fatalf("fourth subagent: exit %d, want 2", code)
	}
	if n, err := m.Store.SubagentCount(j.ID); err != nil || n != 3 {
		t.Fatalf("store counter = %d, %v; want 3", n, err)
	}
	// H1: shortening the log (as a script the job runs could) does not reset the cap: the store's
	// monotonic counter still says 3, and the shortened log is reported as tampering.
	if err := os.WriteFile(m.SubagentLogPath(j.ID), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if code := runHook(t, cmd, agent); code != 2 {
		t.Fatalf("subagent after truncating the log: exit %d, want 2", code)
	}
	// H1: the job cannot write its subagent log or the job database with file tools or Bash.
	logPath := m.SubagentLogPath(j.ID)
	db := filepath.Join(m.StateDir, "jobs.db")
	for _, tool := range []string{"Write", "Edit", "MultiEdit", "NotebookEdit"} {
		key := "file_path"
		if tool == "NotebookEdit" {
			key = "notebook_path"
		}
		for _, p := range []string{logPath, db} {
			if code := runHook(t, cmd, map[string]any{"tool_name": tool, "cwd": dir, "tool_input": map[string]any{key: p}}); code != 2 {
				t.Errorf("%s %s: exit %d, want 2", tool, p, code)
			}
		}
	}
	for _, c := range []string{
		": > " + shellQuote(logPath), "rm -f " + shellQuote(logPath), "truncate -s 0 " + shellQuote(logPath),
		"sqlite3 " + shellQuote(db) + " 'update jobs set subagents=0'", "cp /dev/null " + shellQuote(db),
	} {
		if code := runHook(t, cmd, map[string]any{"tool_name": "Bash", "cwd": dir, "tool_input": map[string]any{"command": c}}); code != 2 {
			t.Errorf("%q: exit %d, want 2", c, code)
		}
	}
	// A second job has its own count.
	cmd2, _ := hookCommand(t, m.JobDir(j2.ID))
	if code := runHook(t, cmd2, agent); code != 0 {
		t.Fatalf("other job's first subagent: exit %d, want 0", code)
	}
	// No nested jobs: the hook refuses agent-jobs control from the job or any of its subagents ...
	for _, c := range []string{
		"agent-jobs start --agent test --title t --model opus --brief b",
		shellQuote(selfBin) + " start --agent test --title t --model opus --brief b",
	} {
		if code := runHook(t, cmd, map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": c}}); code != 2 {
			t.Fatalf("%q: exit %d, want 2", c, code)
		}
	}
	// ... and the binary itself refuses under the job marker every subagent inherits.
	start := exec.Command(selfBin, "start", "--agent", "test", "--title", "t", "--model", "opus", "--brief", "b")
	start.Env = append(os.Environ(), JobEnvVar+"="+j.ID)
	out, err := start.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "refused") {
		t.Fatalf("nested start under the marker: err=%v out=%s", err, out)
	}
	// The job's MCP server is the job role, which has no job tools.
	mcp, _ := os.ReadFile(filepath.Join(dir, "mcp.json"))
	if !strings.Contains(string(mcp), `"job"`) || strings.Contains(string(mcp), "orchestrator") {
		t.Fatalf("mcp.json: %s", mcp)
	}
}

func TestExtractProfile(t *testing.T) {
	ok := "intro\n" + ProfileBegin + "\n- rule one\n" + ProfileEnd + "\nprivate\n" + ProfileBegin + "- rule two" + ProfileEnd
	got, err := ExtractProfile(ok)
	if err != nil || len(got) != 2 || got[0] != "- rule one" || got[1] != "- rule two" {
		t.Fatalf("ExtractProfile = %q, %v", got, err)
	}
	for name, src := range map[string]string{
		"none":     "just rules\n",
		"unclosed": ProfileBegin + "\n- x\n",
		"stray":    "- x\n" + ProfileEnd,
		"nested":   ProfileBegin + "a" + ProfileBegin + "b" + ProfileEnd,
		"empty":    ProfileBegin + "\n \n" + ProfileEnd,
	} {
		if _, err := ExtractProfile(src); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The job profile is the reference's owner-rules file: at most 8 KB. Generation refuses anything
// larger and writes nothing, and the shipped template fits.
func TestProfileAtMost8KB(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "rules.md")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(src, []byte("# Agent rules\n"+ProfileBegin+"\n"+body+"\n"+ProfileEnd+"\nnot for jobs\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("- x")
	small, err := GenerateProfile([]string{src})
	if err != nil {
		t.Fatal(err)
	}
	overhead := len(small) - len("- x")
	write(strings.Repeat("y", MaxOwnerRulesBytes-overhead))
	exact, err := GenerateProfile([]string{src})
	if err != nil || len(exact) != MaxOwnerRulesBytes {
		t.Fatalf("profile of exactly %d bytes: len %d, %v", MaxOwnerRulesBytes, len(exact), err)
	}
	write(strings.Repeat("y", MaxOwnerRulesBytes-overhead+1))
	if _, err := GenerateProfile([]string{src}); err == nil || !strings.Contains(err.Error(), "limit is 8192") {
		t.Fatalf("profile over 8 KB: %v", err)
	}
	m := newManager(t)
	before, _ := os.ReadFile(m.Cfg.OwnerRulesPath)
	if _, _, err := m.WriteProfile([]string{src}, true); err == nil {
		t.Fatal("WriteProfile wrote an oversized profile")
	}
	if after, _ := os.ReadFile(m.Cfg.OwnerRulesPath); !bytes.Equal(before, after) {
		t.Fatal("a refused profile changed the owner-rules file")
	}
	tpl, err := agentjobs.Templates.ReadFile("rules/owner.md")
	if err != nil || len(tpl) > MaxOwnerRulesBytes {
		t.Fatalf("shipped owner-rules template: %d bytes, %v", len(tpl), err)
	}
}

func TestWriteProfileIntoBrief(t *testing.T) {
	m := newManager(t)
	src := filepath.Join(t.TempDir(), "CLAUDE.md")
	if err := os.WriteFile(src, []byte("secret owner notes\n"+ProfileBegin+"\n- PROFILE_RULE_MARKER: commits in English\n"+ProfileEnd+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// newManager wrote a hand-made owner-rules file: replaced only with force.
	if _, _, err := m.WriteProfile([]string{src}, false); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("hand-written rules replaced without force: %v", err)
	}
	if _, _, err := m.WriteProfile([]string{src}, true); err != nil {
		t.Fatal(err)
	}
	// A generated profile and the untouched template may be regenerated without force.
	if _, _, err := m.WriteProfile([]string{src}, false); err != nil {
		t.Fatalf("regenerate: %v", err)
	}
	tpl, _ := agentjobs.Templates.ReadFile("rules/owner.md")
	if err := os.WriteFile(m.Cfg.OwnerRulesPath, tpl, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.WriteProfile([]string{src}, false); err != nil {
		t.Fatalf("replace the shipped template: %v", err)
	}
	j := runToEnd(t, m, enqueue(t, m, "x", nil).ID)
	prompt, _ := os.ReadFile(filepath.Join(m.JobDir(j.ID), "prompt.md"))
	if !strings.Contains(string(prompt), "PROFILE_RULE_MARKER") || strings.Contains(string(prompt), "secret owner notes") {
		t.Fatalf("brief must carry the marked section only:\n%s", prompt)
	}
	if fi, _ := os.Stat(m.Cfg.OwnerRulesPath); fi.Mode().Perm() != 0o600 {
		t.Fatalf("profile mode %v, want 0600", fi.Mode().Perm())
	}
}

// Network and local remotes are different identities; local paths keep their case and resolve
// against the repository; host case and URL spelling do not matter for network remotes.
func TestRemoteIdentity(t *testing.T) {
	base := "/work/repo"
	for in, want := range map[string]string{
		"https://github.com/Owner/Repo.git":    "net:github.com/Owner/Repo",
		"https://user@GitHub.com:443/O/R/":     "net:github.com/O/R",
		"git@github.com:O/R.git":               "net:github.com/O/R",
		"ssh://git@github.com:22/O/R":          "net:github.com/O/R",
		"/srv/git/Repo.git":                    "local:/srv/git/Repo.git",
		"/srv/git/repo.git":                    "local:/srv/git/repo.git",
		"file:///srv/git/Repo.git":             "local:/srv/git/Repo.git",
		"github.com/o/r.git":                   "local:/work/repo/github.com/o/r.git",
		"../up":                                "local:/work/up",
		"  https://gitlab.example.com/g/s/p  ": "net:gitlab.example.com/g/s/p",
	} {
		if got, err := RemoteIdentity(in, base); err != nil || got != want {
			t.Errorf("RemoteIdentity(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	net, _ := RemoteIdentity("https://github.com/o/r.git", base)
	loc, _ := RemoteIdentity("github.com/o/r.git", base)
	if net == loc {
		t.Fatal("a relative local remote collides with the network repository")
	}
	a, _ := RemoteIdentity("/srv/Repo.git", base)
	b, _ := RemoteIdentity("/srv/repo.git", base)
	if a == b {
		t.Fatal("local paths that differ in case collide")
	}
	for _, bad := range []string{"", "https://x.com/o/r\nrepo: /etc", "git@h:o/r\r"} {
		if _, err := RemoteIdentity(bad, base); err == nil {
			t.Errorf("RemoteIdentity(%q) accepted", bad)
		}
	}
	// Card side: the host/owner/name shorthand is a network repository.
	for in, want := range map[string]string{
		"github.com/qwwiwi/agent-jobs":         "net:github.com/qwwiwi/agent-jobs",
		"https://github.com/qwwiwi/agent-jobs": "net:github.com/qwwiwi/agent-jobs",
		"/srv/git/Repo.git":                    "local:/srv/git/Repo.git",
	} {
		if got, err := CardRemoteIdentity(in); err != nil || got != want {
			t.Errorf("CardRemoteIdentity(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"relative/path", "noslash", "github.com/o/r\u2028x"} {
		if _, err := CardRemoteIdentity(bad); err == nil {
			t.Errorf("CardRemoteIdentity(%q) accepted", bad)
		}
	}
}

func TestParseCard(t *testing.T) {
	c, err := ParseCard("p", []byte("---\nrepo: /src/p\nremote: git@github.com:O/P.git\n---\n# P\n\n## Stack\n- Go\n"))
	if err != nil || len(c.Repos) != 1 || c.Remotes[0] != "net:github.com/O/P" || !strings.HasPrefix(c.Body, "# P") {
		t.Fatalf("ParseCard = %+v, %v", c, err)
	}
	for name, s := range map[string]string{
		"no front matter": "# P\n",
		"unclosed":        "---\nrepo: /x\n# P\n",
		"no keys":         "---\n---\n# P\n",
		"relative repo":   "---\nrepo: src/p\n---\n# P\n",
		"unknown key":     "---\nrepo: /x\nowner: me\n---\n# P\n",
		"empty body":      "---\nrepo: /x\n---\n\n",
		"CR in selector":  "---\nrepo: /x\rremote: github.com/e/vil\n---\n# P\n",
		"tab in selector": "---\nrepo:\t/x\n---\n# P\n",
		"bidi in remote":  "---\nremote: github.com/o/\u202er\n---\n# P\n",
	} {
		if _, err := ParseCard("p", []byte(s)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The shipped example and template are valid cards and short.
func TestShippedCards(t *testing.T) {
	b, err := agentjobs.Templates.ReadFile("cards/example-agent-jobs.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > MaxCardBytes {
		t.Fatalf("example card is %d bytes, limit %d", len(b), MaxCardBytes)
	}
	c, err := ParseCard("example-agent-jobs", b)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Matches("/nowhere", []string{mustIdentity(t, "https://github.com/qwwiwi/agent-jobs.git")}) {
		t.Fatal("example card does not match its own repository")
	}
	for _, sec := range []string{"## Stack", "## Check runner", "## Do not touch", "## Known traps"} {
		if !strings.Contains(c.Body, sec) {
			t.Errorf("example card lacks %s", sec)
		}
	}
}

func TestProjectCardsAttachedToMatchingRepo(t *testing.T) {
	m := newManager(t)
	repo := newRepo(t)
	gitT(t, repo, "remote", "add", "origin", "git@github.com:Org/Project.git")
	p, err := m.NewCard(repo, "project")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.NewCard(repo, "project"); err == nil {
		t.Fatal("NewCard overwrote an existing card")
	}
	card, _ := os.ReadFile(p)
	if !strings.Contains(string(card), "remote: github.com/Org/Project\n") || !strings.Contains(string(card), "## Known traps") {
		t.Fatalf("new card:\n%s", card)
	}
	if err := os.WriteFile(p, bytes.Replace(card, []byte("## Known traps"), []byte("## Known traps\n- CARD_MARKER_BY_REPO"), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	// A card that matches by remote only, and one for another project.
	other := filepath.Join(m.CardsDir(), "remote-only.md")
	if err := os.WriteFile(other, []byte("---\nremote: https://github.com/Org/Project\n---\n- CARD_MARKER_BY_REMOTE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.CardsDir(), "unrelated.md"), []byte("---\nremote: github.com/org/else\nrepo: /elsewhere\n---\n- CARD_MARKER_UNRELATED\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	j := runToEnd(t, m, enqueue(t, m, "x", func(r *StartRequest) { r.RepoPath = repo }).ID)
	if j.Status != store.StatusSucceeded {
		t.Fatalf("job %s: %q", j.Status, j.Error)
	}
	prompt, _ := os.ReadFile(filepath.Join(m.JobDir(j.ID), "prompt.md"))
	for _, s := range []string{"## Project card: project", "CARD_MARKER_BY_REPO", "## Project card: remote-only", "CARD_MARKER_BY_REMOTE"} {
		if !strings.Contains(string(prompt), s) {
			t.Fatalf("prompt.md lacks %q", s)
		}
	}
	if strings.Contains(string(prompt), "CARD_MARKER_UNRELATED") || strings.Contains(string(prompt), "remote: ") {
		t.Fatal("brief carries an unrelated card or a card's front matter")
	}
	// A job without a repository gets no cards.
	j = runToEnd(t, m, enqueue(t, m, "x", nil).ID)
	if prompt, _ := os.ReadFile(filepath.Join(m.JobDir(j.ID), "prompt.md")); strings.Contains(string(prompt), "Project card") {
		t.Fatal("job without a repository got a project card")
	}
}

// A broken or oversized card refuses the start, like unreadable owner rules: never skipped silently.
func TestBrokenOrOversizedCardRefusesStart(t *testing.T) {
	m := newManager(t)
	repo := newRepo(t)
	if err := os.MkdirAll(m.CardsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(m.CardsDir(), "bad.md")
	for name, body := range map[string]string{
		"broken":    "no front matter\n",
		"oversized": "---\nrepo: " + repo + "\n---\n" + strings.Repeat("z", MaxCardBytes),
	} {
		if err := os.WriteFile(bad, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		j := enqueue(t, m, "x", func(r *StartRequest) { r.RepoPath = repo })
		if err := m.Tick(); err != nil {
			t.Fatal(err)
		}
		if j = get(t, m, j.ID); j.Status != store.StatusFailed || !strings.Contains(j.Error, "refused to start: project card") {
			t.Fatalf("%s card: %s %q", name, j.Status, j.Error)
		}
	}
}

// The CLI for job context: profile (dry run and write), card new / list; all refused inside a job.
func TestContextCLI(t *testing.T) {
	home := t.TempDir()
	repo := newRepo(t)
	src := filepath.Join(t.TempDir(), "rules.md")
	if err := os.WriteFile(src, []byte(ProfileBegin+"\n- CLI_PROFILE_RULE\n"+ProfileEnd+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(env []string, args ...string) (string, error) {
		c := exec.Command(selfBin, args...)
		c.Env = append(append(os.Environ(), "AGENT_JOBS_HOME="+home), env...)
		out, err := c.CombinedOutput()
		return string(out), err
	}
	if out, err := run(nil, "profile", "--agent", "test", "--from", src, "--dry-run"); err != nil || !strings.Contains(out, "CLI_PROFILE_RULE") {
		t.Fatalf("profile --dry-run: %v\n%s", err, out)
	}
	if out, err := run(nil, "profile", "--agent", "test", "--from", src); err != nil || !strings.Contains(out, "job profile written") {
		t.Fatalf("profile: %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(filepath.Join(home, "test", "owner-rules.md")); !strings.Contains(string(b), "CLI_PROFILE_RULE") {
		t.Fatalf("owner-rules.md: %s", b)
	}
	if out, err := run(nil, "card", "new", "--agent", "test", "--repo", repo, "--name", "demo"); err != nil {
		t.Fatalf("card new: %v\n%s", err, out)
	}
	out, err := run(nil, "card", "list", "--agent", "test", "--repo", filepath.Join(repo, "."))
	if err != nil || !strings.Contains(out, "demo") {
		t.Fatalf("card list --repo: %v\n%s", err, out)
	}
	for _, args := range [][]string{
		{"profile", "--agent", "test", "--from", src},
		{"card", "new", "--agent", "test", "--repo", repo, "--name", "other"},
		{"card", "list", "--agent", "test"},
	} {
		if out, err := run([]string{JobEnvVar + "=some-job"}, args...); err == nil || !strings.Contains(out, "refused") {
			t.Errorf("%v inside a job: err=%v\n%s", args, err, out)
		}
	}
}

func mustIdentity(t *testing.T, u string) string {
	t.Helper()
	id, err := RemoteIdentity(u, "/")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// M3: an origin with CR/LF cannot inject a second front matter selector into a new card.
func TestNewCardRejectsControlCharsInOrigin(t *testing.T) {
	m := newManager(t)
	repo := newRepo(t)
	gitT(t, repo, "config", "remote.origin.url", "https://github.com/o/r\nrepo: /")
	if p, err := m.NewCard(repo, "evil"); err == nil {
		b, _ := os.ReadFile(p)
		t.Fatalf("card written from a CR/LF origin:\n%s", b)
	}
	if _, err := os.Stat(filepath.Join(m.CardsDir(), "evil.md")); !os.IsNotExist(err) {
		t.Fatal("card file exists after the refusal")
	}
	// A plain origin is written as the shorthand and parses back to the same identity.
	gitT(t, repo, "config", "remote.origin.url", "git@github.com:Org/Proj.git")
	p, err := m.NewCard(repo, "ok")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), "remote: github.com/Org/Proj\n") {
		t.Fatalf("card:\n%s", b)
	}
}

// N5: `card new` works for an origin whose host has no dot (or is an IPv6 address), and the written
// value reads back to the origin's identity.
func TestNewCardDotlessHost(t *testing.T) {
	m := newManager(t)
	for i, u := range []string{"git@myserver:org/repo.git", "ssh://localhost/srv/repo", "ssh://git@[::1]:22/o/r.git", "https://github.com/O/R"} {
		repo := newRepo(t)
		gitT(t, repo, "config", "remote.origin.url", u)
		name := fmt.Sprintf("c%d", i)
		p, err := m.NewCard(repo, name)
		if err != nil {
			t.Fatalf("%s: %v", u, err)
		}
		b, _ := os.ReadFile(p)
		c, err := ParseCard(name, b)
		if err != nil {
			t.Fatalf("%s: %v", u, err)
		}
		want, err := RemoteIdentity(u, repo)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(c.Remotes, want) {
			t.Fatalf("%s: card remotes %v, want %s:\n%s", u, c.Remotes, want, b)
		}
	}
}

// M4: a card that is a symlink (also one pointing outside the cards dir) or not a regular file is
// refused, never followed.
func TestCardSymlinkAndNonRegularRefused(t *testing.T) {
	m := newManager(t)
	if err := os.MkdirAll(m.CardsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(outside, []byte("---\nrepo: /x\n---\n- SECRET\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(m.CardsDir(), "link.md")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := m.LoadCards(); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlinked card: %v", err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(m.CardsDir(), "dir.md"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := m.LoadCards(); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("directory card: %v", err)
	}
}

// M5: one card may be exactly MaxCardBytes, not one byte more; the brief section of all matching
// cards (headings and separators included) may be exactly MaxCardsPerJobBytes, not one byte more.
func TestCardSizeLimitsExact(t *testing.T) {
	m := newManager(t)
	repo := newRepo(t)
	if err := os.MkdirAll(m.CardsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	front := "---\nrepo: " + repo + "\n---\n"
	writeCard := func(name string, size int) {
		t.Helper()
		body := strings.Repeat("b", size-len(front))
		if err := os.WriteFile(filepath.Join(m.CardsDir(), name+".md"), []byte(front+body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeCard("one", MaxCardBytes)
	if _, err := m.LoadCards(); err != nil {
		t.Fatalf("card of exactly %d bytes: %v", MaxCardBytes, err)
	}
	writeCard("one", MaxCardBytes+1)
	if _, err := m.LoadCards(); err == nil || !strings.Contains(err.Error(), "limit is 4096") {
		t.Fatalf("card of %d bytes: %v", MaxCardBytes+1, err)
	}
	// Three cards whose rendered section is exactly the aggregate limit.
	rendered := func(name string, body int) int { return len("## Project card: "+name+"\n\n") + body + 2 }
	b1 := MaxCardBytes - len(front)
	b2 := b1
	b3 := MaxCardsPerJobBytes - rendered("c1", b1) - rendered("c2", b2) - rendered("c3", 0)
	if b3 <= 0 {
		t.Fatalf("test setup: b3=%d", b3)
	}
	if err := os.Remove(filepath.Join(m.CardsDir(), "one.md")); err != nil {
		t.Fatal(err)
	}
	writeCard("c1", len(front)+b1)
	writeCard("c2", len(front)+b2)
	writeCard("c3", len(front)+b3)
	cards, err := m.CardsFor(repo)
	if err != nil || len(RenderCards(cards)) != MaxCardsPerJobBytes {
		t.Fatalf("aggregate of exactly %d: %d bytes, %v", MaxCardsPerJobBytes, len(RenderCards(cards)), err)
	}
	writeCard("c3", len(front)+b3+1)
	if _, err := m.CardsFor(repo); err == nil || !strings.Contains(err.Error(), "limit is 8192") {
		t.Fatalf("aggregate of %d: %v", MaxCardsPerJobBytes+1, err)
	}
}
