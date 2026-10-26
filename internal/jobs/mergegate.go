package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/qwwiwi/agent-jobs/internal/checklist"
	"github.com/qwwiwi/agent-jobs/internal/guard"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// The merge hook (Wave F, F9 (b)): `agent-jobs hook merge-gate`, a PreToolUse hook on Bash in the
// orchestrator session. In a repository with an approved brief/CHECKLIST.md, `gh pr merge` is allowed
// only with `--match-head-commit <full sha>` (GitHub refuses the merge if the head moved) and only when
// release_check passed on exactly that sha at the checklist as it is (store.ReleasePass); `--auto` is
// refused (auto-merge merges a later head), and so is a pass older than evidence release_check would fail
// on (a newer verdict for the sha, a red manifest-check run). A repository whose checklist was never
// approved is not gated; one whose approval is void now (the checklist or brief edited by hand or gone, a
// newer slicing check, an unreadable file) stays gated and is refused until it is approved again (fail
// closed: a void approval never lifts the gate) until the owner retires it (`agent-jobs gate retire`).
// Another clone of a gated repository (a shared OWNER/REPO remote) is gated with it: the names of a gated
// repository's remotes are stored (store.GateRepo, by checklist_set at the approval, by release_check on a
// pass and by the daemon's backfill at its start), so a gated work tree that is gone or moved is still
// matched by name and never makes other merges unreadable (one without stored names and with unreadable
// remotes makes a merge by name or in another work tree unsure, never one into an identified gated
// repository: that one is still checked). A gh pr merge written in the Bash command whose form or target
// the reader cannot tell passes with a one-line warning while some repository is gated (owner decision
// 2026-10-25: only a parsed gh pr merge in a gated repository without a release pass is blocked); a
// merge whose target resolves to a gated repository and whose only unreadable part is a variable or
// substitution word (the sha, the PR number) is refused: write them literally. A command that runs
// `agent-jobs gate retire` is always refused (RetireRefusal). It reads the store, git remotes and the
// brief files only.
// It sees only the command text: a script or program that runs gh pr merge itself (`make merge`,
// `./scripts/ship.sh`), `gh api` merges, `git push` to the base branch and the web UI pass by it, and so
// does the auto-merge or merge queue gh may enable on a protected branch (PLAN Wave F, Risks): it catches
// honest mistakes, it is not a boundary.

// MergeGatePrefix starts the hook's refusal line; MergeGateWarnPrefix its warning line (the command runs).
const (
	MergeGatePrefix     = "agent-jobs merge-gate: blocked: "
	MergeGateWarnPrefix = "agent-jobs merge-gate: warning: "
)

// fullSHA1Re is the full commit id --match-head-commit must carry (GitHub's ids are sha1).
var fullSHA1Re = regexp.MustCompile(`^[0-9a-f]{40}$`)

// PreToolUseInput is the part of Claude Code's PreToolUse-hook input the merge hook reads.
type PreToolUseInput struct {
	SessionID string
	Cwd       string
	ToolName  string
	// Command is tool_input.command of a Bash call ("" for another tool).
	Command string
}

// ParsePreToolUseInput reads the PreToolUse-hook JSON: a PreToolUse event with a tool_name; a Bash call
// must carry tool_input.command as a string.
func ParsePreToolUseInput(r io.Reader) (PreToolUseInput, error) {
	raw, err := readHookInput(r)
	if err != nil {
		return PreToolUseInput{}, err
	}
	if *raw.HookEventName != "PreToolUse" {
		return PreToolUseInput{}, fmt.Errorf("hook input: event %q, want PreToolUse", Printable(*raw.HookEventName, 64))
	}
	if raw.ToolName == nil || *raw.ToolName == "" {
		return PreToolUseInput{}, errors.New("hook input: PreToolUse event without tool_name")
	}
	in := PreToolUseInput{ToolName: *raw.ToolName}
	if raw.SessionID != nil {
		in.SessionID = *raw.SessionID
	}
	if raw.Cwd != nil {
		in.Cwd = *raw.Cwd
	}
	if in.ToolName != "Bash" {
		return in, nil
	}
	var ti struct {
		Command *string `json:"command"`
	}
	if len(raw.ToolInput) == 0 || json.Unmarshal(raw.ToolInput, &ti) != nil || ti.Command == nil {
		return PreToolUseInput{}, errors.New("hook input: Bash call without a tool_input.command string")
	}
	in.Command = *ti.Command
	return in, nil
}

// MergeGateNeeded reports whether the command runs, or may run, `gh pr merge`: only then does the hook
// open the store.
func MergeGateNeeded(command string) bool {
	calls, unsure := guard.GhPrMerges(command, "")
	return len(calls) > 0 || unsure != ""
}

// gatedRepo is the checklist state of one repository's main work tree.
type gatedRepo struct {
	// names are the OWNER/REPO names of a gated repository: its stored ones and those of its remotes now
	// (read once, namesRead); namesUnknown says it has no stored names and its remotes are unreadable
	// (approved before names were stored, its work tree gone before the backfill read it).
	names        []string
	namesRead    bool
	namesUnknown bool
	main         string
	// gated: the checklist is approved now, or it was approved once and the approval is void (void says
	// why; every merge is refused then).
	gated   bool
	void    string
	id      string
	version int
	hash    string
}

// mergeGate is one hook call: the repositories it has read, by main work tree.
type mergeGate struct {
	m     *Manager
	repos map[string]*gatedRepo
	all   []*gatedRepo // the gated stored repositories (nil = not read yet)
}

// MergeGateAnswer is the hook's answer: Refusal blocks the command ("" = it runs); Warning is one line
// for stderr when the command runs although the reader could not tell what it merges.
type MergeGateAnswer struct {
	Refusal string
	Warning string
}

// MergeGate is the hook's refusal line for a Bash command run at cwd ("" = allowed); see MergeGateCheck.
func (m *Manager) MergeGate(cwd, command, envRepo string) (string, error) {
	a, err := m.MergeGateCheck(cwd, command, envRepo)
	return a.Refusal, err
}

// MergeGateCheck is the hook's answer for a Bash command run at cwd. envRepo is GH_REPO of the session's
// environment. An error (unreadable store or brief files) is returned as is; the caller refuses on it.
func (m *Manager) MergeGateCheck(cwd, command, envRepo string) (MergeGateAnswer, error) {
	var a MergeGateAnswer
	calls, unsure := guard.GhPrMerges(command, cwd)
	if len(calls) == 0 && unsure == "" {
		return a, nil
	}
	g := &mergeGate{m: m, repos: map[string]*gatedRepo{}}
	warn := func(why string) error {
		w, err := g.warnUnsure(why)
		if a.Warning == "" {
			a.Warning = w
		}
		return err
	}
	if unsure != "" {
		if err := warn(unsure); err != nil {
			return MergeGateAnswer{}, err
		}
	}
	for _, call := range calls {
		if call.Unsure != "" && (call.Unsure != guard.DynamicWordUnsure || dynamicWord(call.Repo)) {
			if err := warn(call.Unsure); err != nil {
				return MergeGateAnswer{}, err
			}
			continue
		}
		// Every identified target is checked, also when another candidate is unreadable (why): an
		// unreadable other repository never lifts the gate of a known one.
		targets, why, err := g.targets(cwd, call, envRepo)
		if err != nil {
			return MergeGateAnswer{}, err
		}
		for _, t := range targets {
			if call.Unsure != "" {
				// Only a variable or substitution among the words the target does not depend on (the sha, the PR
				// number, a flag): the merge runs in this gated repository, so it is refused, not waved through.
				// Also with a literal sha that has a pass: the call does not tell which word is dynamic, and an
				// unquoted one may split into flags (`--body $B` with B="x --auto").
				return MergeGateAnswer{Refusal: dynamicWordRefusal(t)}, nil
			}
			if r, err := g.check(t, call); r != "" || err != nil {
				return MergeGateAnswer{Refusal: r}, err
			}
		}
		if call.Unsure != "" && why == "" {
			why = call.Unsure
		}
		if why != "" {
			if err := warn(why); err != nil {
				return MergeGateAnswer{}, err
			}
		}
	}
	return a, nil
}

// dynamicWord reports a word the shell expands ($VAR, ${VAR}, $(…), `…`).
func dynamicWord(w string) bool {
	return strings.ContainsAny(w, "$`")
}

// dynamicWordRefusal refuses a merge in gated repository r whose sha, PR number or another word is a
// variable or substitution: the hook cannot tell which sha, PR or flags it merges with.
func dynamicWordRefusal(r *gatedRepo) string {
	return MergeGatePrefix + fmt.Sprintf("gh pr merge in %s (a repo with an approved checklist) has a variable or "+
		"substitution in its words, so the hook cannot tell which sha, PR or flags it merges with, also when the "+
		"sha has a release_check pass; write every word of gh pr merge literally (no variables or $(...)).",
		Printable(r.main, 200))
}

// warnUnsure is the warning for a merge the hook cannot read while some stored repository has an
// approved checklist ("" when none has: nothing is gated). It no longer refuses (owner decision
// 2026-10-25): an unreadable form blocked harmless commands and caught no defect.
func (g *mergeGate) warnUnsure(why string) (string, error) {
	all, err := g.approvedRepos()
	if err != nil || len(all) == 0 {
		return "", err
	}
	names := make([]string, 0, len(all))
	for _, r := range all {
		names = append(names, Printable(r.main, 200))
	}
	return MergeGateWarnPrefix + "cannot tell whether this command runs gh pr merge in a repo with an approved checklist (" +
		Printable(why, 200) + "; gated: " + strings.Join(names, ", ") + "); not checked: run release_check before merging, and " +
		"write gh pr merge as a plain command with -R OWNER/REPO and --match-head-commit <full sha> to be checked.", nil
}

// targets are the gated repositories a call may merge in. A repository the call names (PR URL, -R) is
// the one gh uses; without one, every candidate counts (each GH_REPO value and every directory the call
// may run in, guard.GhMerge.Dirs), since the reader cannot tell which one gh takes. why is set when a
// candidate cannot be read; out still holds every gated repository identified (the caller checks them
// before it warns about why).
func (g *mergeGate) targets(cwd string, call guard.GhMerge, envRepo string) (out []*gatedRepo, why string, err error) {
	add := func(r *gatedRepo) {
		if r.gated && !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	unsure := func(w string) {
		if why == "" {
			why = w
		}
	}
	named := []string{call.Repo}
	if call.Repo == "" {
		named = append(slices.Clone(call.GHRepos), envRepo)
	}
	for _, n := range named {
		if n == "" {
			continue
		}
		name := gitHubRepoName(n)
		if name == "" {
			unsure(fmt.Sprintf("repository %q is not OWNER/REPO", n))
			continue
		}
		all, err := g.approvedRepos()
		if err != nil {
			return nil, "", err
		}
		for _, r := range all {
			names, err := g.gatedNames(r)
			if err != nil {
				return nil, "", err
			}
			if slices.Contains(names, name) {
				add(r)
			}
		}
		w, err := g.unknownNames()
		if err != nil {
			return nil, "", err
		}
		unsure(w)
	}
	if call.Repo != "" {
		return out, why, nil
	}
	if cwd == "" || !filepath.IsAbs(cwd) {
		unsure("the hook input has no absolute cwd")
		return out, why, nil
	}
	skipped := 0
	for _, d := range call.Dirs {
		if _, err := os.Stat(d); err != nil {
			// A relative cd from another candidate to a directory missing now fails there: the call does not
			// run in it. Else the command may create it before the call (git clone, mkdir, ln -s): what is
			// there then is unknown.
			if slices.Contains(call.RelDirs, d) {
				skipped++
				continue
			}
			unsure(fmt.Sprintf("the directory %s does not exist now", d))
			continue
		}
		// Inside a .git directory git has no work tree, gh still finds the repository's remotes.
		if i := strings.Index(d+"/", "/.git/"); i > 0 {
			d = d[:i]
		}
		_, main, err := ChecklistRepo(d)
		if err != nil {
			continue // not a git work tree: gh has no repository there
		}
		r, err := g.repo(main)
		if err != nil {
			return nil, "", err
		}
		if r.gated {
			add(r)
			continue
		}
		// Another clone of a gated repository merges the same pull requests: match it by its remotes.
		w, err := g.byRemotes(r, add)
		if err != nil {
			return nil, "", err
		}
		unsure(w)
		if w, err = g.unknownNames(); err != nil {
			return nil, "", err
		}
		unsure(w)
	}
	if skipped > 0 && skipped == len(call.Dirs) {
		unsure("every directory the call may run in is missing now (the command may create it before the call)")
	}
	return out, why, nil
}

// byRemotes adds every gated repository that shares an OWNER/REPO name with work tree r (not gated
// itself); why is set when r's remotes are unreadable (what the call merges is then unknown).
func (g *mergeGate) byRemotes(r *gatedRepo, add func(*gatedRepo)) (string, error) {
	all, err := g.approvedRepos()
	if err != nil || len(all) == 0 {
		return "", err
	}
	mine, err := remoteRepoNames(r.main)
	if err != nil {
		return fmt.Sprintf("the remotes of %s are unreadable: %v", r.main, err), nil
	}
	for _, gr := range all {
		names, err := g.gatedNames(gr)
		if err != nil {
			return "", err
		}
		if slices.ContainsFunc(mine, func(n string) bool { return slices.Contains(names, n) }) {
			add(gr)
		}
	}
	return "", nil
}

// gatedNames are gated repository r's OWNER/REPO names (cached): those stored for it and those of its
// remotes now. A work tree that is gone, moved or whose remotes are unreadable has its stored names only:
// it is never a reason to refuse a merge elsewhere.
func (g *mergeGate) gatedNames(r *gatedRepo) ([]string, error) {
	if r.namesRead {
		return r.names, nil
	}
	rec, err := g.m.Store.GateRepoOf(r.main)
	if err != nil {
		return nil, err
	}
	names := slices.Clone(rec.Names)
	live, err := remoteRepoNames(r.main)
	if err == nil {
		for _, n := range live {
			if !slices.Contains(names, n) {
				names = append(names, n)
			}
		}
	}
	r.names, r.namesRead, r.namesUnknown = names, true, len(rec.Names) == 0 && err != nil
	return names, nil
}

// unknownNames is why a merge that names a repository or runs in another work tree cannot be told apart
// from one in a gated repository whose names are unknown (gatedRepo.namesUnknown; "" = none is).
func (g *mergeGate) unknownNames() (string, error) {
	all, err := g.approvedRepos()
	if err != nil {
		return "", err
	}
	for _, r := range all {
		if _, err := g.gatedNames(r); err != nil {
			return "", err
		}
		if r.namesUnknown {
			return fmt.Sprintf("gated %s has no stored names and unreadable remotes: restore it and restart the daemon, "+
				"or retire it", Printable(r.main, 120)), nil
		}
	}
	return "", nil
}

// RecordGateNames stores the OWNER/REPO names of main's remotes for the merge hook (checklist_set calls it
// when it stores an approval, release_check on a pass, BackfillGateNames at the daemon's start; the hook
// itself opens the store read-only), so the hook still knows them once the work tree is gone or moved.
func (m *Manager) RecordGateNames(main string) error {
	names, err := remoteRepoNames(main)
	if err != nil || len(names) == 0 {
		return err
	}
	return m.Store.AddGateRepoNames(main, names)
}

// BackfillGateNames stores the remote names of every repository with a stored approval, not retired,
// whose main work tree is readable now (the daemon runs it at start): an approval stored before names
// were recorded is then matched by name once its work tree is gone. A work tree that is gone, moved or
// unreadable is skipped; a store error is returned.
func (m *Manager) BackfillGateNames() error {
	mains, err := m.Store.ApprovalRepos()
	if err != nil {
		return err
	}
	for _, main := range mains {
		last, err := m.Store.LatestApproval(main)
		if err != nil {
			return err
		}
		rec, err := m.Store.GateRepoOf(main)
		if err != nil {
			return err
		}
		if last == nil || last.Seq <= rec.RetiredSeq {
			continue
		}
		// Only the stored main work tree itself: a directory inside another repository is not it.
		if _, got, err := ChecklistRepo(main); err != nil || got != main {
			continue
		}
		names, err := remoteRepoNames(main)
		if err != nil || len(names) == 0 {
			continue
		}
		if err := m.Store.AddGateRepoNames(main, names); err != nil {
			return err
		}
	}
	return nil
}

// RetireRefusal is the hook's refusal of a Bash command that runs `agent-jobs gate retire` ("" = it does
// not, guard.RunsGateRetire): the owner retires a repository in a terminal outside the agent session.
func RetireRefusal(command string) string {
	if !guard.RunsGateRetire(command) {
		return ""
	}
	return MergeGatePrefix + "agent-jobs gate retire takes a repository out of the merge gate; only the owner runs it, " +
		"in a terminal outside the agent session."
}

// check is the refusal for a merge in gated repository r ("" = allowed).
func (g *mergeGate) check(r *gatedRepo, call guard.GhMerge) (string, error) {
	if r.void != "" {
		return MergeGatePrefix + fmt.Sprintf("gh pr merge in %s: its checklist approval is void (%s); approve the checklist "+
			"again (checklist_set) and run release_check on the sha first.", Printable(r.main, 200), RedactPrintable(r.void, 200)), nil
	}
	where := fmt.Sprintf("%s (checklist %s v%d is approved)", Printable(r.main, 200), Printable(r.id, 80), r.version)
	switch {
	case call.DisableAuto && !call.Auto:
		return "", nil // gh only turns auto-merge off
	case call.Auto:
		return MergeGatePrefix + "gh pr merge --auto in " + where + ": auto-merge merges a later head; merge without --auto, " +
			"with --match-head-commit <sha> of a commit release_check passed on.", nil
	case !call.HasMatchHead:
		return MergeGatePrefix + "gh pr merge in " + where + " without --match-head-commit <full sha>; add the 40-hex sha " +
			"release_check passed on.", nil
	}
	sha := strings.ToLower(call.MatchHead)
	if !fullSHA1Re.MatchString(sha) {
		return MergeGatePrefix + fmt.Sprintf("gh pr merge in %s: --match-head-commit %q is not a full 40-hex sha.", where,
			Printable(call.MatchHead, 80)), nil
	}
	p, err := g.m.Store.ReleasePassed(r.main, sha, r.id, r.version, r.hash)
	if err != nil {
		return "", err
	}
	if p == nil {
		return MergeGatePrefix + fmt.Sprintf("gh pr merge in %s: release_check has not passed on %s at this checklist; "+
			"run release_check(repo, sha) first.", where, sha), nil
	}
	// Evidence stored after the pass that release_check would fail on: a newer verdict for the sha, or a
	// red manifest-check run. The pass then no longer counts until release_check runs again.
	stale := ""
	if red, err := g.m.Store.RedManifestCheck(r.main, sha, r.id, r.version, r.hash); err != nil {
		return "", err
	} else if red != nil {
		stale = fmt.Sprintf("manifest-check of job %s exited %d after it", red.JobID, red.ExitCode)
	}
	vs, err := g.m.Store.VerdictsAt(r.main, r.id, r.version, sha)
	if err != nil {
		return "", err
	}
	latest := ""
	for _, v := range vs {
		if v.ChecklistSHA256 == r.hash {
			latest = v.JobID
		}
	}
	if stale == "" && latest != p.VerdictJob {
		stale = fmt.Sprintf("the latest verdict for the sha is of job %s, the pass read job %s", latest, p.VerdictJob)
	}
	if stale != "" {
		return MergeGatePrefix + fmt.Sprintf("gh pr merge in %s: the release_check pass on %s is stale (%s); run "+
			"release_check(repo, sha) again.", where, sha, RedactPrintable(stale, 200)), nil
	}
	return "", nil
}

// repo reads the checklist state of a main work tree (cached), without the checklist lock like the
// brief reminder: a read that races a checklist_set can misjudge one call. A repository never approved,
// or retired by the owner since its latest approval, is not gated; one approved once stays gated (void
// when its checklist is not approved now, also when the file or the work tree is gone); a store error is
// returned.
func (g *mergeGate) repo(main string) (*gatedRepo, error) {
	if r, ok := g.repos[main]; ok {
		return r, nil
	}
	last, err := g.m.Store.LatestApproval(main)
	if err != nil {
		return nil, err
	}
	r := &gatedRepo{main: main}
	if last != nil {
		rec, err := g.m.Store.GateRepoOf(main)
		if err != nil {
			return nil, err
		}
		if last.Seq <= rec.RetiredSeq {
			g.repos[main] = r // retired: out of the gate until it is approved again
			return r, nil
		}
	}
	voided := func(why string) (*gatedRepo, error) {
		r.gated, r.void = last != nil, why
		g.repos[main] = r
		return r, nil
	}
	f, err := checklist.Read(main)
	switch {
	case err != nil:
		return voided("the brief files are unreadable: " + err.Error())
	case f.Checklist == nil && last == nil:
		g.repos[main] = r
		return r, nil
	case f.Checklist == nil:
		return voided("the work tree has no " + checklist.ChecklistRel)
	case f.Brief == nil:
		return voided("the work tree has no " + checklist.BriefRel)
	}
	text, _ := Redact(string(f.Checklist))
	c, err := checklist.Parse(text)
	if err != nil {
		return voided(checklist.ChecklistRel + " does not parse: " + err.Error())
	}
	approved, hash, reason, _, err := g.m.ChecklistStatus(main, c, f.Brief)
	if err != nil {
		return nil, err
	}
	r.id, r.version, r.hash = c.ID, c.Version, hash
	if !approved {
		return voided(reason)
	}
	r.gated = true
	g.repos[main] = r
	return r, nil
}

// approvedRepos are the gated repositories among those with a stored approval.
func (g *mergeGate) approvedRepos() ([]*gatedRepo, error) {
	if g.all != nil {
		return g.all, nil
	}
	mains, err := g.m.Store.ApprovalRepos()
	if err != nil {
		return nil, err
	}
	all := []*gatedRepo{}
	for _, main := range mains {
		r, err := g.repo(main)
		if err != nil {
			return nil, err
		}
		if r.gated {
			all = append(all, r)
		}
	}
	g.all = all
	return all, nil
}

// gitHubRepoName is the lower-cased OWNER/REPO of a repository as gh takes it (OWNER/REPO,
// HOST/OWNER/REPO, a URL, an scp-like git remote), "" when it names none.
func gitHubRepoName(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
		if at := strings.LastIndex(strings.SplitN(s, "/", 2)[0], "@"); at >= 0 {
			s = s[at+1:]
		}
	} else if host, path, ok := strings.Cut(s, ":"); ok && !strings.Contains(host, "/") {
		s = host + "/" + path // git@github.com:owner/repo.git
	}
	s = strings.TrimSuffix(strings.TrimSuffix(s, "/"), ".git")
	parts := strings.Split(s, "/")
	if len(parts) < 2 {
		return ""
	}
	owner, name := parts[len(parts)-2], parts[len(parts)-1]
	if owner == "" || name == "" || len(parts) > 3 {
		return ""
	}
	return strings.ToLower(owner + "/" + name)
}

// remoteRepoNames are the OWNER/REPO names of a work tree's remotes.
func remoteRepoNames(dir string) ([]string, error) {
	out, err := git(dir, "remote", "-v")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		if n := gitHubRepoName(f[1]); n != "" && !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	return names, nil
}

// RetireGate takes a repository out of the merge gate (owner CLI `agent-jobs gate retire`, no MCP tool):
// path is its main work tree as stored, or any work tree of it while one exists. It returns the stored
// repository; store.ErrNotGated when none of path's forms has a stored approval. A later approval of the
// repository gates it again.
func (m *Manager) RetireGate(path string) (string, error) {
	if path == "" || !filepath.IsAbs(path) {
		return "", fmt.Errorf("repo %q must be an absolute path", path)
	}
	forms := []string{filepath.Clean(path)}
	if _, main, err := ChecklistRepo(path); err == nil && main != forms[0] {
		forms = append([]string{main}, forms...)
	}
	for _, repo := range forms {
		_, err := m.Store.RetireGateRepo(repo)
		if errors.Is(err, store.ErrNotGated) {
			continue
		}
		return repo, err
	}
	return "", fmt.Errorf("%s: %w", Printable(path, 200), store.ErrNotGated)
}
