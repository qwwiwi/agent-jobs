package jobs

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	agentjobs "github.com/qwwiwi/agent-jobs"
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
	j := runToEnd(t, m, enqueue(t, m, "x", nil).ID)
	if j.Status != store.StatusSucceeded {
		t.Fatalf("job %s: %q", j.Status, j.Error)
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
	agent := map[string]any{"tool_name": "Agent", "tool_input": map[string]any{"description": "d", "prompt": "p"}}
	for i := 1; i <= 3; i++ {
		if code := runHook(t, cmd, agent); code != 0 {
			t.Fatalf("subagent %d: exit %d, want 0", i, code)
		}
	}
	if code := runHook(t, cmd, agent); code != 2 {
		t.Fatalf("fourth subagent: exit %d, want 2", code)
	}
	// A second job has its own count.
	j2 := runToEnd(t, m, enqueue(t, m, "y", nil).ID)
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

func TestNormalizeRemote(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/Owner/Repo.git":      "github.com/owner/repo",
		"https://user@github.com:443/O/R/":       "github.com/o/r",
		"git@github.com:O/R.git":                 "github.com/o/r",
		"ssh://git@github.com/O/R":               "github.com/o/r",
		"github.com/O/R":                         "github.com/o/r",
		"/srv/git/project.git":                   "/srv/git/project",
		"  https://gitlab.example.com/g/s/p  \n": "gitlab.example.com/g/s/p",
	} {
		if got := NormalizeRemote(in); got != want {
			t.Errorf("NormalizeRemote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseCard(t *testing.T) {
	c, err := ParseCard("p", []byte("---\nrepo: /src/p\nremote: git@github.com:O/P.git\n---\n# P\n\n## Stack\n- Go\n"))
	if err != nil || len(c.Repos) != 1 || c.Remotes[0] != "github.com/o/p" || !strings.HasPrefix(c.Body, "# P") {
		t.Fatalf("ParseCard = %+v, %v", c, err)
	}
	for name, s := range map[string]string{
		"no front matter": "# P\n",
		"unclosed":        "---\nrepo: /x\n# P\n",
		"no keys":         "---\n---\n# P\n",
		"relative repo":   "---\nrepo: src/p\n---\n# P\n",
		"unknown key":     "---\nrepo: /x\nowner: me\n---\n# P\n",
		"empty body":      "---\nrepo: /x\n---\n\n",
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
	if !c.Matches("/nowhere", []string{NormalizeRemote("https://github.com/qwwiwi/agent-jobs.git")}) {
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
	if !strings.Contains(string(card), "remote: github.com/org/project") || !strings.Contains(string(card), "## Known traps") {
		t.Fatalf("new card:\n%s", card)
	}
	if err := os.WriteFile(p, bytes.Replace(card, []byte("## Known traps"), []byte("## Known traps\n- CARD_MARKER_BY_REPO"), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	// A card that matches by remote only, and one for another project.
	other := filepath.Join(m.CardsDir(), "remote-only.md")
	if err := os.WriteFile(other, []byte("---\nremote: https://github.com/org/project\n---\n- CARD_MARKER_BY_REMOTE\n"), 0o600); err != nil {
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
