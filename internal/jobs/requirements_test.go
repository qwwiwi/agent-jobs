package jobs

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

const reqReport = "## Summary\nok\n\n## Findings / Changes\n- changed a.go\n\n### Requirements\n" +
	"- R01 · done · login page in web/login.go:12 · ran: go test ./web → ok\n" +
	"- R02 · done · sessions kept · REDPROOF: TestSession failed before\n" +
	"- R03i · partial · only the form · ran: go test ./web → ok\n" +
	"- R09 · done · not covered · ran: go test ./x → ok · REDPROOF R09: TestX failed before\n" +
	"- R04 · finished · bad status\n" +
	"REDPROOF R01 · TestLogin · FAIL before the change, ok after\n\n" +
	"### Other\nR05 · done · outside the block · ran: x\n\n" +
	"## Evidence\n-\n\n## Not done / uncertain\nnothing\n"

func TestParseRequirements(t *testing.T) {
	r := ParseRequirements(reqReport, []string{"R01", "R02", "R03i", "R05"})
	if !r.Block || len(r.Lines) != 4 {
		t.Fatalf("block %+v", r)
	}
	if l := r.Lines[0]; l.ID != "R01" || l.Status != "done" || l.Ran != "go test ./web → ok" || !l.Verified ||
		l.Evidence != "login page in web/login.go:12" {
		t.Fatalf("R01 %+v", l)
	}
	// done without ran: (R02) is claimed; R09 is verified but not covered; R05 has no line inside the block.
	if !slices.Equal(r.Claimed, []string{"R02"}) || !slices.Equal(r.Uncovered, []string{"R09"}) ||
		!slices.Equal(r.MissingLines, []string{"R05"}) || len(r.Invalid) != 1 || !strings.Contains(r.Invalid[0], "R04") {
		t.Fatalf("claimed %v uncovered %v missing %v invalid %v", r.Claimed, r.Uncovered, r.MissingLines, r.Invalid)
	}
	if len(r.RedProofs) != 1 || !strings.HasPrefix(r.RedProofs[0], "REDPROOF R01") {
		t.Fatalf("redproofs %v", r.RedProofs)
	}
	// done with ran: but no REDPROOF naming it is claimed too.
	noProof := strings.Replace(reqReport, "REDPROOF R01 · TestLogin", "REDPROOF R77 · TestLogin", 1)
	if r := ParseRequirements(noProof, []string{"R01"}); !slices.Contains(r.Claimed, "R01") {
		t.Fatalf("done without REDPROOF not claimed: %v", r.Claimed)
	}
	for _, report := range []string{
		"## Summary\nok\n\n## Findings / Changes\n- x\n\n## Not done / uncertain\nnothing\n",
		"## Summary\nok\n\n### Requirements\nR01 · done\n\n## Not done / uncertain\nnothing\n",
		"## Summary\nok\n\n## Findings / Changes\n```\n### Requirements\n```\n\n## Not done / uncertain\nnothing\n",
	} {
		if ParseRequirements(report, []string{"R01"}).Block {
			t.Errorf("block found in %q", report)
		}
	}
}

func TestReportContractNoteCovers(t *testing.T) {
	plain := "## Summary\nok\n\n## Findings / Changes\n- x\n\n## Not done / uncertain\nnothing\n"
	if n := reportContractNote(plain, nil); n != "" {
		t.Fatalf("no covers: %q", n)
	}
	if n := reportContractNote(plain, []string{"R01"}); n != NoteNoRequirements || OffContractNote("x; "+n) != NoteNoRequirements {
		t.Fatalf("covers without block: %q", n)
	}
	if n := reportContractNote(reqReport, []string{"R01"}); n != "" {
		t.Fatalf("covers with block: %q", n)
	}
}

func TestEnqueueCovers(t *testing.T) {
	m := newManager(t)
	repo := briefRepo(t)
	approveBrief(t, m, repo)
	j := enqueue(t, m, "do it", func(r *StartRequest) { r.RepoPath = repo; r.Covers = []string{" R02", "R01", "R02"} })
	got, err := m.Store.Get(j.ID)
	if err != nil || !slices.Equal(got.Covers, []string{"R02", "R01"}) {
		t.Fatalf("covers %v %v", got.Covers, err)
	}
	if _, err := m.Enqueue(StartRequest{Title: "t", Kind: "general", Brief: "b", Model: "sonnet", Covers: []string{"A01"}}); err == nil ||
		!strings.Contains(err.Error(), "covers") {
		t.Fatalf("bad id accepted: %v", err)
	}
	plain := enqueue(t, m, "do it", nil)
	if got, _ := m.Store.Get(plain.ID); len(got.Covers) != 0 {
		t.Fatalf("job without covers: %v", got.Covers)
	}
}

// A job with covers whose report has no block is off contract (job row, done line, stats); one with
// a block counts its claimed done lines in the stats; a job without covers is unchanged.
func TestCoversReportContract(t *testing.T) {
	m := newManager(t)
	repo := briefRepo(t)
	approveBrief(t, m, repo)
	j := runToEnd(t, m, enqueue(t, m, "do the thing", func(r *StartRequest) { r.RepoPath = repo; r.Covers = []string{"R01"} }).ID)
	if j.Status != store.StatusSucceeded || j.Error != NoteNoRequirements {
		t.Fatalf("no block: %s %q", j.Status, j.Error)
	}
	ok := runToEnd(t, m, enqueue(t, m, "FAKE_REQUIREMENTS_REPORT", func(r *StartRequest) { r.RepoPath = repo; r.Covers = []string{"R01", "R02"} }).ID)
	if ok.Status != store.StatusSucceeded || ok.Error != "" {
		t.Fatalf("block: %s %q", ok.Status, ok.Error)
	}
	plain := runToEnd(t, m, enqueue(t, m, "do the thing", nil).ID)
	if plain.Error != "" {
		t.Fatalf("no covers: %q", plain.Error)
	}
	rep, err := m.Stats(time.Time{}, "kind")
	if err != nil || rep.Totals.ReportOffContract != 1 || rep.Totals.RequirementsClaimed != 1 || rep.Totals.ClaimedJobs != 1 {
		t.Fatalf("stats: %+v %v", rep.Totals, err)
	}
	st, err := m.Store.GetStats(ok.ID)
	if err != nil || st.RequirementsClaimed != 1 {
		t.Fatalf("stats row: %+v %v", st, err)
	}
}
