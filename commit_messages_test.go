package agentjobs

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// commitRepo is a throwaway git repository with an isolated environment (no global config, no
// GIT_* from a hook).
type commitRepo struct {
	t   *testing.T
	dir string
	env []string
}

func newCommitRepo(t *testing.T) *commitRepo {
	t.Helper()
	dir := t.TempDir()
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	env = append(env, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
	r := &commitRepo{t: t, dir: dir, env: env}
	r.git("init", "-q")
	return r
}

func (r *commitRepo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir, cmd.Env = r.dir, r.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit writes the message through a file, so it reaches git byte for byte.
func (r *commitRepo) commit(msg string) string {
	r.t.Helper()
	f := filepath.Join(r.t.TempDir(), "msg")
	if err := os.WriteFile(f, []byte(msg), 0o644); err != nil {
		r.t.Fatal(err)
	}
	r.git("commit", "-q", "--allow-empty", "-F", f)
	return r.git("rev-parse", "--short", "HEAD")
}

func (r *commitRepo) check() (string, error) {
	r.t.Helper()
	script, err := filepath.Abs("scripts/check-commit-messages.sh")
	if err != nil {
		r.t.Fatal(err)
	}
	cmd := exec.Command("bash", script)
	cmd.Dir, cmd.Env = r.dir, r.env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestCheckCommitMessagesRefusesCyrillic(t *testing.T) {
	r := newCommitRepo(t)
	r.commit("Add the base\n\nCo-authored-by: Someone <someone@users.noreply.github.com>\n")
	if out, err := r.check(); err != nil {
		t.Fatalf("English-only history refused: %v\n%s", err, out)
	}

	ru := r.commit("Добавил проверку лимитов\n")
	out, err := r.check()
	if err == nil {
		t.Fatalf("Russian subject accepted:\n%s", out)
	}
	if !strings.Contains(out, ru) {
		t.Fatalf("output does not name commit %s:\n%s", ru, out)
	}

	// Cyrillic only in the body is refused too.
	r.git("reset", "-q", "--hard", "HEAD~1")
	body := r.commit("Fix the limits\n\nТело на русском.\n")
	if out, err := r.check(); err == nil || !strings.Contains(out, body) {
		t.Fatalf("Russian body accepted or not named (%v):\n%s", err, out)
	}

	r.git("reset", "-q", "--hard", "HEAD~1")
	if out, err := r.check(); err != nil {
		t.Fatalf("refused after dropping the Russian commit: %v\n%s", err, out)
	}
}

func TestCheckCommitMessagesOnlyUnpushed(t *testing.T) {
	r := newCommitRepo(t)
	r.commit("Добавил старое\n")
	// Commits already on origin/main are not the pusher's to fix.
	r.git("update-ref", "refs/remotes/origin/main", "HEAD")
	r.commit("Add the new part\n")
	if out, err := r.check(); err != nil {
		t.Fatalf("commit on origin/main checked: %v\n%s", err, out)
	}
	ru := r.commit("Новое\n")
	if out, err := r.check(); err == nil || !strings.Contains(out, ru) {
		t.Fatalf("unpushed Russian commit accepted or not named (%v):\n%s", err, out)
	}
}
