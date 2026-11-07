package jobs

import (
	"errors"
	"fmt"
	"strings"
)

// branchGit runs git in one repository, with the daemon's timeouts and safe environment.
// Keeping the ref operations independent of the store also permits offline Git tests.
type branchGit func(args ...string) (string, error)

// validateResultBranch checks a caller-supplied result target at start time.
func validateResultBranch(run branchGit, name string) error {
	return validateBranchTarget(run, name, false)
}

// validateBranchTarget refuses invalid and protected names. generated marks the job/<id> fallback
// the daemon creates itself: it needs no knowledge of the default branch, so it also works in a
// detached repository without metadata. Any other name is refused unless the default branch is
// identified, because a requested name could be that branch.
func validateBranchTarget(run branchGit, name string, generated bool) error {
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
	declared := false
	if configured, err := run("config", "--get", "init.defaultBranch"); err == nil && configured != "" {
		declared = true
		if strings.EqualFold(name, configured) {
			return fmt.Errorf("protected default branch %q", name)
		}
	}
	// The main worktree's current branch is always protected too, but it is no proof of the
	// default: a repository on a feature branch, with the default elsewhere and no remote HEAD,
	// would otherwise let a request move the real default. Fail closed for requested names until
	// a remote HEAD or an explicit init.defaultBranch identifies the default.
	if current, err := run("symbolic-ref", "-q", "HEAD"); err == nil && strings.EqualFold(current, "refs/heads/"+name) {
		return fmt.Errorf("protected current branch %q", name)
	}
	if !generated && !knownDefault && !declared {
		return errors.New("result branch refused: the default branch cannot be identified (no remote HEAD metadata and no init.defaultBranch); run git remote set-head or set init.defaultBranch")
	}
	return nil
}

// publishBranchRef creates a branch or advances it only from an ancestor of head.
// The expected old OID makes the write atomic against other jobs and owner actions.
// It never follows a symbolic ref or changes a checked-out worktree's branch.
//
// generated marks the daemon's own job/<id> fallback, which needs no default-branch knowledge;
// every other name is validated again, since the repository may have changed since start.
func publishBranchRef(run branchGit, name, head string, generated bool) (fastForwarded bool, err error) {
	if err := validateBranchTarget(run, name, generated); err != nil {
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
	if err := refNotCheckedOut(run, ref); err != nil {
		return false, err
	}
	if _, err := run("update-ref", "--no-deref", ref, head, tip); err != nil {
		return false, fmt.Errorf("atomic branch update refused: %w", err)
	}
	// update-ref knows nothing about worktrees, and a checkout of the unchanged tip does not fail
	// the expected-old-OID check. Look again now that the ref has moved: a worktree that switched to
	// it in between would sit on a new tip with its old index and files, so put the ref back.
	if err := refNotCheckedOut(run, ref); err != nil {
		rollback := []string{"update-ref", "--no-deref", ref, tip, head}
		if tip == "" {
			rollback = []string{"update-ref", "--no-deref", "-d", ref, head}
		}
		if _, rbErr := run(rollback...); rbErr != nil {
			return false, fmt.Errorf("%w; restoring the branch failed: %v", err, rbErr)
		}
		return false, err
	}
	return tip != "", nil
}

// refNotCheckedOut fails when any worktree (the main one included) has ref checked out.
func refNotCheckedOut(run branchGit, ref string) error {
	worktrees, err := run("worktree", "list", "--porcelain")
	if err != nil {
		return fmt.Errorf("check worktrees: %w", err)
	}
	for _, line := range strings.Split(worktrees, "\n") {
		if strings.EqualFold(line, "branch "+ref) {
			return errors.New("branch is checked out in a worktree")
		}
	}
	return nil
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
