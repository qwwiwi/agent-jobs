package jobs

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/qwwiwi/agent-jobs/internal/checklist"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// release_check (Wave F, F7): read-only, it answers whether the code at a commit may ship under the
// repository's approved checklist. PASS only when every condition holds; a missing, unreadable or
// ambiguous record fails its condition (fail closed). It writes nothing.

// Release-check condition names, in the order they are reported.
const (
	ReleaseApproval      = "approval"
	ReleaseManifestCheck = "manifest_check"
	ReleaseVerdict       = "verdict"
	ReleaseRequirements  = "requirements"
	ReleaseUnmapped      = "unmapped_brief"
	ReleaseSuspectTests  = "suspect_tests"
	ReleaseFamily        = "family"
	ReleaseBlind         = "enforced_blind"
	ReleaseTrace         = "trace"
)

// ReleaseConditions lists the conditions in report order.
var ReleaseConditions = []string{ReleaseApproval, ReleaseManifestCheck, ReleaseVerdict, ReleaseRequirements, ReleaseUnmapped,
	ReleaseSuspectTests, ReleaseFamily, ReleaseBlind, ReleaseTrace}

// ReleaseCondition is one condition of release_check and whether it holds.
type ReleaseCondition struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// ReleaseRequirement is one live requirement as release_check saw it.
type ReleaseRequirement struct {
	ID string `json:"id"`
	// Verdict is the judge's ("" = none).
	Verdict       string `json:"verdict"`
	PlaceholderOK bool   `json:"placeholder_ok,omitempty"`
	// Waived: an owner waiver bound to this checklist hash and version and this sha covers it.
	Waived      bool   `json:"waived,omitempty"`
	WaiverQuote string `json:"waiver_quote,omitempty"`
	// Accepted: met, placeholder on a placeholder-ok row, or waived.
	Accepted bool `json:"accepted"`
	// CoveredBy are the feature jobs whose covers name it.
	CoveredBy []string `json:"covered_by"`
}

// ReleaseFeatureJob is a feature job of the checklist: a fix or general job of the repository whose
// covers name the checklist id (F9 (a)'s size is not read yet; DEVIATIONS #34).
type ReleaseFeatureJob struct {
	ID     string   `json:"id"`
	Kind   string   `json:"kind"`
	Family string   `json:"family"`
	Covers []string `json:"covers"`
	// LiveCovers are its covers that are live requirements of the approved checklist.
	LiveCovers []string `json:"live_covers"`
}

// ReleaseCheckResult is release_check's answer.
type ReleaseCheckResult struct {
	Pass            bool   `json:"pass"`
	Repo            string `json:"repo"`
	SHA             string `json:"sha"`
	ChecklistID     string `json:"checklist_id"`
	Version         int    `json:"version"`
	ChecklistSHA256 string `json:"checklist_sha256"`
	// Accepted of Live requirements are accepted (met, placeholder where placeholder-ok, or waived).
	Accepted     int                  `json:"accepted"`
	Live         int                  `json:"live"`
	Conditions   []ReleaseCondition   `json:"conditions"`
	Requirements []ReleaseRequirement `json:"requirements"`
	FeatureJobs  []ReleaseFeatureJob  `json:"feature_jobs"`
	// VerdictJob is the judge job whose verdict was read (the latest for the sha); Verdicts counts the
	// verdicts for the sha at this checklist version (all must agree).
	VerdictJob    string               `json:"verdict_job,omitempty"`
	Verdicts      int                  `json:"verdicts"`
	JudgeFamily   string               `json:"judge_family,omitempty"`
	ManifestCheck *store.ManifestCheck `json:"manifest_check"`
	Mismatches    []string             `json:"mismatches"`
	// OwnerLine is the one line for the owner: «Acceptance: N of M» or «Acceptance: N of M, mismatch: …».
	OwnerLine string `json:"owner_line"`
}

// maxReleaseReason bounds one mismatch reason.
const maxReleaseReason = 160

// ReleaseCheck is release_check(repo, sha) (and `agent-jobs gate release-check`). An error is returned
// only for a bad argument or an unreadable store or work tree; every missing record is a failed
// condition in the result.
func (m *Manager) ReleaseCheck(repo, sha string) (*ReleaseCheckResult, error) {
	top, main, err := ChecklistRepo(repo)
	if err != nil {
		return nil, err
	}
	sha = strings.TrimSpace(sha)
	if err := CheckCommit(main, sha); err != nil {
		return nil, fmt.Errorf("release_check: %w", err)
	}
	out := &ReleaseCheckResult{Repo: main, SHA: sha, Requirements: []ReleaseRequirement{}, FeatureJobs: []ReleaseFeatureJob{},
		Mismatches: []string{}}
	conds := map[string]ReleaseCondition{}
	set := func(name string, ok bool, format string, a ...any) {
		conds[name] = ReleaseCondition{Name: name, OK: ok, Detail: RedactPrintable(fmt.Sprintf(format, a...), 600)}
	}
	defer func() {
		out.Conditions = make([]ReleaseCondition, 0, len(ReleaseConditions))
		out.Pass = true
		for _, n := range ReleaseConditions {
			c, ok := conds[n]
			if !ok {
				c = ReleaseCondition{Name: n, Detail: "not checked: the checklist is not approved"}
			}
			out.Conditions = append(out.Conditions, c)
			out.Pass = out.Pass && c.OK
			if !c.OK && n != ReleaseRequirements {
				out.Mismatches = append(out.Mismatches, n+" — "+RedactPrintable(c.Detail, maxReleaseReason))
			}
		}
		out.OwnerLine = fmt.Sprintf("Acceptance: %d of %d", out.Accepted, out.Live)
		if !out.Pass {
			out.OwnerLine += ", mismatch: " + strings.Join(out.Mismatches, "; ")
		}
	}()

	c, brief, why, err := m.releaseChecklist(top)
	if err != nil {
		return nil, err
	}
	if why != "" {
		set(ReleaseApproval, false, "%s", why)
		return out, nil
	}
	out.ChecklistID, out.Version, out.Live = c.ID, c.Version, len(c.LiveIDs())
	approved, hash, reason, last, err := m.ChecklistStatus(main, c, brief)
	if err != nil {
		return nil, err
	}
	out.ChecklistSHA256 = hash
	switch {
	case last == nil:
		set(ReleaseApproval, false, "the repository has no stored owner approval")
		return out, nil
	case !approved:
		set(ReleaseApproval, false, "checklist %s version %d is not approved: %s", c.ID, c.Version, reason)
		return out, nil
	case last.ChecklistID != c.ID || last.Version != c.Version || last.ChecklistSHA256 != hash:
		set(ReleaseApproval, false, "the latest approval is of %s version %d, not of the checklist as it is", last.ChecklistID, last.Version)
		return out, nil
	}
	set(ReleaseApproval, true, "checklist %s version %d approved, hash unchanged", c.ID, c.Version)
	live := c.LiveIDs()
	if len(live) == 0 {
		set(ReleaseRequirements, false, "the checklist has no live requirements")
	}

	// The verdicts for this sha at the approved checklist id and version (and hash): the latest is read,
	// and every other one must agree with it, so no verdict is passed over by judging again.
	all, err := m.Store.VerdictsAt(main, c.ID, c.Version, sha)
	if err != nil {
		return nil, err
	}
	var verdicts []*store.JudgeVerdict
	for _, v := range all {
		if v.ChecklistSHA256 == hash {
			verdicts = append(verdicts, v)
		}
	}
	out.Verdicts = len(verdicts)
	var latest *store.JudgeVerdict
	var lo *JudgeOutput
	if len(verdicts) == 0 {
		set(ReleaseVerdict, false, "no judge verdict for %s at checklist %s version %d", shortSHA(sha), c.ID, c.Version)
	} else {
		latest = verdicts[len(verdicts)-1]
		out.VerdictJob = latest.JobID
		if lo, err = verdictOutput(latest); err != nil {
			set(ReleaseVerdict, false, "verdict of job %s unreadable: %v", latest.JobID, err)
			lo = nil
		} else if why := verdictsDisagree(verdicts, lo); why != "" {
			set(ReleaseVerdict, false, "ambiguous: %s", why)
		} else {
			set(ReleaseVerdict, true, "job %s (%d verdict(s) for this sha, all agree)", latest.JobID, len(verdicts))
		}
	}

	// The manifest-check record of the judge's attempt (F5), else the latest one for the sha.
	var mc *store.ManifestCheck
	if latest != nil {
		mc, err = m.Store.ManifestCheckByJob(latest.JobID, latest.Attempt)
	} else {
		mc, err = m.Store.LatestManifestCheck(main, sha)
	}
	if err != nil {
		return nil, err
	}
	out.ManifestCheck = mc
	switch {
	case mc == nil && latest != nil:
		set(ReleaseManifestCheck, false, "no manifest-check record of judge job %s", latest.JobID)
	case mc == nil:
		set(ReleaseManifestCheck, false, "no manifest-check record for %s", shortSHA(sha))
	case mc.SHA != sha:
		set(ReleaseManifestCheck, false, "the record of job %s is for %s, not %s", mc.JobID, shortSHA(mc.SHA), shortSHA(sha))
	case mc.ExitCode != 0:
		set(ReleaseManifestCheck, false, "%s exited %d (job %s)", mc.Script, mc.ExitCode, mc.JobID)
	default:
		set(ReleaseManifestCheck, true, "%s exit 0 (job %s)", mc.Script, mc.JobID)
	}

	waivers, err := m.Store.Waivers(main, hash, c.Version)
	if err != nil {
		return nil, err
	}
	coders, err := m.Store.CoverJobs(main, c.ID)
	if err != nil {
		return nil, err
	}
	var reqMismatch []string
	for _, id := range live {
		row, _ := c.Row(id)
		r := ReleaseRequirement{ID: id, PlaceholderOK: row.PlaceholderOK, CoveredBy: []string{}}
		for _, w := range waivers {
			if w.SHA == sha && w.Requirement == id {
				r.Waived, r.WaiverQuote = true, RedactPrintable(w.Quote, 300)
			}
		}
		for _, cj := range coders {
			if slices.Contains(cj.Covers, id) {
				r.CoveredBy = append(r.CoveredBy, cj.ID)
			}
		}
		why := ""
		if lo != nil {
			var found []JudgeRequirement
			for _, jr := range lo.Requirements {
				if jr.ID == id {
					found = append(found, jr)
				}
			}
			if len(found) == 1 {
				r.Verdict = found[0].Verdict
				why = r.Verdict
				if n := strings.TrimSpace(found[0].Note); n != "" {
					why += ": " + n
				}
			} else {
				why = fmt.Sprintf("the verdict lists it %d times", len(found))
			}
		} else {
			why = "no readable verdict"
		}
		r.Accepted = r.Verdict == "met" || r.Verdict == "placeholder" && r.PlaceholderOK || r.Waived && r.Verdict != ""
		if r.Accepted {
			out.Accepted++
		} else {
			reqMismatch = append(reqMismatch, id+" — "+RedactPrintable(why, maxReleaseReason))
		}
		out.Requirements = append(out.Requirements, r)
	}
	out.Mismatches = append(out.Mismatches, reqMismatch...)
	if _, done := conds[ReleaseRequirements]; !done {
		if len(reqMismatch) == 0 {
			set(ReleaseRequirements, true, "%d of %d accepted", out.Accepted, len(live))
		} else {
			set(ReleaseRequirements, false, "%d of %d accepted", out.Accepted, len(live))
		}
	}

	if lo == nil {
		set(ReleaseUnmapped, false, "no readable verdict")
		set(ReleaseSuspectTests, false, "no readable verdict")
	} else {
		listCond := func(name string, l []string) {
			if len(l) == 0 {
				set(name, true, "empty")
			} else {
				set(name, false, "%d entr(ies): %s", len(l), quoteList(l[:min(len(l), 3)]))
			}
		}
		listCond(ReleaseUnmapped, lo.UnmappedBrief)
		listCond(ReleaseSuspectTests, lo.SuspectTests)
	}

	// The coders' families, recomputed now (the verdict's are a snapshot taken when it was stored).
	if latest == nil {
		set(ReleaseFamily, false, "no verdict")
		set(ReleaseBlind, false, "no verdict")
	} else {
		out.JudgeFamily = ModelFamily(latest.JudgeRunner, latest.JudgeModel)
		var same []string
		for _, cj := range coders {
			if ModelFamily(cj.Runner, cj.Model) == out.JudgeFamily {
				same = append(same, cj.ID)
			}
		}
		switch {
		case len(coders) == 0:
			set(ReleaseFamily, false, "no feature job covers checklist %s, so no coder family to differ from", c.ID)
		case len(same) > 0:
			set(ReleaseFamily, false, "the judge (%s, model %s) shares its family with coder job(s) %s", out.JudgeFamily,
				latest.JudgeModel, strings.Join(same, ", "))
		default:
			set(ReleaseFamily, true, "judge %s, coders differ", out.JudgeFamily)
		}
		if latest.EnforcedBlind {
			set(ReleaseBlind, true, "the judge was held to its input by the guard")
		} else {
			set(ReleaseBlind, false, "the judge (runner %s) was not blind by enforcement", latest.JudgeRunner)
		}
	}

	// The trace both ways: every live requirement in some feature job's covers, every feature job
	// covering at least one live requirement.
	var uncovered, idle []string
	for _, cj := range coders {
		fj := ReleaseFeatureJob{ID: cj.ID, Kind: cj.Kind, Family: ModelFamily(cj.Runner, cj.Model), Covers: nonNilSlice(cj.Covers),
			LiveCovers: []string{}}
		for _, id := range cj.Covers {
			if slices.Contains(live, id) {
				fj.LiveCovers = append(fj.LiveCovers, id)
			}
		}
		if len(fj.LiveCovers) == 0 {
			idle = append(idle, cj.ID)
		}
		out.FeatureJobs = append(out.FeatureJobs, fj)
	}
	for _, r := range out.Requirements {
		if len(r.CoveredBy) == 0 {
			uncovered = append(uncovered, r.ID)
		}
	}
	var trace []string
	if len(coders) == 0 {
		trace = append(trace, "no feature job covers checklist "+c.ID)
	}
	if len(uncovered) > 0 {
		trace = append(trace, "in no feature job's covers: "+strings.Join(uncovered, ", "))
	}
	if len(idle) > 0 {
		trace = append(trace, "feature job(s) covering no live requirement: "+strings.Join(idle, ", "))
	}
	if len(trace) == 0 {
		set(ReleaseTrace, true, "%d feature job(s), every live requirement covered", len(coders))
	} else {
		set(ReleaseTrace, false, "%s", strings.Join(trace, "; "))
	}
	return out, nil
}

// releaseChecklist reads and parses the work tree's checklist (redacted) and BRIEF.md; why says what is
// missing ("" = both read).
func (m *Manager) releaseChecklist(top string) (c *checklist.Checklist, brief []byte, why string, err error) {
	f, err := m.readWorkTreeBrief(top)
	if err != nil {
		return nil, nil, "", err
	}
	if f.Checklist == nil {
		return nil, nil, "the work tree has no " + checklist.ChecklistRel, nil
	}
	if f.Brief == nil {
		return nil, nil, "the work tree has no " + checklist.BriefRel, nil
	}
	text, _ := Redact(string(f.Checklist))
	if c, err = checklist.Parse(text); err != nil {
		return nil, nil, fmt.Sprintf("%s does not parse: %v", checklist.ChecklistRel, err), nil
	}
	return c, f.Brief, "", nil
}

// verdictOutput decodes a stored verdict's block.
func verdictOutput(v *store.JudgeVerdict) (*JudgeOutput, error) {
	b, err := json.Marshal(v.Verdict)
	if err != nil {
		return nil, err
	}
	var o JudgeOutput
	if err := json.Unmarshal(b, &o); err != nil {
		return nil, err
	}
	return &o, nil
}

// verdictsDisagree is why the verdicts for one sha and checklist version do not agree with the latest
// one's block lo ("" = they agree): a requirement judged differently, or unmapped_brief / suspect_tests
// empty in one and not in the other.
func verdictsDisagree(vs []*store.JudgeVerdict, lo *JudgeOutput) string {
	want := map[string]string{}
	for _, r := range lo.Requirements {
		want[r.ID] = r.Verdict
	}
	latest := vs[len(vs)-1]
	for _, v := range vs[:len(vs)-1] {
		o, err := verdictOutput(v)
		if err != nil {
			return fmt.Sprintf("verdict of job %s unreadable: %v", v.JobID, err)
		}
		got := map[string]string{}
		for _, r := range o.Requirements {
			got[r.ID] = r.Verdict
		}
		ids := make([]string, 0, len(want)+len(got))
		for id := range want {
			ids = append(ids, id)
		}
		for id := range got {
			if _, ok := want[id]; !ok {
				ids = append(ids, id)
			}
		}
		slices.Sort(ids)
		for _, id := range ids {
			if got[id] != want[id] {
				return fmt.Sprintf("jobs %s and %s disagree on %s (%q, %q)", v.JobID, latest.JobID, id, got[id], want[id])
			}
		}
		if (len(o.UnmappedBrief) == 0) != (len(lo.UnmappedBrief) == 0) || (len(o.SuspectTests) == 0) != (len(lo.SuspectTests) == 0) {
			return fmt.Sprintf("jobs %s and %s disagree on unmapped_brief or suspect_tests", v.JobID, latest.JobID)
		}
	}
	return ""
}
