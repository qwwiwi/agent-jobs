package jobs

import (
	"errors"
	"fmt"
	"strings"
)

// branchGit runs git in one repository, with the daemon's timeouts and safe environment.
// Keeping the ref operations independent of the store also permits offline Git tests.
type branchGit func(args ...string) (string, error)

func validateResultBranch(run branchGit, name string) error {
	return validateBranchTarget(run, name, true)
}

func validateBranchTarget(run branchGit, name string, requireDefault bool) error {
	// check-ref-format expands @{-N}; accept only the literal name the caller supplied.
	checked, err := run("check-ref-format", "--branch", name)
	if err != nil || checked != name || strings.HasPrefix(name, "refs/") {
		return fmt.Errorf("invalid result branch %q", name)
	}
	// Fold protected names as well: refs may live on a case-insensitive filesystem.
	lower := strings.ToLower(name)
	if lower == "main" || lower == "master" || strings.HasPrefix(lower, "release") {
		return fmt.Errorf("protected result branch %q", name)
	}
	// Remote HEADs and init.defaultBranch are local metadata: never fetch just to validate a name.
	defaults, err := run("for-each-ref", "--format=%(symref)", "refs/remotes/")
	if err != nil {
		return fmt.Errorf("check default branch: %w", err)
	}
	knownDefault := false
	for _, ref := range strings.Fields(defaults) {
		if rest, ok := strings.CutPrefix(ref, "refs/remotes/"); ok {
			if _, branch, ok := strings.Cut(rest, "/"); ok {
				knownDefault = true
				if strings.EqualFold(branch, name) {
					return fmt.Errorf("protected default branch %q", name)
				}
			}
		}
	}
	if configured, err := run("config", "--get", "init.defaultBranch"); err == nil && configured != "" {
		if strings.EqualFold(name, configured) {
			return fmt.Errorf("protected default branch %q", name)
		}
	}
	// Without remote metadata, conservatively protect the main worktree's current branch.
	// init.defaultBranch is an extra protected name, not proof of an existing repo's default.
	if !knownDefault {
		current, err := run("symbolic-ref", "-q", "HEAD")
		if (err != nil || !strings.HasPrefix(current, "refs/heads/")) && requireDefault {
			return errors.New("result branch refused: default branch cannot be determined from local metadata (main worktree HEAD is detached)")
		}
		if strings.EqualFold(current, "refs/heads/"+name) {
			return fmt.Errorf("protected current branch %q (default branch metadata unavailable)", name)
		}
	}
	return nil
}

// publishBranchRef creates a branch or advances it only from an ancestor of head.
// The expected old OID makes the write atomic against other jobs and owner actions.
// It never follows a symbolic ref or changes a checked-out worktree's branch.
func publishBranchRef(run branchGit, name, head string) (fastForwarded bool, err error) {
	// Generated job/<id> refs must also work in detached repositories with no default metadata.
	// Explicit targets already passed the stricter start-time validation above.
	if err := validateBranchTarget(run, name, false); err != nil {
		return false, err
	}
	if !branchOID(head) {
		return false, errors.New("job head is not a full commit ID")
	}
	commit, err := run("rev-parse", "--verify", "--end-of-options", head+"^{commit}")
	if err != nil || commit != head {
		return false, errors.New("job head is not an available commit")
	}
	ref := "refs/heads/" + name
	if _, err := run("symbolic-ref", "-q", ref); err == nil {
		return false, errors.New("branch is a symbolic ref")
	}
	tip, lookupErr := run("rev-parse", "--verify", "--end-of-options", ref)
	if lookupErr != nil {
		tip = "" // update-ref's empty expected OID requires the ref not to exist.
	} else {
		if !branchOID(tip) {
			return false, errors.New("branch tip is not a full commit ID")
		}
		if tip == head {
			return true, nil // an existing branch already at head: idempotent crash recovery.
		}
		if _, err := run("merge-base", "--is-ancestor", tip, head); err != nil {
			return false, errors.New("not a fast-forward (current tip is not an ancestor of job head)")
		}
	}
	worktrees, err := run("worktree", "list", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("check worktrees: %w", err)
	}
	for _, line := range strings.Split(worktrees, "\n") {
		if strings.EqualFold(line, "branch "+ref) {
			return false, errors.New("branch is checked out in a worktree")
		}
	}
	if _, err := run("update-ref", "--no-deref", ref, head, tip); err != nil {
		return false, fmt.Errorf("atomic branch update refused: %w", err)
	}
	return tip != "", nil
}

func branchOID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
