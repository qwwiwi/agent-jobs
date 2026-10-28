package jobs

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// gitConfigValue reads key from the job's gitconfig file ("" when unset).
func gitConfigValue(t *testing.T, cfg, key string) string {
	t.Helper()
	out, err := exec.Command("git", "config", "--file", cfg, "--get", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// With git_name/git_email empty, the job's gitconfig takes the repository's local identity, so the
// job's commits are not left without one (codex commits were skipped).
func TestGitIdentityFallsBackToRepoLocal(t *testing.T) {
	m := newManager(t)
	repo := newRepo(t) // local user.name T, user.email t@example.com
	j := runToEnd(t, m, enqueue(t, m, "do it", inRepo(repo)).ID)
	cfg := filepath.Join(m.JobDir(j.ID), gitConfigFile)
	if n, e := gitConfigValue(t, cfg, "user.name"), gitConfigValue(t, cfg, "user.email"); n != "T" || e != "t@example.com" {
		t.Fatalf("job gitconfig identity %q <%q>, want the repository's T <t@example.com>", n, e)
	}
}

// The configured identity wins over the repository's, field by field.
func TestGitIdentityConfigWins(t *testing.T) {
	m := newManager(t)
	m.Cfg.GitName = "Agent Bot"
	repo := newRepo(t)
	j := runToEnd(t, m, enqueue(t, m, "do it", inRepo(repo)).ID)
	cfg := filepath.Join(m.JobDir(j.ID), gitConfigFile)
	if n, e := gitConfigValue(t, cfg, "user.name"), gitConfigValue(t, cfg, "user.email"); n != "Agent Bot" || e != "t@example.com" {
		t.Fatalf("job gitconfig identity %q <%q>, want Agent Bot <t@example.com>", n, e)
	}
}

// Only the repository's local config counts (not the user's global identity), and a local identity
// that names the owner is not taken over; a job without a repository gets no identity.
func TestGitIdentityFallbackLimits(t *testing.T) {
	m := newManager(t)
	m.Cfg.OwnerNames = []string{"Owner Person"}
	repo := newRepo(t)
	gitT(t, repo, "config", "user.name", "owner person")
	j := runToEnd(t, m, enqueue(t, m, "do it", inRepo(repo)).ID)
	cfg := filepath.Join(m.JobDir(j.ID), gitConfigFile)
	if n, e := gitConfigValue(t, cfg, "user.name"), gitConfigValue(t, cfg, "user.email"); n != "" || e != "" {
		t.Fatalf("job gitconfig identity %q <%q>: an identity naming the owner must not be taken over, neither half", n, e)
	}
	bare := newRepo(t)
	gitT(t, bare, "config", "--unset", "user.name")
	gitT(t, bare, "config", "--unset", "user.email")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	gitT(t, home, "config", "--global", "user.name", "Global Name")
	if got := repoLocalIdentity(bare, "user.name"); got != "" {
		t.Fatalf("fallback read the global identity %q", got)
	}
	j = runToEnd(t, m, enqueue(t, m, "do it", nil).ID)
	if n := gitConfigValue(t, filepath.Join(m.JobDir(j.ID), gitConfigFile), "user.name"); n != "" {
		t.Fatalf("job without a repository got identity %q", n)
	}
}

// commitIdentity commits in the job's worktree with the job's git environment (GitEnv), as the job's
// runner would, and returns the commit's "author name <email> / committer name <email>".
func commitIdentity(t *testing.T, m *Manager, id string) string {
	t.Helper()
	dir := m.JobDir(id)
	env, err := GitEnv(dir, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "commit", "-q", "--allow-empty", "-m", "job commit")
	cmd.Dir, cmd.Env = filepath.Join(dir, "work"), env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("commit in the job worktree: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "log", "-1", "--format=%an <%ae> / %cn <%ce>")
	cmd.Dir, cmd.Env = filepath.Join(dir, "work"), env
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// The accepted identity is what a commit in the job's worktree carries: the configured one wins over
// the repository's local user.name/user.email (repository config outranks GIT_CONFIG_GLOBAL, so the
// gitconfig file alone would lose).
func TestGitIdentityEffectiveInWorktree(t *testing.T) {
	m := newManager(t)
	m.Cfg.GitName, m.Cfg.GitEmail = "Agent Bot", "bot@example.org"
	repo := newRepo(t) // local T <t@example.com>
	gitT(t, repo, "config", "author.name", "Author Override")
	j := runToEnd(t, m, enqueue(t, m, "do it", inRepo(repo)).ID)
	if got, want := commitIdentity(t, m, j.ID), "Agent Bot <bot@example.org> / Agent Bot <bot@example.org>"; got != want {
		t.Fatalf("job commit identity %q, want %q", got, want)
	}
	// Taken over from the repository, the pair is what the commit carries too.
	m.Cfg.GitName, m.Cfg.GitEmail = "", ""
	repo = newRepo(t)
	j = runToEnd(t, m, enqueue(t, m, "do it", inRepo(repo)).ID)
	if got, want := commitIdentity(t, m, j.ID), "T <t@example.com> / T <t@example.com>"; got != want {
		t.Fatalf("job commit identity %q, want %q", got, want)
	}
}

// The repository's local name and email are taken together or not at all: a repository with only one
// of them, or with one naming the owner, gives the job neither.
func TestGitIdentityRepoPairOnly(t *testing.T) {
	m := newManager(t)
	m.Cfg.OwnerNames = []string{"Owner Person"}
	for name, mod := range map[string][]string{
		"email only":  {"--unset", "user.name"},
		"name only":   {"--unset", "user.email"},
		"owner email": {"user.email", "owner person@example.com"}, // the name is fine, the email names the owner
	} {
		repo := newRepo(t)
		gitT(t, repo, append([]string{"config"}, mod...)...)
		j := runToEnd(t, m, enqueue(t, m, "do it", inRepo(repo)).ID)
		cfg := filepath.Join(m.JobDir(j.ID), gitConfigFile)
		if n, e := gitConfigValue(t, cfg, "user.name"), gitConfigValue(t, cfg, "user.email"); n != "" || e != "" {
			t.Errorf("%s: job gitconfig identity %q <%q>, want none", name, n, e)
		}
	}
}

// A codex job (no guard hook) whose commits would carry the owner's identity – the repository's local
// one, which a dropped fallback leaves in force – is refused before it starts; with an accepted
// identity configured it runs, and its commits carry that identity.
func TestCodexRefusedWithOwnerIdentity(t *testing.T) {
	m := newCodexManager(t)
	m.Cfg.OwnerNames = []string{"Owner Person"}
	repo := newRepo(t)
	gitT(t, repo, "config", "user.name", "Owner Person")
	j := enqueue(t, m, "review the thing", func(r *StartRequest) { codexReview(r); r.RepoPath = repo })
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if j = get(t, m, j.ID); j.Status != store.StatusFailed || !strings.Contains(j.Error, "refused to start") || !strings.Contains(j.Error, "owner") {
		t.Fatalf("codex job with the owner's effective identity: %s %q", j.Status, j.Error)
	}
	if _, err := os.Stat(filepath.Join(m.JobDir(j.ID), "stream.jsonl")); err == nil {
		t.Fatal("codex started with the owner's identity")
	}
	m.Cfg.GitName, m.Cfg.GitEmail = "Agent Bot", "bot@example.org"
	j = runToEnd(t, m, enqueue(t, m, "review the thing", func(r *StartRequest) { codexReview(r); r.RepoPath = repo }).ID)
	if j.Status != store.StatusSucceeded {
		t.Fatalf("codex job with an accepted identity: %s %q", j.Status, j.Error)
	}
	if got, want := commitIdentity(t, m, j.ID), "Agent Bot <bot@example.org> / Agent Bot <bot@example.org>"; got != want {
		t.Fatalf("codex job commit identity %q, want %q", got, want)
	}
}
