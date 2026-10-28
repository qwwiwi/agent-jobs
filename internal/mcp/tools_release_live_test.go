package mcp

import (
	"slices"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// Suspect tests warn, they do not block (#106, owner decision 2026-10-27): a check whose only finding is
// suspect_tests passes, lists them in needs_live_check and says «live check needed» in the owner line,
// and stores the pass for the merge hook; with any blocking condition broken as well it still fails.
func TestReleaseCheckSuspectTestsWarnOnly(t *testing.T) {
	const s1, s2 = "TestE2E passes with a mock model", "TestServer passes with a scripted fake model"
	for _, tc := range []struct {
		name string
		set  func(t *testing.T, f *releaseFix)
		pass bool
		line string   // the owner line, or part of it when the check fails
		live []string // needs_live_check
	}{
		{"suspect only", func(t *testing.T, f *releaseFix) {
			j := okJudge()
			j.suspect = []string{s1, s2}
			f.judge(t, j)
		}, true, "Acceptance: 2 of 2; live check needed: 2 test(s)", []string{s1, s2}},
		{"an earlier verdict lists suspect tests, the latest none", func(t *testing.T, f *releaseFix) {
			j := okJudge()
			j.suspect = []string{s1}
			f.judge(t, j)
			f.judge(t, okJudge())
		}, true, "Acceptance: 2 of 2; live check needed: 1 test(s)", []string{s1}},
		{"suspect and a partial requirement", func(t *testing.T, f *releaseFix) {
			j := okJudge()
			j.verdicts = map[string]string{"R01": "partial", "R02": "placeholder"}
			j.suspect = []string{s1}
			f.judge(t, j)
		}, false, "mismatch: R01 — partial: NOTE_R01; live check needed: 1 test(s)", []string{s1}},
		{"suspect and a missing requirement", func(t *testing.T, f *releaseFix) {
			j := okJudge()
			j.verdicts = map[string]string{"R01": "met", "R02": "missing"}
			j.suspect = []string{s1}
			f.judge(t, j)
		}, false, "R02 — missing", []string{s1}},
		{"suspect and an unmapped brief quote", func(t *testing.T, f *releaseFix) {
			j := okJudge()
			j.unmapped = []string{"a logout button"}
			j.suspect = []string{s1}
			f.judge(t, j)
		}, false, "unmapped_brief — 1 entr(ies)", []string{s1}},
		{"suspect and a red manifest-check", func(t *testing.T, f *releaseFix) {
			j := okJudge()
			j.exit = 1
			j.suspect = []string{s1}
			f.judge(t, j)
		}, false, "manifest_check — scripts/manifest-check.sh exited 1", []string{s1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReleaseFix(t)
			f.coder(t, store.RunnerCodex, "gpt-5.6-sol", "R01", "R02")
			tc.set(t, f)
			r := f.check(t)
			if r.Pass != tc.pass || !slices.Equal(r.NeedsLiveCheck, tc.live) {
				t.Fatalf("pass %v, needs_live_check %q, owner line %q, mismatches %q", r.Pass, r.NeedsLiveCheck, r.OwnerLine, r.Mismatches)
			}
			if tc.pass && (r.OwnerLine != tc.line || len(r.Mismatches) != 0) || !tc.pass && !strings.Contains(r.OwnerLine, tc.line) {
				t.Fatalf("owner line %q, want %q; mismatches %q", r.OwnerLine, tc.line, r.Mismatches)
			}
			for _, m := range r.Mismatches {
				if strings.Contains(m, "suspect") {
					t.Fatalf("suspect tests reported as a mismatch: %q", r.Mismatches)
				}
			}
			p, err := f.m.Store.ReleasePassed(r.Repo, r.SHA, r.ChecklistID, r.Version, r.ChecklistSHA256)
			if err != nil || (p != nil) != tc.pass {
				t.Fatalf("pass record %+v %v, want stored %v", p, err, tc.pass)
			}
		})
	}
}

// Without suspect tests needs_live_check is an empty list and the owner line has no live-check part.
func TestReleaseCheckNoSuspectTests(t *testing.T) {
	r := passing(t).check(t)
	if !r.Pass || r.NeedsLiveCheck == nil || len(r.NeedsLiveCheck) != 0 || r.OwnerLine != "Acceptance: 2 of 2" {
		t.Fatalf("%+v", r)
	}
	if slices.Contains(jobs.ReleaseConditions, "suspect_tests") {
		t.Fatalf("suspect_tests is still a condition: %q", jobs.ReleaseConditions)
	}
}
