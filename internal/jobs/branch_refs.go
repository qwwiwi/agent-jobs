package jobs

import (
	"errors"
	"fmt"
	"strings"
)

// branchGit runs git in one repository, with the daemon's timeouts and safe environment.
// Keeping the ref operations independent of the store also permits offline Git tests.
type branchGit func(args ...string) (string, error)

// errBranchExists reports that the branch to create is already taken by another ref (or by this
// job's own earlier publication of a different commit). Result branches are create-only: the daemon
// never moves an existing ref, so it can never disturb a branch somebody else has checked out,
// advanced or relies on, and no default-branch knowledge is needed.
var errBranchExists = errors.New("branch already exists")

// validateResultBranch checks a caller-supplied result name statically: a valid branch name written
// literally, and not one of the protected names (main, master, release*). Whether the name is free
// is decided when the ref is created.
func validateResultBranch(run branchGit, name string) error {
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
	return nil
}

// publishBranchRef creates refs/heads/<name> at head and nothing else. The expected old value is
// empty (update-ref: the ref must not exist), so the write is atomic against every other writer: of
// concurrent creators exactly one wins, the others get errBranchExists. An existing ref is never
// moved, followed or rolled back. A retry that finds the ref already at head (a crash between the
// write and the job row update) succeeds without writing; created is false then.
func publishBranchRef(run branchGit, name, head string) (created bool, err error) {
	if err := validateResultBranch(run, name); err != nil {
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
	if exists, atHead := existingRef(run, ref, head); exists {
		if atHead {
			return false, nil
		}
		return false, errBranchExists
	}
	// An unborn branch that a worktree has checked out has no ref to collide with; do not make one
	// appear under that worktree.
	if err := refNotCheckedOut(run, ref); err != nil {
		return false, err
	}
	_, writeErr := run("update-ref", "--no-deref", ref, head, "")
	if writeErr == nil {
		return true, nil
	}
	// The write failed: either another writer created the ref meanwhile (a legitimate outcome) or
	// something else is wrong (a directory/file name conflict, a lock, a read-only repository).
	if exists, atHead := existingRef(run, ref, head); exists {
		if atHead {
			return false, nil
		}
		return false, errBranchExists
	}
	return false, fmt.Errorf("create branch: %w", writeErr)
}

// existingRef reports whether ref exists (a symbolic ref counts, dangling or not, and is never
// "at head": it must not be adopted) and whether it points at head.
func existingRef(run branchGit, ref, head string) (exists, atHead bool) {
	if _, err := run("symbolic-ref", "-q", ref); err == nil {
		return true, false
	}
	tip, err := run("rev-parse", "--verify", "-q", "--end-of-options", ref)
	if err != nil {
		return false, false
	}
	return true, tip == head
}

// publishResultRefs publishes the job head under the requested name when it is free, and under
// job/<id> otherwise (or when no name was requested). It returns the branch that holds the result
// and a note when the request was not honoured. It writes one ref only.
func publishResultRefs(run branchGit, id, requested, head string) (branch, note string, err error) {
	generated := "job/" + id
	if requested != "" && requested != generated {
		_, err := publishBranchRef(run, requested, head)
		if err == nil {
			return requested, "", nil
		}
		if errors.Is(err, errBranchExists) {
			note = fmt.Sprintf("requested branch %s exists; not moved - published %s; fast-forward it yourself with git merge --ff-only",
				requested, generated)
		} else {
			note = fmt.Sprintf("requested branch %s not created: %s - published %s", requested, RedactPrintable(err.Error(), 240), generated)
		}
	}
	if _, err := publishBranchRef(run, generated, head); err != nil {
		return "", "", err
	}
	return generated, note, nil
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
