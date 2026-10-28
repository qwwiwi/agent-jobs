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

// The configured identity wins over the repository's.
func TestGitIdentityConfigWins(t *testing.T) {
	m := newManager(t)
	m.Cfg.GitName, m.Cfg.GitEmail = "Agent Bot", "bot@example.org"
	repo := newRepo(t)
	j := runToEnd(t, m, enqueue(t, m, "do it", inRepo(repo)).ID)
	cfg := filepath.Join(m.JobDir(j.ID), gitConfigFile)
	if n, e := gitConfigValue(t, cfg, "user.name"), gitConfigValue(t, cfg, "user.email"); n != "Agent Bot" || e != "bot@example.org" {
		t.Fatalf("job gitconfig identity %q <%q>, want Agent Bot <bot@example.org>", n, e)
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

// git_name and git_email are set together or not at all: a half identity let the repository's config
// fill in the other half (a codex job committed as "Job Bot <owner@…>" when the repository's pair named
// the owner and was dropped). Such a config is refused when it loads.
func TestGitIdentityHalfConfigRefused(t *testing.T) {
	state := t.TempDir()
	for _, body := range []string{
		`{"git_name": "Job Bot", "codex_write_allowed": true, "owner_names": ["Owner Person"]}`,
		`{"git_email": "bot@example.org"}`,
		`{"git_name": "Job Bot", "git_email": ""}`,
	} {
		if _, err := parseConfig(state, []byte(body)); err == nil || !strings.Contains(err.Error(), "git_name and git_email") {
			t.Errorf("%s: err = %v, want a refusal of the half identity", body, err)
		}
	}
	for _, body := range []string{`{}`, `{"git_name": "Job Bot", "git_email": "bot@example.org"}`} {
		if _, err := parseConfig(state, []byte(body)); err != nil {
			t.Errorf("%s: %v", body, err)
		}
	}
}

// A half configured identity (only possible past config validation) is never completed from the
// repository: the job gets no identity of its own, and a codex job, whose commits would then carry the
// repository's owner pair, is refused; the name alone no longer hides the owner's email.
func TestCodexRefusedWithHalfIdentity(t *testing.T) {
	m := newCodexManager(t)
	m.Cfg.OwnerNames = []string{"Owner Person"}
	m.Cfg.GitName = "Job Bot"
	repo := newRepo(t)
	gitT(t, repo, "config", "user.name", "Owner Person")
	gitT(t, repo, "config", "user.email", "boss@corp.example")
	if n, e := m.jobIdentity(repo); n != "" || e != "" {
		t.Fatalf("jobIdentity with a half config and the owner's repository pair: %q <%q>, want none", n, e)
	}
	j := enqueue(t, m, "review the thing", func(r *StartRequest) { codexReview(r); r.RepoPath = repo })
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if j = get(t, m, j.ID); j.Status != store.StatusFailed || !strings.Contains(j.Error, "refused to start") {
		t.Fatalf("codex job with a half identity over the owner's repository pair: %s %q", j.Status, j.Error)
	}
}

// The configured email being the email of the repository's rejected (owner-naming) pair is the owner's
// identity too, although it does not contain one of owner_names: a codex job is refused.
func TestCodexRefusedWithRejectedPairEmail(t *testing.T) {
	m := newCodexManager(t)
	m.Cfg.OwnerNames = []string{"Owner Person"}
	m.Cfg.GitName, m.Cfg.GitEmail = "Job Bot", "Boss@corp.example"
	repo := newRepo(t)
	gitT(t, repo, "config", "user.name", "Owner Person")
	gitT(t, repo, "config", "user.email", "boss@corp.example")
	j := enqueue(t, m, "review the thing", func(r *StartRequest) { codexReview(r); r.RepoPath = repo })
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if j = get(t, m, j.ID); j.Status != store.StatusFailed || !strings.Contains(j.Error, "refused to start") || !strings.Contains(j.Error, "email") {
		t.Fatalf("codex job committing with the rejected pair's email: %s %q", j.Status, j.Error)
	}
}

// Nothing in the repository's config changes the committed email: with the job's pair set, the
// repository's author.email, committer.email and an includeIf-included user.email lose; a job gitconfig
// with only half an identity (which would let them fill in the other half) is refused by GitEnv.
func TestGitIdentityRepoConfigCannotChangeEmail(t *testing.T) {
	m := newManager(t)
	m.Cfg.GitName, m.Cfg.GitEmail = "Agent Bot", "bot@example.org"
	repo := newRepo(t)
	inc := filepath.Join(t.TempDir(), "inc.gitconfig")
	if err := os.WriteFile(inc, []byte("[user]\n\temail = included@corp.example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "config", "author.email", "author@corp.example")
	gitT(t, repo, "config", "committer.email", "committer@corp.example")
	gitT(t, repo, "config", "includeIf.gitdir:/.path", inc)
	gitT(t, repo, "config", "include.path", inc)
	j := runToEnd(t, m, enqueue(t, m, "do it", inRepo(repo)).ID)
	if got, want := commitIdentity(t, m, j.ID), "Agent Bot <bot@example.org> / Agent Bot <bot@example.org>"; got != want {
		t.Fatalf("job commit identity %q, want %q", got, want)
	}
	// A half identity in the job's gitconfig: GitEnv refuses instead of leaving the email to the repository.
	cfg := filepath.Join(m.JobDir(j.ID), gitConfigFile)
	if err := os.Chmod(cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "config", "--file", cfg, "--unset", "user.email")
	if env, err := GitEnv(m.JobDir(j.ID), os.Environ()); err == nil {
		t.Fatalf("GitEnv with a half identity in the job's gitconfig: no error (env %d entries)", len(env))
	} else if !strings.Contains(err.Error(), "user.email") {
		t.Fatalf("GitEnv error %q does not name the missing half", err)
	}
}
