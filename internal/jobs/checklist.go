package jobs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
)

// ChecklistRepo resolves the repo argument of the checklist tools (Wave F, F2): an absolute path in a
// git work tree. top is that work tree's root (where brief/ is read and written); main is the
// repository's main worktree, the key waivers are stored under (as a job's repo_path).
func ChecklistRepo(path string) (top, main string, err error) {
	if path == "" || !filepath.IsAbs(path) {
		return "", "", fmt.Errorf("repo %q must be an absolute path", path)
	}
	if top, err = git(filepath.Clean(path), "rev-parse", "--show-toplevel"); err != nil {
		return "", "", fmt.Errorf("repo %s is not a git work tree: %w", path, err)
	}
	return top, mainWorktree(top), nil
}

// commitSHARe is a full commit id (sha1 or sha256 object format).
var commitSHARe = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// CheckCommit verifies that sha is a full commit id that names a commit in repo.
func CheckCommit(repo, sha string) error {
	if !commitSHARe.MatchString(sha) {
		return fmt.Errorf("sha %q must be a full commit id (40 or 64 lowercase hex)", sha)
	}
	if _, err := git(repo, "cat-file", "-e", sha+"^{commit}"); err != nil {
		return fmt.Errorf("sha %s is not a commit in %s", sha, repo)
	}
	return nil
}

// checklistLockFile serializes the checklist tools of this agent (every session's MCP server): one
// checklist_set reads, computes and writes both brief files while no other runs.
const checklistLockFile = "checklist.lock"

// LockChecklist takes <state>/checklist.lock exclusively (waiting for another holder) and returns the
// unlock.
func (m *Manager) LockChecklist() (func(), error) {
	f, err := os.OpenFile(filepath.Join(m.StateDir, checklistLockFile), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open checklist lock: %w", err)
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("flock checklist lock: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
