package mcp

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qwwiwi/agent-jobs/internal/checklist"
	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// releaseFix is a repository with an approved checklist (R01 live, R02 live placeholder-ok, R03
// deferred) and what release_check reads, stored as the wrapper and the daemon store them.
type releaseFix struct {
	m    *jobs.Manager
	cs   *sdk.ClientSession
	repo string
	head string
	v    ChecklistView
}

func newReleaseFix(t *testing.T) *releaseFix {
	t.Helper()
	m := newManager(t)
	repo, head := gitRepo(t)
	f := &releaseFix{m: m, cs: connect(t, mustServer(t, m)), repo: repo, head: head}
	call(t, f.cs, "checklist_init", map[string]any{"repo": repo, "brief": "I want a login page and a logout button, later a report."}, &f.v)
	call(t, f.cs, "checklist_set", map[string]any{"repo": repo, "rows": []map[string]any{
		{"id": "R01", "verbatim": "a login page", "contract": "live", "rationale": "brief"},
		{"id": "R02", "verbatim": "a logout button", "contract": "live", "placeholder_ok": true, "rationale": "brief"},
		{"id": "R03", "verbatim": "later a report", "contract": "deferred", "rationale": "owner: «later»"}}}, &f.v)
	storeSlicing(t, m, repo)
	call(t, f.cs, "checklist_set", map[string]any{"repo": repo, "approve": true, "approve_quote": "yes"}, &f.v)
	if !f.v.Approved || !slices.Equal(f.v.Live, []string{"R01", "R02"}) {
		t.Fatalf("fixture checklist: %+v", f.v)
	}
	return f
}

var releaseSeq int

// coder stores a succeeded feature job (kind general, size large) of the checklist with covers, as
// job_start stores one.
func (f *releaseFix) coder(t *testing.T, runner, model string, covers ...string) string {
	t.Helper()
	return f.coderStatus(t, store.StatusSucceeded, runner, model, covers...)
}

// coderStatus stores a feature job of the checklist that ended with status.
func (f *releaseFix) coderStatus(t *testing.T, status, runner, model string, covers ...string) string {
	t.Helper()
	return f.coderSized(t, jobs.SizeLarge, status, runner, model, covers...)
}

// coderSized stores a general job of the repository with covers of the checklist and the given size.
func (f *releaseFix) coderSized(t *testing.T, size, status, runner, model string, covers ...string) string {
	t.Helper()
	releaseSeq++
	id := fmt.Sprintf("coder-%d", releaseSeq)
	if err := f.m.Store.Insert(&store.Job{ID: id, Agent: f.m.Agent, Title: "coder", Kind: "general", Brief: "b", RepoPath: f.repo,
		Status: status, Model: model, Runner: runner, MaxTurns: 1, MaxBudgetUSD: 1, TimeoutMinutes: 1, Size: size,
		CreatedAt: time.Now(), Covers: covers, CoverContract: &store.CoverContract{ChecklistID: f.v.ID, Version: f.v.Version}}); err != nil {
		t.Fatal(err)
	}
	return id
}

// judged is what one judge job leaves: its manifest-check record and its verdict.
type judged struct {
	exit       int
	noManifest bool
	manifestAt string // another sha for the record
	verdicts   map[string]string
	unmapped   []string
	suspect    []string
	runner     string
	model      string
	blind      bool
	kind       string // the job's kind (default judge)
	status     string // how the job's row ends (default succeeded; running leaves it running)
	hash       string // another checklist hash for the verdict
	reattempt  bool   // the row ends at a later attempt than the verdict's
	noVerdict  bool   // the job stores its manifest-check record but no verdict
}

func okJudge() judged {
	return judged{verdicts: map[string]string{"R01": "met", "R02": "placeholder"}, runner: store.RunnerClaude, model: "opus", blind: true}
}

// judge stores a judge job's records for the fixture's head, under the job's launch token.
func (f *releaseFix) judge(t *testing.T, j judged) string {
	t.Helper()
	kind := j.kind
	if kind == "" {
		kind = jobs.KindJudge
	}
	id, token := gateJobRow(t, f.m, kind)
	hash := f.v.SHA256
	if j.hash != "" {
		hash = j.hash
	}
	if !j.noManifest {
		sha := f.head
		if j.manifestAt != "" {
			sha = j.manifestAt
		}
		if _, err := f.m.Store.PutManifestCheck(token, store.ManifestCheck{JobID: id, Repo: f.repo, SHA: sha, ChecklistID: f.v.ID,
			Version: f.v.Version, ChecklistSHA256: hash, Script: jobs.ManifestScript, ExitCode: j.exit, Output: "R01 ok",
			CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	if j.noVerdict {
		f.finish(t, id, token, j.status)
		return id
	}
	o := jobs.JudgeOutput{Schema: jobs.JudgeSchema, Checklist: jobs.GateChecklistRef{ID: f.v.ID, Version: f.v.Version, SHA256: f.v.SHA256},
		SHA: f.head, Ran: []jobs.JudgeRun{}, CouldNotRun: []string{}, UnmappedBrief: nonNil(j.unmapped), ExtraScope: []string{},
		SuspectTests: nonNil(j.suspect)}
	for _, rid := range []string{"R01", "R02"} {
		if verdict, ok := j.verdicts[rid]; ok {
			o.Requirements = append(o.Requirements, jobs.JudgeRequirement{ID: rid, Verdict: verdict, How: "read",
				Evidence: []string{"main.go:1"}, Note: "NOTE_" + rid})
		}
	}
	if _, err := f.m.Store.PutVerdict(token, store.JudgeVerdict{JobID: id, Repo: f.repo, SHA: f.head, ChecklistID: f.v.ID,
		Version: f.v.Version, ChecklistSHA256: hash, JudgeModel: j.model, JudgeRunner: j.runner,
		JudgeFamily: jobs.ModelFamily(j.runner, j.model), EnforcedBlind: j.blind, Counts: map[string]int{}, Verdict: o,
		CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if j.reattempt {
		// The attempt fails after storing its verdict; the next attempt (no verdict of its own) succeeds.
		if err := f.m.Store.Finish(id, token, store.StatusFailed, 0, 0, 0, "x", time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := f.m.Store.Requeue(id, store.ResumeLimits{MaxTurns: 1, MaxBudgetUSD: 1, TimeoutMinutes: 1}, "", ""); err != nil {
			t.Fatal(err)
		}
		token = token + "-2"
		if err := f.m.Store.Claim(id, token, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	f.finish(t, id, token, j.status)
	return id
}

// finish ends a judge job's row with status (default succeeded; running leaves it running).
func (f *releaseFix) finish(t *testing.T, id, token, status string) {
	t.Helper()
	switch status {
	case store.StatusRunning:
	case "":
		status = store.StatusSucceeded
		fallthrough
	default:
		if err := f.m.Store.Finish(id, token, status, 0, 0, 0, "", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
}

func nonNil(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
}

func (f *releaseFix) check(t *testing.T) jobs.ReleaseCheckResult {
	t.Helper()
	var out jobs.ReleaseCheckResult
	call(t, f.cs, "release_check", map[string]any{"repo": f.repo, "sha": f.head}, &out)
	return out
}

func condition(r jobs.ReleaseCheckResult, name string) jobs.ReleaseCondition {
	for _, c := range r.Conditions {
		if c.Name == name {
			return c
		}
	}
	return jobs.ReleaseCondition{Name: name, Detail: "absent"}
}

// passing builds the fixture that passes: a codex coder covering both live requirements, a claude
// judge (another family, blind by enforcement), manifest-check exit 0, R01 met, R02 placeholder on a
// placeholder-ok row.
func passing(t *testing.T) *releaseFix {
	t.Helper()
	f := newReleaseFix(t)
	f.coder(t, store.RunnerCodex, "gpt-5.6-sol", "R01", "R02")
	f.judge(t, okJudge())
	return f
}

// release_check passes only when every condition holds; the structured result lists each condition,
// each live requirement and the feature jobs, and the owner line reads «Acceptance: N of M».
func TestReleaseCheckPasses(t *testing.T) {
	f := passing(t)
	r := f.check(t)
	if !r.Pass || r.OwnerLine != "Acceptance: 2 of 2" || r.Accepted != 2 || r.Live != 2 || len(r.Mismatches) != 0 ||
		r.ChecklistID != f.v.ID || r.Version != f.v.Version || r.ChecklistSHA256 != f.v.SHA256 || r.SHA != f.head ||
		r.ManifestCheck == nil || r.ManifestCheck.ExitCode != 0 || r.Verdicts != 1 || r.JudgeFamily != "anthropic" {
		t.Fatalf("release_check: %+v", r)
	}
	if len(r.Conditions) != len(jobs.ReleaseConditions) {
		t.Fatalf("conditions: %+v", r.Conditions)
	}
	for _, c := range r.Conditions {
		if !c.OK {
			t.Fatalf("condition %s failed: %s", c.Name, c.Detail)
		}
	}
	if len(r.Requirements) != 2 || !r.Requirements[1].Accepted || !r.Requirements[1].PlaceholderOK || r.Requirements[1].Verdict != "placeholder" ||
		len(r.FeatureJobs) != 1 || !slices.Equal(r.FeatureJobs[0].LiveCovers, []string{"R01", "R02"}) {
		t.Fatalf("requirements / feature jobs: %+v %+v", r.Requirements, r.FeatureJobs)
	}
	// The only write is the pass record (a second call answers the same).
	if again := f.check(t); again.OwnerLine != r.OwnerLine || !again.Pass {
		t.Fatalf("second call: %+v", again)
	}
}

// Each condition, broken alone, fails the check with its name (and, for a requirement, its ID and
// reason) in the owner line; the other conditions still hold.
func TestReleaseCheckFailsEachCondition(t *testing.T) {
	for _, tc := range []struct {
		name string
		cond string
		line string // part of the owner line
		set  func(t *testing.T, f *releaseFix)
	}{
		{"checklist edited after the approval", jobs.ReleaseApproval, "mismatch: approval — ", func(t *testing.T, f *releaseFix) {
			p := checklist.ChecklistPath(f.repo)
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(strings.Replace(string(b), "| brief |", "| edited by hand |", 1)), 0o644); err != nil {
				t.Fatal(err)
			}
			f.coder(t, store.RunnerCodex, "gpt-5.6-sol", "R01", "R02")
			f.judge(t, okJudge())
		}},
		{"manifest-check exit 1", jobs.ReleaseManifestCheck, "manifest_check — scripts/manifest-check.sh exited 1", func(t *testing.T, f *releaseFix) {
			f.coder(t, store.RunnerCodex, "gpt-5.6-sol", "R01", "R02")
			j := okJudge()
			j.exit = 1
			f.judge(t, j)
		}},
		{"no manifest-check record", jobs.ReleaseManifestCheck, "manifest_check — no manifest-check record", func(t *testing.T, f *releaseFix) {
			f.coder(t, store.RunnerCodex, "gpt-5.6-sol", "R01", "R02")
			j := okJudge()
			j.noManifest = true
			f.judge(t, j)
		}},
		{"manifest-check of another sha", jobs.ReleaseManifestCheck, "manifest_check — the record of job", func(t *testing.T, f *releaseFix) {
			f.coder(t, store.RunnerCodex, "gpt-5.6-sol", "R01", "R02")
			j := okJudge()
			j.manifestAt = strings.Repeat("b", 40)
			f.judge(t, j)
		}},
		{"no verdict", jobs.ReleaseVerdict, "verdict — no judge verdict", func(t *testing.T, f *releaseFix) {
			f.coder(t, store.RunnerCodex, "gpt-5.6-sol", "R01", "R02")
		}},
		{"two verdicts disagree", jobs.ReleaseVerdict, "verdict — ambiguous", func(t *testing.T, f *releaseFix) {
			f.coder(t, store.RunnerCodex, "gpt-5.6-sol", "R01", "R02")
			j := okJudge()
			j.verdicts = map[string]string{"R01": "missing", "R02": "placeholder"}
			f.judge(t, j)
			f.judge(t, okJudge()) // judging again does not pass over the first verdict
		}},
		{"requirement partial", jobs.ReleaseRequirements, "mismatch: R01 — partial: NOTE_R01", func(t *testing.T, f *releaseFix) {
			f.coder(t, store.RunnerCodex, "gpt-5.6-sol", "R01", "R02")
			j := okJudge()
			j.verdicts = map[string]string{"R01": "partial", "R02": "placeholder"}
			f.judge(t, j)
		}},
		{"placeholder without placeholder-ok", jobs.ReleaseRequirements, "R01 — placeholder", func(t *testing.T, f *releaseFix) {
			f.coder(t, store.RunnerCodex, "gpt-5.6-sol", "R01", "R02")
			j := okJudge()
			j.verdicts = map[string]string{"R01": "placeholder", "R02": "met"}
			f.judge(t, j)
		}},
		{"waiver at another checklist hash", jobs.ReleaseRequirements, "R01 — missing", func(t *testing.T, f *releaseFix) {
			f.coder(t, store.RunnerCodex, "gpt-5.6-sol", "R01", "R02")
			j := okJudge()
			j.verdicts = map[string]string{"R01": "missing", "R02": "met"}
			f.judge(t, j)
			if err := f.m.Store.PutWaiver(store.Waiver{Repo: f.repo, ChecklistID: f.v.ID, ChecklistSHA256: strings.Repeat("e", 64),
				ChecklistVersion: f.v.Version, Requirement: "R01", Quote: "ship it", CreatedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
		}},
		{"unmapped brief", jobs.ReleaseUnmapped, "unmapped_brief — 1 entr(ies)", func(t *testing.T, f *releaseFix) {
			f.coder(t, store.RunnerCodex, "gpt-5.6-sol", "R01", "R02")
			j := okJudge()
			j.unmapped = []string{"a logout button"}
			f.judge(t, j)
		}},
		{"suspect tests", jobs.ReleaseSuspectTests, "suspect_tests — 1 entr(ies)", func(t *testing.T, f *releaseFix) {
			f.coder(t, store.RunnerCodex, "gpt-5.6-sol", "R01", "R02")
			j := okJudge()
			j.suspect = []string{"TestLogin passes without a login"}
			f.judge(t, j)
		}},
		{"a coder of the judge's family, started after the verdict", jobs.ReleaseFamily, "family — the judge (anthropic", func(t *testing.T, f *releaseFix) {
			f.coder(t, store.RunnerCodex, "gpt-5.6-sol", "R01", "R02")
			f.judge(t, okJudge())
			f.coder(t, store.RunnerClaude, "sonnet", "R02") // the verdict's stored snapshot does not know it
		}},
		{"judge not blind by enforcement", jobs.ReleaseBlind, "enforced_blind — the judge (runner codex)", func(t *testing.T, f *releaseFix) {
			f.coder(t, store.RunnerClaude, "sonnet", "R01", "R02")
			j := okJudge()
			j.runner, j.model, j.blind = store.RunnerCodex, "gpt-5.6-sol", false
			f.judge(t, j)
		}},
		{"only a failed coder", jobs.ReleaseFamily, "family — no succeeded feature job (a fix or general job with covers, size not small)", func(t *testing.T, f *releaseFix) {
			f.coderStatus(t, store.StatusFailed, store.RunnerCodex, "gpt-5.6-sol", "R01", "R02")
			f.judge(t, okJudge())
		}},
		{"a red manifest-check judged again until green", jobs.ReleaseManifestCheck, "manifest_check — scripts/manifest-check.sh exited 1 (job gate-",
			func(t *testing.T, f *releaseFix) {
				f.coder(t, store.RunnerCodex, "gpt-5.6-sol", "R01", "R02")
				red := okJudge()
				red.exit = 1
				f.judge(t, red)
				f.judge(t, okJudge())
			}},
		{"a red manifest-check of a cancelled judge without a verdict", jobs.ReleaseManifestCheck,
			"manifest_check — scripts/manifest-check.sh exited 1 (job gate-", func(t *testing.T, f *releaseFix) {
				f.coder(t, store.RunnerCodex, "gpt-5.6-sol", "R01", "R02")
				red := okJudge()
				red.exit = 1
				red.noVerdict = true
				red.status = store.StatusCancelled
				f.judge(t, red)
				f.judge(t, okJudge())
			}},
		{"an earlier verdict without a manifest-check record", jobs.ReleaseManifestCheck, "manifest_check — no manifest-check record of judge job",
			func(t *testing.T, f *releaseFix) {
				f.coder(t, store.RunnerCodex, "gpt-5.6-sol", "R01", "R02")
				j := okJudge()
				j.noManifest = true
				f.judge(t, j)
				f.judge(t, okJudge())
			}},
		{"a verdict of a judge job that failed", jobs.ReleaseVerdict, "verdict — the verdict of job gate-", func(t *testing.T, f *releaseFix) {
			f.coder(t, store.RunnerCodex, "gpt-5.6-sol", "R01", "R02")
			j := okJudge()
			j.status = store.StatusFailed
			f.judge(t, j)
		}},
		{"a verdict of a judge job still running", jobs.ReleaseVerdict, "is running, not succeeded", func(t *testing.T, f *releaseFix) {
			f.coder(t, store.RunnerCodex, "gpt-5.6-sol", "R01", "R02")
			j := okJudge()
			j.status = store.StatusRunning
			f.judge(t, j)
		}},
		{"a verdict stored by a job that is not a judge", jobs.ReleaseVerdict, "is a slicing job, not a judge", func(t *testing.T, f *releaseFix) {
			f.coder(t, store.RunnerCodex, "gpt-5.6-sol", "R01", "R02")
			j := okJudge()
			j.kind = jobs.KindSlicing
			f.judge(t, j)
		}},
		{"a verdict of an earlier attempt of the judge", jobs.ReleaseVerdict, "is of attempt 0, the job ended attempt 1", func(t *testing.T, f *releaseFix) {
			f.coder(t, store.RunnerCodex, "gpt-5.6-sol", "R01", "R02")
			j := okJudge()
			j.reattempt = true
			f.judge(t, j)
		}},
		{"an earlier verdict disagrees on unmapped_brief", jobs.ReleaseVerdict, "disagree on unmapped_brief or suspect_tests", func(t *testing.T, f *releaseFix) {
			f.coder(t, store.RunnerCodex, "gpt-5.6-sol", "R01", "R02")
			j := okJudge()
			j.unmapped = []string{"a logout button"}
			f.judge(t, j)
			f.judge(t, okJudge())
		}},
		{"an earlier verdict disagrees on suspect_tests", jobs.ReleaseVerdict, "disagree on unmapped_brief or suspect_tests", func(t *testing.T, f *releaseFix) {
			f.coder(t, store.RunnerCodex, "gpt-5.6-sol", "R01", "R02")
			j := okJudge()
			j.suspect = []string{"TestLogin passes without a login"}
			f.judge(t, j)
			f.judge(t, okJudge())
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReleaseFix(t)
			tc.set(t, f)
			r := f.check(t)
			c := condition(r, tc.cond)
			if r.Pass || c.OK || !strings.Contains(r.OwnerLine, tc.line) || !strings.HasPrefix(r.OwnerLine, "Acceptance: ") {
				t.Fatalf("%s: pass %v, %s %+v, owner line %q", tc.name, r.Pass, tc.cond, c, r.OwnerLine)
			}
			if tc.cond == jobs.ReleaseApproval || tc.name == "no verdict" {
				return // the rest is not checked without an approved checklist; without a verdict, what reads it fails too
			}
			for _, o := range r.Conditions {
				if o.Name != tc.cond && !o.OK && !(tc.cond == jobs.ReleaseVerdict && o.Name == jobs.ReleaseRequirements) {
					t.Fatalf("%s: condition %s failed too: %s", tc.name, o.Name, o.Detail)
				}
			}
		})
	}
}

// Only the verdicts at the approved checklist hash count: a disagreeing verdict at another hash (same id
// and version) is not read; a coder covering only a deferred row does not fail the trace.
func TestReleaseCheckIgnoresOtherHashAndDeferredCovers(t *testing.T) {
	f := passing(t)
	other := okJudge()
	other.hash = strings.Repeat("e", 64)
	other.verdicts = map[string]string{"R01": "missing", "R02": "missing"}
	other.exit = 1
	f.judge(t, other)
	f.coder(t, store.RunnerCodex, "gpt-5.6-sol", "R03") // R03 was deferred after this job started
	r := f.check(t)
	if !r.Pass || r.Verdicts != 1 || r.OwnerLine != "Acceptance: 2 of 2" || len(r.FeatureJobs) != 2 {
		t.Fatalf("release_check: %+v", r)
	}
}

// An owner waiver bound to this hash and sha (written by checklist_set) accepts a requirement with any
// verdict but met; the waiver is shown with its quote. Without an approval release_check fails closed.
func TestReleaseCheckWaiverAndNoApproval(t *testing.T) {
	f := newReleaseFix(t)
	f.coder(t, store.RunnerCodex, "gpt-5.6-sol", "R01", "R02")
	j := okJudge()
	j.verdicts = map[string]string{"R01": "cannot_check", "R02": "met"}
	f.judge(t, j)
	if r := f.check(t); r.Pass || r.OwnerLine != "Acceptance: 1 of 2, mismatch: R01 — cannot_check: NOTE_R01" {
		t.Fatalf("before the waiver: %+v", r)
	}
	var v ChecklistView
	call(t, f.cs, "checklist_set", map[string]any{"repo": f.repo, "waivers": []map[string]any{
		{"requirement": "R01", "sha": f.head, "quote": "ship without the login check"}}}, &v)
	r := f.check(t)
	if !r.Pass || r.OwnerLine != "Acceptance: 2 of 2" || !r.Requirements[0].Waived || r.Requirements[0].WaiverQuote != "ship without the login check" {
		t.Fatalf("with the waiver: %+v", r)
	}

	repo, head := gitRepo(t)
	var none jobs.ReleaseCheckResult
	call(t, f.cs, "release_check", map[string]any{"repo": repo, "sha": head}, &none)
	if none.Pass || condition(none, jobs.ReleaseApproval).OK || !strings.Contains(none.OwnerLine, "mismatch: approval — ") {
		t.Fatalf("no checklist: %+v", none)
	}
	if msg := callErr(t, f.cs, "release_check", map[string]any{"repo": f.repo, "sha": "abc"}); !strings.Contains(msg, "full commit id") {
		t.Fatalf("short sha: %q", msg)
	}
	if msg := callErr(t, f.cs, "release_check", map[string]any{"repo": filepath.Join(t.TempDir(), "x"), "sha": head}); msg == "" {
		t.Fatal("no repo accepted")
	}
}

// An owner waiver binds to the checklist hash and the requirement, not to a commit (owner decision
// 2026-10-25): one recorded for another sha, or for none, still accepts the requirement at a fix commit.
func TestReleaseCheckWaiverSurvivesFixCommit(t *testing.T) {
	for _, sha := range []string{strings.Repeat("c", 40), ""} {
		f := newReleaseFix(t)
		f.coder(t, store.RunnerCodex, "gpt-5.6-sol", "R01", "R02")
		j := okJudge()
		j.verdicts = map[string]string{"R01": "missing", "R02": "met"}
		f.judge(t, j)
		if err := f.m.Store.PutWaiver(store.Waiver{Repo: f.repo, ChecklistID: f.v.ID, ChecklistSHA256: f.v.SHA256,
			ChecklistVersion: f.v.Version, SHA: sha, Requirement: "R01", Quote: "ship it", CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		if r := f.check(t); !r.Pass || !r.Requirements[0].Waived || r.OwnerLine != "Acceptance: 2 of 2" {
			t.Fatalf("waiver for sha %q: %+v", sha, r)
		}
	}
}

// A feature job is a fix or general job with covers whose size is not small (F9 (a)): one queued before
// sizes (none recorded) counts, so a same-family one fails the family condition; a small one does not;
// alone, small coders leave the check without a feature job.
func TestReleaseCheckFeatureJobsBySize(t *testing.T) {
	f := passing(t)
	small := f.coderSized(t, jobs.SizeSmall, store.StatusSucceeded, store.RunnerClaude, "opus", "R01")
	r := f.check(t)
	if !r.Pass || len(r.FeatureJobs) != 1 || r.FeatureJobs[0].ID == small {
		t.Fatalf("small coder counted: %+v", r)
	}
	unsized := f.coderSized(t, "", store.StatusSucceeded, store.RunnerClaude, "opus", "R02")
	r = f.check(t)
	if fam := condition(r, jobs.ReleaseFamily); r.Pass || fam.OK || !strings.Contains(fam.Detail, unsized) || len(r.FeatureJobs) != 2 {
		t.Fatalf("a same-family coder without a recorded size not counted: %+v", r)
	}

	g := newReleaseFix(t)
	g.coderSized(t, jobs.SizeSmall, store.StatusSucceeded, store.RunnerCodex, "gpt-5.6-sol", "R01", "R02")
	g.judge(t, okJudge())
	r = g.check(t)
	if r.Pass || len(r.FeatureJobs) != 0 || condition(r, jobs.ReleaseFamily).OK ||
		!strings.Contains(r.OwnerLine, "no succeeded feature job (a fix or general job with covers, size not small)") {
		t.Fatalf("only a small coder: %+v", r)
	}
}

// There is no trace condition (owner decision 2026-10-25): a live requirement in no feature job's covers,
// a feature job covering no row of the checklist and a cancelled coder pass; covers stay informational.
func TestReleaseCheckNoTraceCondition(t *testing.T) {
	f := newReleaseFix(t)
	f.coder(t, store.RunnerCodex, "gpt-5.6-sol", "R01")
	f.coder(t, store.RunnerCodex, "gpt-5.6-sol", "R09") // a row since removed from the checklist
	f.coderStatus(t, store.StatusCancelled, store.RunnerCodex, "gpt-5.6-sol", "R02")
	f.judge(t, okJudge())
	r := f.check(t)
	if !r.Pass || r.OwnerLine != "Acceptance: 2 of 2" || condition(r, "trace").Detail != "absent" {
		t.Fatalf("trace still fails the check: %+v", r)
	}
	if len(r.Requirements) != 2 || len(r.Requirements[0].CoveredBy) != 1 || len(r.Requirements[1].CoveredBy) != 0 || len(r.FeatureJobs) != 3 {
		t.Fatalf("covers not shown: %+v %+v", r.Requirements, r.FeatureJobs)
	}
}

// The family condition counts only the succeeded feature jobs (owner decision 2026-10-25): a failed or
// cancelled claude feature job and a claude coder of size small do not fail it under a claude judge; a
// succeeded claude feature job does.
func TestReleaseCheckFamilyCountsSucceededFeatureJobsOnly(t *testing.T) {
	f := passing(t)
	f.coderStatus(t, store.StatusFailed, store.RunnerClaude, "sonnet", "R02")
	f.coderStatus(t, store.StatusCancelled, store.RunnerClaude, "opus", "R01")
	f.coderSized(t, jobs.SizeSmall, store.StatusSucceeded, store.RunnerClaude, "opus", "R01")
	if r := f.check(t); !r.Pass || !condition(r, jobs.ReleaseFamily).OK {
		t.Fatalf("failed, cancelled or small coders fail the family: %+v", r)
	}
	same := f.coder(t, store.RunnerClaude, "sonnet", "R01")
	r := f.check(t)
	if fam := condition(r, jobs.ReleaseFamily); r.Pass || fam.OK || !strings.Contains(fam.Detail, same) {
		t.Fatalf("a succeeded same-family feature job passes the family condition: %+v", r)
	}
}

// Claude coders and a codex judge launched under the read-only codex sandbox (stored enforced_blind true)
// pass: the family differs and the judge counts as blind.
func TestReleaseCheckClaudeCodersCodexJudgePass(t *testing.T) {
	f := newReleaseFix(t)
	f.coder(t, store.RunnerClaude, "opus", "R01", "R02")
	f.coderStatus(t, store.StatusFailed, store.RunnerCodex, "gpt-5.6-sol", "R01")
	j := okJudge()
	j.runner, j.model, j.blind = store.RunnerCodex, "gpt-5.6-sol", true
	f.judge(t, j)
	if r := f.check(t); !r.Pass || r.OwnerLine != "Acceptance: 2 of 2" {
		t.Fatalf("claude coders, codex judge: %+v", r)
	}
}

// A pass is recorded for the merge hook (repo, sha, checklist id, version, hash); a later run for the
// same sha that does not pass removes it.
func TestReleaseCheckRecordsPass(t *testing.T) {
	f := passing(t)
	if p, err := f.m.Store.ReleasePassed(f.repo, f.head, f.v.ID, f.v.Version, f.v.SHA256); err != nil || p != nil {
		t.Fatalf("a pass before the check: %+v %v", p, err)
	}
	r := f.check(t)
	p, err := f.m.Store.ReleasePassed(f.repo, f.head, f.v.ID, f.v.Version, f.v.SHA256)
	if err != nil || p == nil || !r.Pass || p.VerdictJob != r.VerdictJob {
		t.Fatalf("pass not recorded: %+v %v (%+v)", p, err, r)
	}
	bad := okJudge()
	bad.verdicts = map[string]string{"R01": "missing", "R02": "placeholder"}
	f.judge(t, bad)
	if r := f.check(t); r.Pass {
		t.Fatalf("passes with a disagreeing verdict: %+v", r)
	}
	if p, err := f.m.Store.ReleasePassed(f.repo, f.head, f.v.ID, f.v.Version, f.v.SHA256); err != nil || p != nil {
		t.Fatalf("pass kept after a failing run: %+v %v", p, err)
	}
}

// A waiver stored for another checklist id with the same hash and version (identical content) does not
// accept a requirement of this checklist; the newest waiver's quote is the one shown.
func TestReleaseCheckWaiverBoundToChecklistID(t *testing.T) {
	f := newReleaseFix(t)
	f.coder(t, store.RunnerCodex, "gpt-5.6-sol", "R01", "R02")
	j := okJudge()
	j.verdicts = map[string]string{"R01": "missing", "R02": "met"}
	f.judge(t, j)
	at := time.Now()
	if err := f.m.Store.PutWaiver(store.Waiver{Repo: f.repo, ChecklistID: f.v.ID + "-other", ChecklistSHA256: f.v.SHA256,
		ChecklistVersion: f.v.Version, Requirement: "R01", Quote: "other checklist", CreatedAt: at}); err != nil {
		t.Fatal(err)
	}
	if r := f.check(t); r.Pass || r.Requirements[0].Waived {
		t.Fatalf("waiver of another checklist id accepted R01: %+v", r)
	}
	for i, sha := range []string{strings.Repeat("f", 40), strings.Repeat("0", 40)} {
		if err := f.m.Store.PutWaiver(store.Waiver{Repo: f.repo, ChecklistID: f.v.ID, ChecklistSHA256: f.v.SHA256,
			ChecklistVersion: f.v.Version, SHA: sha, Requirement: "R01", Quote: "quote " + sha[:1],
			CreatedAt: at.Add(time.Duration(i+1) * time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	if r := f.check(t); !r.Pass || !r.Requirements[0].Waived || r.Requirements[0].WaiverQuote != "quote 0" {
		t.Fatalf("own waiver: %+v", r.Requirements)
	}
}
