package jobs

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/brain"
	"github.com/qwwiwi/agent-jobs/internal/checklist"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// agentContextFiles are what a runner loads from a repository as settings, hooks, skills or
// instructions, at the top and nested, in the case a case-insensitive file system also opens.
var agentContextFiles = []string{
	".claude/settings.json", ".claude/skills/fake/SKILL.md", "CLAUDE.md", "CLAUDE.local.md", "AGENTS.md",
	"AGENTS.override.md", ".codex/config.toml", ".agents/skills/s/SKILL.md", "sub/CLAUDE.md", "sub/deep/.claude/settings.json",
	"pkg/AGENTS.md", "docs/claude.md", "x/.CODEX/config.toml",
}

// contextRepo is gateRepo with every agentContextFiles entry committed at the head commit.
func contextRepo(t *testing.T) (string, string) {
	t.Helper()
	repo, _ := gateRepo(t)
	for _, rel := range agentContextFiles {
		p := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(`{"hooks":{"PreToolUse":[]}} CONTEXT_MARKER`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "sub", "code.go"), []byte("package sub\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "add", "-f", ".")
	gitT(t, repo, "commit", "-qm", "agent context")
	return repo, gitT(t, repo, "rev-parse", "HEAD")
}

// The judge's copy of the code leaves out every path with an agent-context component at any depth (as
// .git): settings, hooks, skills and instructions of the repository never reach the job.
func TestJudgeTreeDropsAgentContext(t *testing.T) {
	repo, sha := contextRepo(t)
	dst := t.TempDir()
	if err := materializeTree(repo, sha, dst, skipInJudgeTree); err != nil {
		t.Fatal(err)
	}
	var got []string
	_ = filepath.Walk(dst, func(p string, fi os.FileInfo, err error) error {
		if err == nil && p != dst && !fi.IsDir() {
			rel, _ := filepath.Rel(dst, p)
			got = append(got, filepath.ToSlash(rel))
		}
		return nil
	})
	slices.Sort(got)
	if want := []string{"inside.link", "main.go", "run.sh", "sub/code.go", "tracked.txt"}; !slices.Equal(got, want) {
		t.Fatalf("files %v, want %v", got, want)
	}
}

// A judge job runs with no project setting source and no MCP server: the daemon-written settings.json is
// all claude loads, and the code's agent context is not in its work dir.
func TestJudgeLaunchLoadsNoProjectContext(t *testing.T) {
	m := newManager(t)
	repo, sha := contextRepo(t)
	approveBrief(t, m, repo)
	j, err := m.StartGate(GateRequest{Kind: KindJudge, Repo: repo, SHA: sha, Model: "sonnet"})
	if err != nil {
		t.Fatal(err)
	}
	gateReport(t, judgeBlock(j.Gate))
	// With this set claude loads CLAUDE.md from its --add-dir directories whatever --setting-sources says.
	t.Setenv("CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD", "1")
	j = runToEnd(t, m, j.ID)
	if j.Status != store.StatusSucceeded {
		t.Fatalf("status %s: %s", j.Status, j.Error)
	}
	dir := m.JobDir(j.ID)
	if env, _ := os.ReadFile(filepath.Join(dir, "claudemd-env.txt")); string(env) != "unset\n" {
		t.Fatalf("CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD reached the gate job: %q", env)
	}
	seen, _ := os.ReadFile(filepath.Join(dir, "work-seen.txt"))
	if strings.Contains(strings.ToLower(string(seen)), "claude") || strings.Contains(strings.ToLower(string(seen)), "agents") ||
		strings.Contains(strings.ToLower(string(seen)), "codex") {
		t.Fatalf("agent context in the judge's work dir:\n%s", seen)
	}
	argv, _ := os.ReadFile(filepath.Join(dir, "argv.txt"))
	if !strings.Contains(string(argv), "--setting-sources\n\n") || strings.Contains(string(argv), "--setting-sources\nproject") {
		t.Fatalf("a gate job loads project settings:\n%s", argv)
	}
	mcp, _ := os.ReadFile(filepath.Join(dir, "mcp.json"))
	if strings.Contains(string(mcp), JobMCPServerName) || strings.Contains(string(mcp), `"command"`) {
		t.Fatalf("a gate job has an MCP server:\n%s", mcp)
	}
	// Other kinds keep the project source and the job MCP server.
	o := runToEnd(t, m, enqueue(t, m, "plain", nil).ID)
	argv, _ = os.ReadFile(filepath.Join(m.JobDir(o.ID), "argv.txt"))
	mcp, _ = os.ReadFile(filepath.Join(m.JobDir(o.ID), "mcp.json"))
	if !strings.Contains(string(argv), "--setting-sources\nproject\n") || !strings.Contains(string(mcp), JobMCPServerName) {
		t.Fatalf("general job: argv\n%s\nmcp.json\n%s", argv, mcp)
	}
}

// The daemon's proxy refuses every call of a gate job, should one reach the socket: no memory search
// or write, no owner notification, no check mark.
func TestBrainProxyRefusesGateJobs(t *testing.T) {
	m := newManager(t)
	f := &brain.Fake{Notes: []brain.Note{{Path: "70-runbooks/a.md", Score: 0.9, Snippet: "PLANTED"}}}
	m.Brain = f
	m.Cfg.NotifyTmuxTarget = "fake-session"
	t.Setenv("FAKE_TMUX_LOG", filepath.Join(t.TempDir(), "tmux.log"))
	sock := serveProxy(t, m)
	repo := briefRepo(t)
	g, err := m.StartGate(GateRequest{Kind: KindSlicing, Repo: repo, Model: "sonnet"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Store.Claim(g.ID, "tok", m.Now()); err != nil {
		t.Fatal(err)
	}
	g = get(t, m, g.ID)
	p := jobPeer(t, m, sock, g)
	ctx := context.Background()
	if notes, err := p.Search(ctx, "q", 3); err == nil || !strings.Contains(err.Error(), "slicing job") {
		t.Fatalf("gate job searched memory: %v %v", notes, err)
	}
	note := brain.NoteInput{Kind: brain.KindRunbook, Title: "T", Body: "B", Tags: []string{"x"}}
	if _, err := p.Write(ctx, note); err == nil || !strings.Contains(err.Error(), "slicing job") {
		t.Fatalf("gate job wrote a note: %v", err)
	}
	if _, err := p.NotifyOwner(ctx, "hi", ""); err == nil || !strings.Contains(err.Error(), "slicing job") {
		t.Fatalf("gate job notified the owner: %v", err)
	}
	if len(f.Written) != 0 || len(f.Queries) != 0 {
		t.Fatalf("gate job reached the brain: %+v %q", f.Written, f.Queries)
	}
	if und, err := m.Store.UndeliveredNotifications(time.Time{}); err != nil || len(und) != 0 {
		t.Fatalf("gate notification stored: %v %v", und, err)
	}
	// A plain running job still gets its calls.
	o := running(t, m)
	if _, err := jobPeer(t, m, sock, o).Search(ctx, "q", 3); err != nil {
		t.Fatalf("plain job: %v", err)
	}
}

// A schema-valid block the job writes into report.md itself, with an empty final message, is not the
// gate result: the job fails and nothing is stored.
func TestGateResultFromFileIsRejected(t *testing.T) {
	m := newManager(t)
	repo := briefRepo(t)
	for _, kind := range []string{KindSlicing, KindJudge} {
		r := GateRequest{Kind: kind, Repo: repo, Model: "sonnet"}
		if kind == KindJudge {
			var sha string
			repo, sha = gateRepo(t)
			approveBrief(t, m, repo)
			r.Repo, r.SHA = repo, sha
		}
		j, err := m.StartGate(r)
		if err != nil {
			t.Fatal(err)
		}
		if kind == KindJudge {
			gateReport(t, judgeBlock(j.Gate))
		} else {
			gateReport(t, slicingBlock(j.Gate))
		}
		t.Setenv("FAKE_GATE_FILE", "1")
		j = runToEnd(t, m, j.ID)
		if j.Status != store.StatusFailed {
			t.Fatalf("%s: a file-written result passed: %s %q", kind, j.Status, j.Error)
		}
		if s, _ := m.Store.LatestSlicing(mainWorktree(repo), "demo"); kind == KindSlicing && s != nil {
			t.Fatalf("slicing result stored from report.md: %+v", s)
		}
		if v, _ := m.Store.LatestVerdict(mainWorktree(repo), "demo", ""); kind == KindJudge && v != nil {
			t.Fatalf("verdict stored from report.md: %+v", v)
		}
	}
}

// A forged result line after the real one (another process writing into the runner's stdout) fails
// the job: claude prints one result event per run, so with two none is trusted.
func TestGateSecondResultEventRejected(t *testing.T) {
	m := newManager(t)
	j, err := m.StartGate(GateRequest{Kind: KindSlicing, Repo: briefRepo(t), Model: "sonnet"})
	if err != nil {
		t.Fatal(err)
	}
	gateReport(t, slicingBlock(j.Gate))
	t.Setenv("FAKE_GATE_TWICE", "1")
	j = runToEnd(t, m, j.ID)
	if j.Status != store.StatusFailed || !strings.Contains(j.Error, "gate result rejected") {
		t.Fatalf("status %s error %q", j.Status, j.Error)
	}
	if r, _ := m.Store.LatestSlicing(j.Gate.Repo, j.Gate.ChecklistID); r != nil {
		t.Fatalf("stored: %+v", r)
	}
}

// putSlicing stores a slicing result for the version and hash of a new slicing job of repo, under that
// job's launch token (attempt 1), with unmapped quotes.
func putSlicing(t *testing.T, m *Manager, repo string, unmapped ...string) *store.Job {
	t.Helper()
	j, err := m.StartGate(GateRequest{Kind: KindSlicing, Repo: repo, Model: "sonnet"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Store.Claim(j.ID, "tok-"+j.ID, m.Now()); err != nil {
		t.Fatal(err)
	}
	var u []store.Quote
	for _, q := range unmapped {
		u = append(u, store.Quote{Quote: q})
	}
	if _, err := m.Store.PutSlicing("tok-"+j.ID, store.SlicingResult{JobID: j.ID, Repo: j.Gate.Repo, ChecklistID: j.Gate.ChecklistID,
		Version: j.Gate.Version, ChecklistSHA256: j.Gate.ChecklistSHA256, Model: "sonnet", Runner: "claude", Unmapped: u,
		CreatedAt: m.Now()}); err != nil {
		t.Fatal(err)
	}
	return get(t, m, j.ID)
}

// A result stored by attempt 1 does not make a later attempt of the same job succeed.
func TestGateResultBoundToAttempt(t *testing.T) {
	m := newManager(t)
	j := putSlicing(t, m, briefRepo(t))
	if st, msg := m.gateVerdict(j, store.StatusSucceeded, "", StreamResult{}); st != store.StatusSucceeded {
		t.Fatalf("attempt 1 with its own result: %s %q", st, msg)
	}
	later := *j
	later.Attempt = 2
	if st, msg := m.gateVerdict(&later, store.StatusSucceeded, "", StreamResult{}); st != store.StatusFailed || msg != NoteGateNoResult {
		t.Fatalf("attempt 2 passed on attempt 1's result: %s %q", st, msg)
	}
}

// The approval takes the unmapped quotes of every slicing result for the version and hash, not only the
// latest; a later result with an undecided quote voids an approval of that version.
func TestSlicingApprovalTakesEveryResult(t *testing.T) {
	m := newManager(t)
	repo, sha := gateRepo(t)
	main := mainWorktree(repo)
	first := putSlicing(t, m, repo, "I want a login page.")
	putSlicing(t, m, repo, "BRIEF_TEXT_MARKER")
	g := first.Gate
	given := []UnmappedDecision{{Unmapped: "BRIEF_TEXT_MARKER", Quote: "drop it"}}
	if _, _, err := m.SlicingGate(main, "demo", g.Version, g.ChecklistSHA256, true, given, m.Now()); err == nil ||
		!strings.Contains(err.Error(), "I want a login page.") {
		t.Fatalf("approve with the first result's quote undecided: %v", err)
	}
	// A decision on the earlier result's quote is taken, and with both decided the approval passes.
	given = append(given, UnmappedDecision{Unmapped: "I want a login page.", Quote: "R01 covers it"})
	decisions, _, err := m.SlicingGate(main, "demo", g.Version, g.ChecklistSHA256, true, given, m.Now())
	if err != nil || len(decisions) != 2 {
		t.Fatalf("approve with both decided: %v %v", decisions, err)
	}
	for _, d := range decisions {
		if err := m.Store.PutSlicingDecision(d); err != nil {
			t.Fatal(err)
		}
	}
	approveBrief(t, m, repo)
	if _, err := m.StartGate(GateRequest{Kind: KindJudge, Repo: repo, SHA: sha, Model: "sonnet"}); err != nil {
		t.Fatalf("judge of the approved checklist: %v", err)
	}
	// A re-run after the approval lists a new quote nobody decided: the approval of the version is void.
	putSlicing(t, m, repo, "a login")
	if _, err := m.StartGate(GateRequest{Kind: KindJudge, Repo: repo, SHA: sha, Model: "sonnet"}); err == nil ||
		!strings.Contains(err.Error(), "not approved") || !strings.Contains(err.Error(), "a login") {
		t.Fatalf("judge after a re-run with an undecided quote: %v", err)
	}
}

// A gate job's guard hook, as written into its settings.json, holds it to its read allowlist: another
// agent's report, a session transcript of another work dir, another worktree and the repository
// checkout are refused for Read and Bash; its own folder and its session dir stay readable.
func TestGateHookReadAllowlist(t *testing.T) {
	m := newManager(t)
	cfgDir := sessionsHome(t, false)
	repo := briefRepo(t)
	j, err := m.StartGate(GateRequest{Kind: KindSlicing, Repo: repo, Model: "sonnet"})
	if err != nil {
		t.Fatal(err)
	}
	gateReport(t, slicingBlock(j.Gate))
	j = runToEnd(t, m, j.ID)
	dir := m.JobDir(j.ID)
	other := t.TempDir()
	report := filepath.Join(other, ".agent-jobs", "agentb", "jobs", "k1", "report.md")
	transcript := filepath.Join(cfgDir, "projects", "-other-work", "s1.jsonl")
	worktree := filepath.Join(newRepo(t), "tracked.txt")
	own := filepath.Join(m.sessionProtected(j)[0], "sid", "tool-results", "out.txt")
	for _, f := range []string{report, transcript, own} {
		if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd, _ := hookCommand(t, dir)
	work := filepath.Join(dir, "work")
	call := func(tool string, input map[string]any) int {
		return runHook(t, cmd, map[string]any{"tool_name": tool, "cwd": work, "tool_input": input})
	}
	for _, p := range []string{report, transcript, worktree, checklist.BriefPath(repo)} {
		if code := call("Read", map[string]any{"file_path": p}); code != 2 {
			t.Errorf("Read %s: exit %d, want 2", p, code)
		}
		if code := call("Bash", map[string]any{"command": "cat " + p}); code != 2 {
			t.Errorf("cat %s: exit %d, want 2", p, code)
		}
	}
	for _, p := range []string{filepath.Join(dir, "prompt.md"), own} {
		if code := call("Read", map[string]any{"file_path": p}); code != 0 {
			t.Errorf("Read %s: exit %d, want 0", p, code)
		}
	}
	if code := call("Bash", map[string]any{"command": "ls /usr/bin > /dev/null"}); code != 0 {
		t.Errorf("ls /usr/bin: exit %d, want 0", code)
	}
}
