package jobs

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/checklist"
)

// sizeChecklist is coverChecklist plus a deferred row and a constraint row, neither a live requirement.
const sizeChecklist = coverChecklist + `| R04 | «export to pdf» | deferred | owner: later |
| D01 | builds on go 1.22 | live | proven by the build |
`

// sizeRepo is briefRepo with sizeChecklist, approved in m's store.
func sizeRepo(t *testing.T, m *Manager) string {
	t.Helper()
	repo := briefRepo(t)
	if err := os.WriteFile(checklist.ChecklistPath(repo), []byte(sizeChecklist), 0o644); err != nil {
		t.Fatal(err)
	}
	approveBrief(t, m, repo)
	return repo
}

// size is stored on the job row and logged for fix and general jobs; a value other than small or large is
// refused for every kind; read-only kinds may carry one, never checked.
func TestSizeStoredAndLogged(t *testing.T) {
	m := newManager(t)
	var logs bytes.Buffer
	m.Log = log.New(&logs, "", 0)
	small := enqueue(t, m, "b", func(r *StartRequest) { r.Kind = "fix"; r.Size = " small "; r.SizeRule = true })
	if got := get(t, m, small.ID).Size; got != SizeSmall {
		t.Fatalf("stored size %q, want small", got)
	}
	if !strings.Contains(logs.String(), "job "+small.ID+" (fix): size small") {
		t.Errorf("small not logged: %q", logs.String())
	}
	none := enqueue(t, m, "b", func(r *StartRequest) { r.SizeRule = true })
	if get(t, m, none.ID).Size != SizeSmall || !strings.Contains(logs.String(), "job "+none.ID+" (general): size small") {
		t.Errorf("missing size not inferred as small: %q", logs.String())
	}
	review := enqueue(t, m, "b", func(r *StartRequest) { r.Kind = "review"; r.Size = SizeLarge; r.SizeRule = true })
	if get(t, m, review.ID).Size != SizeLarge || strings.Contains(logs.String(), review.ID) {
		t.Errorf("review: size not stored as given, or logged: %q", logs.String())
	}
	for _, kind := range []string{"fix", "review"} {
		if _, err := m.Enqueue(StartRequest{Title: "t", Kind: kind, Brief: "b", Model: "sonnet", Size: "medium"}); err == nil ||
			!strings.Contains(err.Error(), `size "medium" must be small or large`) {
			t.Errorf("%s with size medium: %v", kind, err)
		}
	}
}

// With size_required on, job_start (SizeRule) refuses a fix or general job without size; read-only
// kinds and callers without the rule (schedules, gate jobs) are not held to it. Off (the default), the
// size is inferred.
func TestSizeRequired(t *testing.T) {
	m := newManager(t)
	m.Cfg.SizeRequired = true
	for _, kind := range SizeKinds {
		if _, err := m.Enqueue(StartRequest{Title: "t", Kind: kind, Brief: "b", Model: "sonnet", SizeRule: true}); err == nil ||
			!strings.Contains(err.Error(), "size is required for a "+kind+" job") || !strings.Contains(err.Error(), "size_required") {
			t.Errorf("%s without size: %v", kind, err)
		}
	}
	enqueue(t, m, "b", func(r *StartRequest) { r.Kind = "review"; r.SizeRule = true })
	enqueue(t, m, "b", func(r *StartRequest) { r.Kind = "research"; r.SizeRule = true })
	var logs bytes.Buffer
	m.Log = log.New(&logs, "", 0)
	fired := enqueue(t, m, "b", func(r *StartRequest) { r.Kind = "fix" }) // a schedule's firing: no SizeRule
	// The exempt job gets the inferred size (small: no covers).
	if s := logs.String(); !strings.Contains(s, "job "+fired.ID+" (fix): size small") || get(t, m, fired.ID).Size != SizeSmall {
		t.Errorf("exempt job's log: %q", s)
	}
	enqueue(t, m, "b", func(r *StartRequest) { r.Kind = "fix"; r.Size = SizeSmall; r.SizeRule = true })
	m.Cfg.SizeRequired = false
	logs.Reset()
	off := enqueue(t, m, "b", func(r *StartRequest) { r.Kind = "fix"; r.SizeRule = true })
	if s := logs.String(); !strings.Contains(s, "job "+off.ID+" (fix): size small") {
		t.Errorf("size_required off, log: %q", s)
	}
}

// size large on a fix or general job needs covers naming only live requirements of the repository's
// approved, unchanged checklist; this holds with size_required off. Read-only kinds are exempt.
func TestSizeLarge(t *testing.T) {
	m := newManager(t)
	repo := sizeRepo(t, m)
	large := func(kind string, covers ...string) error {
		_, err := m.Enqueue(StartRequest{Title: "t", Kind: kind, Brief: "b", Model: "sonnet", RepoPath: repo,
			Size: SizeLarge, Covers: covers, SizeRule: true})
		return err
	}
	for _, kind := range SizeKinds {
		if err := large(kind); err == nil || !strings.Contains(err.Error(), "size large needs covers") {
			t.Errorf("%s large without covers: %v", kind, err)
		}
		if err := large(kind, "R01", "R04", "D01"); err == nil || !strings.Contains(err.Error(), "only live requirements") ||
			!strings.Contains(err.Error(), "R04 (deferred)") || !strings.Contains(err.Error(), "D01 (live)") ||
			strings.Contains(err.Error(), "R01") {
			t.Errorf("%s large with covers that are not live: %v", kind, err)
		}
		if err := large(kind, "R09"); err == nil || !strings.Contains(err.Error(), "R09 is not a row") {
			t.Errorf("%s large with an unknown ID: %v", kind, err)
		}
		if err := large(kind, "R01", "R03i"); err != nil {
			t.Errorf("%s large with live covers: %v", kind, err)
		}
	}
	// Read-only kinds are exempt from the checks.
	if err := large("review"); err != nil {
		t.Errorf("review large without covers: %v", err)
	}
	// An edit after the approval voids it: large is refused (the covers check, the one checklist_get uses).
	b, err := os.ReadFile(checklist.ChecklistPath(repo))
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(b), "«export to pdf»", "«export to csv»", 1)
	if err := os.WriteFile(checklist.ChecklistPath(repo), []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := large("fix", "R01"); err == nil || !strings.Contains(err.Error(), "not the repository's latest approved checklist") {
		t.Errorf("large on an edited checklist: %v", err)
	}
	// Large without a repo: covers need one.
	if _, err := m.Enqueue(StartRequest{Title: "t", Kind: "fix", Brief: "b", Model: "sonnet", Size: SizeLarge,
		Covers: []string{"R01"}}); err == nil || !strings.Contains(err.Error(), "covers need a repo") {
		t.Errorf("large without a repo: %v", err)
	}
}

// Without a size, a fix or general job's size is inferred (owner decision 2026-10-25): large with covers
// (held to the large rules: only live requirements), small without; size_required is off by default.
func TestSizeInferredFromCovers(t *testing.T) {
	m := newManager(t)
	if m.Config().SizeRequired {
		t.Fatal("size_required is on by default")
	}
	repo := sizeRepo(t, m)
	for _, kind := range SizeKinds {
		withCovers := enqueue(t, m, "b", func(r *StartRequest) {
			r.Kind, r.RepoPath, r.Covers, r.SizeRule = kind, repo, []string{"R01", "R03i"}, true
		})
		if got := get(t, m, withCovers.ID).Size; got != SizeLarge {
			t.Errorf("%s with covers: size %q, want large", kind, got)
		}
		plain := enqueue(t, m, "b", func(r *StartRequest) { r.Kind, r.RepoPath, r.SizeRule = kind, repo, true })
		if got := get(t, m, plain.ID).Size; got != SizeSmall {
			t.Errorf("%s without covers: size %q, want small", kind, got)
		}
		if _, err := m.Enqueue(StartRequest{Title: "t", Kind: kind, Brief: "b", Model: "sonnet", RepoPath: repo,
			Covers: []string{"R04"}, SizeRule: true}); err == nil || !strings.Contains(err.Error(), "only live requirements") {
			t.Errorf("%s inferred large with a deferred cover: %v", kind, err)
		}
	}
	// An explicit size is kept.
	small := enqueue(t, m, "b", func(r *StartRequest) {
		r.Kind, r.RepoPath, r.Covers, r.Size, r.SizeRule = "fix", repo, []string{"R01"}, SizeSmall, true
	})
	if got := get(t, m, small.ID).Size; got != SizeSmall {
		t.Errorf("explicit small with covers: %q", got)
	}
}
