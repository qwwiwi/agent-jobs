package jobs

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// A checklist whose release shipped stops gating merges (#99): once release_check passed at the latest
// approval (its id, version and hash, checked at that approval: the pass's ApprovalSeq is its seq) on a
// sha that is on the default branch of the repository's origin, the daemon's tick records the approval
// as shipped (store.GateRepo.ShippedSeq, the sha and when), and the merge hook takes the repository out
// of the gate like a retirement: later fixes merge under the normal review rule. The tick computes it,
// not the hook: the hook opens the store read-only and stays cheap, and doctor/status show the stored
// record. It never lifts the gate on its own: the hook still requires the approval to be the latest and
// valid (a void approval stays gated), and the pass to be stored and not stale (a newer verdict or a red
// manifest-check run); a new approval, also an unchanged re-approval, gates the repository again until a
// pass at it ships in turn. The daemon does not fetch: it reads the remote-tracking ref in the main work
// tree (refs/remotes/origin/HEAD, else origin/main, else origin/master), so the merge counts once
// someone fetched origin there (git fetch, git pull).

// Merge-gate states of a repository with a stored approval (MergeGateView.State).
const (
	MergeGateGated   = "gated"
	MergeGateVoid    = "void"
	MergeGateRetired = "retired"
	MergeGateShipped = "shipped"
)

// MergeGateView is the merge-gate state of one repository with a stored approval (doctor, status).
type MergeGateView struct {
	// Repo is the main work tree as stored.
	Repo        string `json:"repo"`
	ChecklistID string `json:"checklist_id"`
	Version     int    `json:"version"`
	// State is gated, void (still gated: every merge is refused), retired (by the owner) or shipped.
	State string `json:"state"`
	// Reason is why a void approval is void.
	Reason string `json:"reason,omitempty"`
	// ShippedSHA is the sha release_check passed on that is on origin's default branch; ShippedAt when
	// the daemon saw it there (RFC 3339).
	ShippedSHA string `json:"shipped_sha,omitempty"`
	ShippedAt  string `json:"shipped_at,omitempty"`
}

// MergeGateText is a view as one line.
func MergeGateText(v MergeGateView) string {
	s := fmt.Sprintf("%s: checklist %s v%d %s", Printable(v.Repo, 200), Printable(v.ChecklistID, 80), v.Version, v.State)
	switch v.State {
	case MergeGateShipped:
		s += fmt.Sprintf(" (release_check passed on %s, on origin's default branch since %s; merges are not gated "+
			"until a new approval)", v.ShippedSHA, v.ShippedAt)
	case MergeGateVoid:
		s += " (" + v.Reason + "; every merge is refused until it is approved again)"
	case MergeGateRetired:
		s += " (by the owner; a new approval gates it again)"
	}
	return s
}

// MergeGates is the merge-gate state of every repository with a stored approval, sorted by repository.
func (m *Manager) MergeGates() ([]MergeGateView, error) {
	mains, err := m.Store.ApprovalRepos()
	if err != nil {
		return nil, err
	}
	g := &mergeGate{m: m, repos: map[string]*gatedRepo{}}
	out := []MergeGateView{}
	for _, main := range mains {
		last, err := m.Store.LatestApproval(main)
		if err != nil {
			return nil, err
		}
		r, err := g.repo(main)
		if err != nil {
			return nil, err
		}
		v := MergeGateView{Repo: main, ChecklistID: r.id, Version: r.version}
		if v.ChecklistID == "" && last != nil {
			v.ChecklistID, v.Version = last.ChecklistID, last.Version
		}
		switch {
		case r.retired:
			v.State = MergeGateRetired
		case r.shipped != "":
			v.State, v.ShippedSHA, v.ShippedAt = MergeGateShipped, r.shipped, r.shippedAt.UTC().Format(time.RFC3339)
		case r.void != "":
			v.State, v.Reason = MergeGateVoid, RedactPrintable(r.void, 200)
		case r.gated:
			v.State = MergeGateGated
		default:
			continue // no approval after all (cannot happen for a stored one)
		}
		out = append(out, v)
	}
	return out, nil
}

// MarkShippedGates records every repository whose latest approval shipped (the daemon's tick runs it):
// the approval is valid now, and release_check passed at it, with a pass that still counts, on a sha on
// origin's default branch (shippedOn). A retired or already shipped approval, one without a pass and a
// work tree that is gone or has no fetched origin are skipped; store errors are returned.
func (m *Manager) MarkShippedGates() error {
	mains, err := m.Store.ApprovalRepos()
	if err != nil {
		return err
	}
	g := &mergeGate{m: m, repos: map[string]*gatedRepo{}}
	var errs []error
	for _, main := range mains {
		if err := m.markShipped(g, main); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", Printable(main, 200), err))
		}
	}
	return errors.Join(errs...)
}

// markShipped is MarkShippedGates for one repository.
func (m *Manager) markShipped(g *mergeGate, main string) error {
	last, err := m.Store.LatestApproval(main)
	if err != nil || last == nil {
		return err
	}
	rec, err := m.Store.GateRepoOf(main)
	if err != nil {
		return err
	}
	if last.Seq <= rec.RetiredSeq {
		return nil
	}
	if last.Seq == rec.ShippedSeq {
		// Shipped already, while its pass still counts; a pass dropped or stale since (release_check ran
		// again) is re-read below: another pass of the approval may have shipped.
		ok, err := m.passCounts(&gatedRepo{main: main, id: last.ChecklistID, version: last.Version, hash: last.ChecklistSHA256,
			seq: last.Seq}, rec.ShippedSHA)
		if err != nil || ok {
			return err
		}
	}
	// Cheap first: without a pass at the approval nothing can ship (no files or git read).
	passes, err := m.passesAt(main, last.ChecklistID, last.Version, last.ChecklistSHA256, last.Seq)
	if err != nil || len(passes) == 0 {
		return err
	}
	r, err := g.repo(main)
	if err != nil {
		return err
	}
	// Only a valid approval, the latest one, ships: a void one stays gated.
	if !r.gated || r.void != "" || r.seq != last.Seq || r.id != last.ChecklistID || r.version != last.Version ||
		r.hash != last.ChecklistSHA256 {
		return nil
	}
	ref, err := originDefaultRef(main)
	if err != nil {
		return nil // nothing fetched from origin: nothing is known to be on its default branch
	}
	for i := range passes {
		p := &passes[i]
		stale, err := m.passStale(r, p)
		if err != nil {
			return err
		}
		if stale != "" || !shippedOn(main, p.SHA, ref) {
			continue
		}
		if err := m.Store.MarkGateShipped(main, last.Seq, p.SHA, m.Now()); err != nil {
			return err
		}
		m.Log.Printf("merge gate: %s checklist %s v%d shipped (release_check passed on %s, on %s); merges are not gated "+
			"until a new approval", main, last.ChecklistID, last.Version, p.SHA, ref)
		return nil
	}
	return nil
}

// originDefaultRef is the remote-tracking ref of origin's default branch in work tree dir:
// refs/remotes/origin/HEAD (what origin names its default branch), else origin/main, else origin/master;
// an error when none is there (nothing fetched).
func originDefaultRef(dir string) (string, error) {
	if ref, err := gitHistory(dir, "symbolic-ref", "-q", "refs/remotes/origin/HEAD"); err == nil && strings.HasPrefix(ref, "refs/remotes/origin/") {
		if _, err := gitHistory(dir, "rev-parse", "-q", "--verify", ref+"^{commit}"); err == nil {
			return ref, nil
		}
	}
	for _, ref := range []string{"refs/remotes/origin/main", "refs/remotes/origin/master"} {
		if _, err := gitHistory(dir, "rev-parse", "-q", "--verify", ref+"^{commit}"); err == nil {
			return ref, nil
		}
	}
	return "", errors.New("no refs/remotes/origin/HEAD, origin/main or origin/master")
}

// shippedOn reports whether sha is on ref in work tree dir: an ancestor of it (a merge commit, a fast
// forward), or a commit on ref that sha does not reach has sha's exact tree (a squash or rebase merge of
// a branch that was up to date with its base). Anything unreadable (a sha not in the object store) is
// not shipped. History is read as committed (gitHistory): neither a replace ref nor a grafts file can
// fake the ancestry or the tree.
func shippedOn(dir, sha, ref string) bool {
	if !commitSHARe.MatchString(sha) {
		return false
	}
	_, err := gitHistory(dir, "merge-base", "--is-ancestor", sha, ref)
	if err == nil {
		return true
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 1 {
		return false
	}
	tree, err := gitHistory(dir, "rev-parse", "-q", "--verify", sha+"^{tree}")
	if err != nil || tree == "" {
		return false
	}
	out, err := gitHistory(dir, "log", "--format=%T", ref, "^"+sha)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(out, "\n") {
		if line == tree {
			return true
		}
	}
	return false
}

// gitHistory is git for the shipped check's history reads in work tree dir, which see the commits as
// they are: replace refs are ignored (noReplace, as release_check reads the sha) and so is a grafts file
// (<git dir>/info/grafts rewrites a commit's parents, which --no-replace-objects leaves in effect):
// GIT_GRAFT_FILE names an empty file, set after daemonGitEnv dropped the inherited GIT_* variables.
func gitHistory(dir string, args ...string) (string, error) {
	return gitEnv(dir, append(daemonGitEnv(), "GIT_GRAFT_FILE="+os.DevNull), append([]string{noReplace}, args...)...)
}

// shippedPass is a pass of gated repository r that could ship it but has not yet ("" = none): the
// newest pass at r's approval that still counts. The hook's refusal names it with what lifts the gate.
func (g *mergeGate) shippedPass(r *gatedRepo) (string, error) {
	passes, err := g.m.passesAt(r.main, r.id, r.version, r.hash, r.seq)
	if err != nil {
		return "", err
	}
	for i := range passes {
		stale, err := g.m.passStale(r, &passes[i])
		if err != nil {
			return "", err
		}
		if stale == "" {
			return passes[i].SHA, nil
		}
	}
	return "", nil
}

// passesAt are the passes of repository main at the checklist id, version and hash that release_check
// checked at approval seq (store.ReleasePass.ApprovalSeq), newest first: a pass of the approval before an
// unchanged re-approval (the same id, version and hash, a new seq) does not ship it, whenever it was
// stored, nor does a pass stored before the seq was recorded (0).
func (m *Manager) passesAt(main, id string, version int, hash string, seq int64) ([]store.ReleasePass, error) {
	if seq == 0 {
		return nil, nil
	}
	passes, err := m.Store.ReleasePassesAt(main, id, version, hash)
	if err != nil {
		return nil, err
	}
	out := passes[:0]
	for _, p := range passes {
		if p.ApprovalSeq == seq {
			out = append(out, p)
		}
	}
	return out, nil
}

// passCounts reports whether repository r's stored pass on sha at its approval (its id, version, hash
// and seq) exists and is not stale.
func (m *Manager) passCounts(r *gatedRepo, sha string) (bool, error) {
	p, err := m.Store.ReleasePassed(r.main, sha, r.id, r.version, r.hash)
	if err != nil || p == nil || r.seq == 0 || p.ApprovalSeq != r.seq {
		return false, err
	}
	stale, err := m.passStale(r, p)
	return stale == "", err
}

// passStale is why pass p of repository r no longer counts ("" = it counts): evidence stored after it
// that release_check would fail on, a red manifest-check run or a newer verdict for the sha.
func (m *Manager) passStale(r *gatedRepo, p *store.ReleasePass) (string, error) {
	if red, err := m.Store.RedManifestCheck(r.main, p.SHA, r.id, r.version, r.hash); err != nil {
		return "", err
	} else if red != nil {
		return fmt.Sprintf("manifest-check of job %s exited %d after it", red.JobID, red.ExitCode), nil
	}
	vs, err := m.Store.VerdictsAt(r.main, r.id, r.version, p.SHA)
	if err != nil {
		return "", err
	}
	latest := ""
	for _, v := range vs {
		if v.ChecklistSHA256 == r.hash {
			latest = v.JobID
		}
	}
	if latest != p.VerdictJob {
		return fmt.Sprintf("the latest verdict for the sha is of job %s, the pass read job %s", latest, p.VerdictJob), nil
	}
	return "", nil
}

// passLiveChecks are the suspect tests of the verdicts pass p counted (those for its sha at gated
// repository r's checklist id, version and hash; liveChecks): the merge runs with a needs-live-check
// warning when there are any (#106).
func (m *Manager) passLiveChecks(r *gatedRepo, p *store.ReleasePass) ([]string, error) {
	vs, err := m.Store.VerdictsAt(r.main, r.id, r.version, p.SHA)
	if err != nil {
		return nil, err
	}
	var counted []*store.JudgeVerdict
	for _, v := range vs {
		if v.ChecklistSHA256 == r.hash {
			counted = append(counted, v)
		}
	}
	return liveChecks(counted), nil
}
