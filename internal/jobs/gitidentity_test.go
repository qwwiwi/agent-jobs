package jobs

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
	if n, e := gitConfigValue(t, cfg, "user.name"), gitConfigValue(t, cfg, "user.email"); n != "" || e != "t@example.com" {
		t.Fatalf("job gitconfig identity %q <%q>: the owner's name must not be taken over", n, e)
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
