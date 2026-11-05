package jobs

import (
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

func TestCodexPromptSandboxSettings(t *testing.T) {
	for _, tc := range []struct {
		name, mode, kind, sandbox, network, commit string
		networkOn                                  bool
	}{
		{"default review", "", "review", "read-only", "off", "no commits", true},
		{"native research", "native", "research", "read-only", "off", "no commits", false},
		{"offline fix", "auto", "fix", "workspace-write", "off", "wrapper commits", false},
		{"online general", "native", "general", "workspace-write", "on", "wrapper commits", true},
		{"full access fix", "worktree-full-access", "fix", "danger-full-access", "on", "commit locally yourself", false},
		{"full access review", "worktree-full-access", "review", "danger-full-access", "on", "no commits", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &Manager{StateDir: t.TempDir(), Cfg: Config{CodexSandboxMode: tc.mode, CodexWriteNetwork: &tc.networkOn}}
			j := &store.Job{ID: "j1", Kind: tc.kind, Runner: store.RunnerCodex, RepoPath: "repository", Brief: "work"}
			p, err := m.BuildPrompt(j, "rules", nil, MemoryNotes{})
			if err != nil {
				t.Fatal(err)
			}
			mode := tc.mode
			if mode == "" {
				mode = "auto"
			}
			for _, want := range []string{"## Your sandbox", "codex_sandbox=" + mode, "filesystem=" + tc.sandbox,
				"network=" + tc.network, tc.commit, "Do not start `codex`"} {
				if !strings.Contains(p, want) {
					t.Errorf("prompt lacks %q", want)
				}
			}
			if tc.sandbox == CodexSandboxWrite {
				if strings.Contains(p, "Commit locally; never push") || strings.Contains(p, "and create a local commit.") || !strings.Contains(p, "Write files only") {
					t.Error("write prompt still asks the job to commit")
				}
			}
			if tc.sandbox == CodexSandboxFullAccess && strings.Contains(p, "You cannot commit") {
				t.Error("full-access prompt incorrectly claims git is read-only")
			}
			if tc.network == "off" {
				for _, want := range []string{"socket-bound tests", "describe the screenshot", "existing checkout"} {
					if !strings.Contains(p, want) {
						t.Errorf("offline guidance lacks %q", want)
					}
				}
			}
		})
	}
	t.Run("authorized launch snapshot", func(t *testing.T) {
		on := true
		m := &Manager{StateDir: t.TempDir(), Cfg: Config{CodexSandboxMode: CodexSandboxModeWorktreeFullAccess, CodexWriteNetwork: &on}}
		j := &store.Job{ID: "j1", Kind: "fix", Runner: store.RunnerCodex, Brief: "git clone repository"}
		p, err := m.buildPrompt(j, "rules", nil, MemoryNotes{}, priorSnapshot{}, nil, Config{CodexSandboxMode: CodexSandboxModeNative})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(p, "filesystem=workspace-write; network=off") || !strings.Contains(p, "The brief asks to clone") ||
			strings.Contains(p, "filesystem=danger-full-access") || strings.Contains(p, "commit locally yourself") {
			t.Error("prompt did not use revoked permissions from launch config")
		}
	})
}

func TestCodexPromptBriefConflicts(t *testing.T) {
	m := &Manager{StateDir: t.TempDir()}
	j := &store.Job{ID: "j1", Kind: "fix", Runner: store.RunnerCodex,
		Brief: "GIT CLONE repo; git COMMIT; git PUSH; PLAYWRIGHT screenshot; PSQL pg_virtualenv DOCKER; CURL https://example.invalid; RUN CODEX"}
	p, err := m.BuildPrompt(j, "rules", nil, MemoryNotes{})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(p, "The brief asks "); got != 7 {
		t.Fatalf("got %d conflict lines, want 7", got)
	}
	// Claude prompts do not acquire Codex restrictions or lint.
	j.Runner = store.RunnerClaude
	p, err = m.BuildPrompt(j, "rules", nil, MemoryNotes{})
	if err != nil || strings.Contains(p, "## Your sandbox") || strings.Contains(p, "The brief asks ") {
		t.Fatalf("Claude prompt changed: err=%v", err)
	}

	for _, tc := range []struct {
		brief, mode, kind string
		on                bool
		want              int
	}{
		{"CLONE repository", "", "fix", false, 1},
		{"Клонировать репозиторий", "", "fix", false, 1},
		{"СКЛОНИРУЙ репозиторий", "", "fix", false, 1},
		{"git commit", "", "fix", false, 1},
		{"Сделай коммит", "", "fix", false, 1},
		{"Запушить изменения", "", "fix", false, 1},
		{"git push", "worktree-full-access", "fix", true, 1},
		{"Take a screenshot", "", "fix", false, 1},
		{"Playwright", "", "fix", false, 1},
		{"Open a BROWSER", "", "fix", false, 1},
		{"снимок экрана", "", "fix", false, 1},
		{"psql", "", "fix", false, 1},
		{"pg_virtualenv", "", "fix", false, 1},
		{"Docker", "", "fix", false, 1},
		{"запусти PostgreSQL", "", "fix", false, 1},
		{"подними постгрес", "", "fix", false, 1},
		{"curl", "", "fix", false, 1},
		{"HTTP request", "", "fix", false, 0},
		{"https://example.invalid", "", "fix", false, 1},
		{"run `codex`", "", "fix", false, 1},
		{"starting CODEX", "", "fix", false, 1},
		{"Запуск codex", "", "fix", false, 1},
		{"Запусти кодекс", "", "fix", false, 1},
		{"codex exec", "worktree-full-access", "fix", true, 1},
		{"git clone; screenshot; psql; curl", "native", "fix", true, 0},
		{"git clone; screenshot; psql; curl", "worktree-full-access", "fix", false, 0},
		{"git commit", "worktree-full-access", "fix", false, 0},
		{"git commit", "worktree-full-access", "review", false, 1},
		{"git clone; screenshot; psql; curl", "native", "review", true, 4},
		{"curl http://localhost:3000", "", "fix", false, 0},
		{"curl http://127.0.0.1:3000", "", "fix", false, 0},
		{"curl http://[::1]:3000", "", "fix", false, 0},
		{"curl http://localhost:3000 https://example.invalid", "", "fix", false, 1},
		{"Read browser.go, cloned_value, commitCount and codex documentation", "", "fix", false, 0},
		{"git clone; git clone; CLONE", "", "fix", false, 1},
	} {
		t.Run(tc.brief+"/"+tc.mode+"/"+tc.kind, func(t *testing.T) {
			cfg := Config{CodexSandboxMode: tc.mode, CodexWriteNetwork: &tc.on}
			j := &store.Job{Runner: store.RunnerCodex, Kind: tc.kind, RepoPath: "repository", Brief: tc.brief}
			if got := cfg.CodexBriefWarnings(j); len(got) != tc.want {
				t.Errorf("got %v, want %d warnings", got, tc.want)
			}
		})
	}

	t.Run("gate ignores brief and warnings never echo it", func(t *testing.T) {
		j := &store.Job{Runner: store.RunnerCodex, Kind: KindJudge, Brief: "git push; curl https://example.invalid/PRIVATE_MARKER"}
		if got := (Config{}).CodexBriefWarnings(j); len(got) != 0 {
			t.Errorf("gate read brief: %v", got)
		}
		j.Kind = "fix"
		for _, warning := range (Config{}).CodexBriefWarnings(j) {
			if strings.Contains(warning, "PRIVATE_MARKER") {
				t.Error("warning echoed brief content")
			}
		}
	})
}

func TestCodexBriefNegatedRequests(t *testing.T) {
	for _, request := range []string{"clone", "git commit", "git push", "a browser", "Docker", "curl", "run codex"} {
		for _, negation := range []string{"not", "don't", "do not", "never", "no", "without", "не", "нельзя", "без", "никогда"} {
			t.Run(negation+" "+request, func(t *testing.T) {
				j := &store.Job{Runner: store.RunnerCodex, Kind: "fix", RepoPath: "repository", Brief: negation + " " + request}
				if got := (Config{}).CodexBriefWarnings(j); len(got) != 0 {
					t.Fatalf("negated request got warnings: %v", got)
				}
			})
		}
	}
	for _, brief := range []string{
		"Do not commit", "не коммить", "не пушь", "без браузера", "Нельзя клонировать",
		"никогда не запускай codex", "Don't start `codex`", "do not take a screenshot",
		"never use Docker", "do not fetch https://example.invalid", "no https://example.invalid",
	} {
		t.Run(brief, func(t *testing.T) {
			j := &store.Job{Runner: store.RunnerCodex, Kind: "fix", RepoPath: "repository", Brief: brief}
			if got := (Config{}).CodexBriefWarnings(j); len(got) != 0 {
				t.Fatalf("negated request got warnings: %v", got)
			}
		})
	}
}

func TestCodexBriefNegationScope(t *testing.T) {
	for _, brief := range []string{
		"Do not commit; git push", "Do not commit. Take a screenshot", "No browser\ngit commit",
		"No commit; commit", "No commit,commit", "No unrelated changes are needed here; clone repository",
	} {
		t.Run(brief, func(t *testing.T) {
			j := &store.Job{Runner: store.RunnerCodex, Kind: "fix", RepoPath: "repository", Brief: brief}
			if got := (Config{}).CodexBriefWarnings(j); len(got) != 1 {
				t.Fatalf("got %v, want one positive-request warning", got)
			}
		})
	}
}

func TestCodexBriefHTTPRequests(t *testing.T) {
	for _, tc := range []struct {
		brief string
		want  int
	}{
		{"Fix the HTTP handler and HTTPS support", 0},
		{"HTTP request parsing", 0},
		{"https://", 0},
		{"https:///path", 0},
		{"wget", 1},
		{"fetch the fixture", 1},
		{"Fetch https://example.invalid/data", 1},
		{"https://example.invalid/data", 1},
		{"wget http://localhost:3000/data", 0},
		{"do not wget https://example.invalid", 0},
		{"without fetch", 0},
	} {
		t.Run(tc.brief, func(t *testing.T) {
			j := &store.Job{Runner: store.RunnerCodex, Kind: "fix", Brief: tc.brief}
			if got := (Config{}).CodexBriefWarnings(j); len(got) != tc.want {
				t.Fatalf("got %v, want %d warnings", got, tc.want)
			}
		})
	}
}

func TestCodexBriefNestedCodexRequests(t *testing.T) {
	for _, tc := range []struct {
		brief string
		want  int
	}{
		{"Document the codex exec runner", 0},
		{"The codex review command uses the sandbox", 0},
		{"Explain codex --help output", 0},
		{"Обнови описание codex exec", 0},
		{"Run codex exec", 1},
		{"Invoke `codex` review", 1},
		{"Вызови codex review", 1},
		{"codex exec", 1},
		{"Commands:\n  $ codex review", 1},
		{"```sh\ncodex exec\n```", 1},
		{"Do not run codex exec", 0},
	} {
		t.Run(tc.brief, func(t *testing.T) {
			j := &store.Job{Runner: store.RunnerCodex, Kind: "fix", Brief: tc.brief}
			if got := (Config{}).CodexBriefWarnings(j); len(got) != tc.want {
				t.Fatalf("got %v, want %d warnings", got, tc.want)
			}
		})
	}
}

func TestCodexPromptNegatedBrief(t *testing.T) {
	m := &Manager{StateDir: t.TempDir()}
	j := &store.Job{ID: "j1", Kind: "fix", Runner: store.RunnerCodex, RepoPath: "repository",
		Brief: "Do not clone; do not commit; never push; no browser; without Docker; do not curl; never run codex"}
	p, err := m.BuildPrompt(j, "rules", nil, MemoryNotes{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p, "The brief asks ") {
		t.Fatal("prompt contradicts the negated brief")
	}
}

func TestCodexPromptReportCommitMessage(t *testing.T) {
	for _, tc := range []struct {
		name, runner, mode string
		wrapperCommits     bool
	}{
		{"native Codex", store.RunnerCodex, "native", true},
		{"full access Codex", store.RunnerCodex, "worktree-full-access", false},
		{"Claude", store.RunnerClaude, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &Manager{StateDir: t.TempDir(), Cfg: Config{CodexSandboxMode: tc.mode}}
			j := &store.Job{ID: "j1", Kind: "fix", Runner: tc.runner, RepoPath: "repository", Brief: "Fix parsing"}
			p, err := m.BuildPrompt(j, "rules", nil, MemoryNotes{})
			if err != nil {
				t.Fatal(err)
			}
			want := "with file:line and commit hashes"
			if tc.wrapperCommits {
				want = "with file:line and commit message (the wrapper commits after the run)"
				if strings.Contains(p, "commit hashes") {
					t.Error("wrapper-commit report still asks for commit hashes")
				}
			}
			if !strings.Contains(p, want) {
				t.Errorf("report contract lacks %q", want)
			}
		})
	}
}
