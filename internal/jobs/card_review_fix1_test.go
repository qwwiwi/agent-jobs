package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// writeCardFor writes the project card "p" for repo with front as its extra front matter keys.
func writeCardFor(t *testing.T, m *Manager, repo, front string) {
	t.Helper()
	if err := os.MkdirAll(m.CardsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	card := "---\nrepo: " + repo + "\n" + front + "---\n" + cardBodyForTests
	if err := os.WriteFile(filepath.Join(m.CardsDir(), "p.md"), []byte(card), 0o600); err != nil {
		t.Fatal(err)
	}
}

const (
	reviewHeading = "\n### Review questions\n"
	uiHeading     = "\n### UI acceptance\n"
)

// F1: the note is worked out once, when the job finishes, and stored with the job; building or
// re-delivering the done line only reads it and never runs git (a slow or hung git cannot hold a line).
func TestDoneLineReadsStoredUINoteWithoutGit(t *testing.T) {
	m, j, work := uiNoteFixture(t, "ui: true\n")
	commitFile(t, work, "web/app.css")
	m.recordUINote(j, store.StatusSucceeded) // the finish step, git still healthy
	mark := filepath.Join(t.TempDir(), "diff.called")
	fakeGitDiff(t, "echo called >> '"+mark+"'\n    exec sleep 60")
	shortGitLongTimeout(t, 3*time.Second) // red on the old code: a failure after 3 s, not a 10 minute hang
	start := time.Now()
	for i := 0; i < 2; i++ { // a re-delivery reads the same stored value
		if line := m.DoneLineFor(j); !strings.Contains(line, uiNoteText) || !strings.Contains(line, "web/app.css") {
			t.Fatalf("delivery %d: the line lacks the stored note: %s", i+1, line)
		}
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("building the done line took %s with a slow git", took)
	}
	if _, err := os.Stat(mark); err == nil {
		t.Fatal("building the done line ran git diff")
	}
}

// F1: the finish step is bounded: a git that hangs gives "no note" within the bound, and a note left by an
// earlier attempt is replaced by it; an endless output is cut while streaming (an error: no note).
func TestRecordUINoteIsBounded(t *testing.T) {
	m, j, work := uiNoteFixture(t, "ui: true\n")
	commitFile(t, work, "web/app.css")
	if n := uiNote(t, m, j); !strings.Contains(n, "web/app.css") {
		t.Fatalf("precondition: healthy git gave note %q", n)
	}
	shortUINoteTimeout(t, 500*time.Millisecond)
	shortGitLongTimeout(t, 5*time.Second)

	fakeGitDiff(t, "exec sleep 60")
	start := time.Now()
	m.recordUINote(j, store.StatusSucceeded)
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("a hung git held the finish step for %s", took)
	}
	if n := m.uiEvidenceNote(j); n != "" {
		t.Fatalf("a timed-out note left %q", n)
	}

	shortUINoteTimeout(t, 5*time.Second)
	fakeGitDiff(t, "printf 'web/app.css\\000'")
	if n := uiNote(t, m, j); !strings.Contains(n, "web/app.css") {
		t.Fatalf("fake git with a short output: note %q", n)
	}
	shortUINoteTimeout(t, 20*time.Second) // only the output cap can end the next one early
	fakeGitDiff(t, "yes 'web/a.css' | tr '\\n' '\\000'")
	start = time.Now()
	m.recordUINote(j, store.StatusSucceeded)
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("an endless output was read for %s", took)
	}
	if n := m.uiEvidenceNote(j); n != "" {
		t.Fatalf("a cut output left %q", n)
	}
}

// F1: the finish of a real job stores the note: its done line carries it with git hung.
func TestFinishStoresUINote(t *testing.T) {
	m := newManager(t)
	repo := newRepo(t)
	writeCardFor(t, m, repo, "ui: true\n")
	id := enqueue(t, m, "FAKE_UI_COMMIT", func(r *StartRequest) { r.Kind, r.RepoPath = "fix", repo }).ID
	j := runToEnd(t, m, id)
	if j.Status != store.StatusSucceeded {
		t.Fatalf("job: %s %q", j.Status, j.Error)
	}
	fakeGitDiff(t, "exec sleep 60")
	shortGitLongTimeout(t, 3*time.Second)
	start := time.Now()
	line := m.DoneLineFor(j)
	if !strings.Contains(line, uiNoteText) || !strings.Contains(line, "web/app.css") {
		t.Fatalf("the line of a finished ui job lacks the note: %s", line)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("the done line took %s with a slow git", took)
	}
}

// A note of an attempt that ended otherwise is not read: only a succeeded job carries it.
func TestStoredUINoteOnlyForSucceeded(t *testing.T) {
	m, j, work := uiNoteFixture(t, "ui: true\n")
	commitFile(t, work, "web/app.css")
	if n := uiNote(t, m, j); n == "" {
		t.Fatal("precondition: no note")
	}
	j.Status = store.StatusFailed
	if n := m.uiEvidenceNote(j); n != "" {
		t.Fatalf("failed job reads %q", n)
	}
	m.recordUINote(j, store.StatusFailed)
	j.Status = store.StatusSucceeded
	if n := m.uiEvidenceNote(j); n != "" {
		t.Fatalf("a failed attempt's finish left %q", n)
	}
}

// resumeCardFixture stops a repository job of kind on a card without the new keys, then gives the
// card review_questions and ui: true, as when the card is edited between attempts.
func resumeCardFixture(t *testing.T, m *Manager, kind string) *store.Job {
	t.Helper()
	repo := newRepo(t)
	writeCardFor(t, m, repo, "")
	st := startAndStop(t, m, func(r *StartRequest) { r.Kind, r.RepoPath = kind, repo })
	writeCardFor(t, m, repo, "review_questions: race, rollback\nui: true\n")
	return st
}

func requireOnce(t *testing.T, what, text string, headings ...string) {
	t.Helper()
	for _, h := range headings {
		if n := strings.Count(text, h); n != 1 {
			t.Errorf("%s has %q %d times, want once", what, h, n)
		}
	}
}

// F2: a resume with an intact session gets the card's current block in the resumed prompt, once; the
// fallback prompt (not the one used here) has it once too.
func TestResumeIntactSessionCarriesCardBlock(t *testing.T) {
	for _, kind := range []string{"fix", "general"} {
		t.Run(kind, func(t *testing.T) {
			m := newManager(t)
			sessionsHome(t, true)
			st := resumeCardFixture(t, m, kind)
			if _, err := m.Resume(st.ID, "go on", Limits{MaxBudgetUSD: 4}); err != nil {
				t.Fatal(err)
			}
			j := runToEnd(t, m, st.ID)
			if j.RunMode != store.RunResume {
				t.Fatalf("the session was not resumed: run mode %q", j.RunMode)
			}
			jd := m.JobDir(st.ID)
			resumed := readFile(t, filepath.Join(jd, resumePromptFile))
			requireOnce(t, "the resumed prompt", resumed, reviewHeading, uiHeading)
			for _, want := range []string{"- race:", "- rollback:"} {
				if !strings.Contains(resumed, want) {
					t.Errorf("the resumed prompt lacks %q", want)
				}
			}
			if strings.Contains(resumed, "- retry:") {
				t.Error("the resumed prompt asks a question the card does not")
			}
			requireOnce(t, "the fallback prompt", readFile(t, filepath.Join(jd, fallbackPromptFile)), reviewHeading, uiHeading)
		})
	}
}

// F2: the fresh-session fallback gets the block once, from the card as it is at the resume.
func TestResumeFallbackCarriesCardBlockOnce(t *testing.T) {
	m := newManager(t)
	sessionsHome(t, false) // no transcript on disk: the session cannot be resumed
	st := resumeCardFixture(t, m, "fix")
	if _, err := m.Resume(st.ID, "go on", Limits{MaxBudgetUSD: 4}); err != nil {
		t.Fatal(err)
	}
	j := runToEnd(t, m, st.ID)
	if j.RunMode != store.RunFallback {
		t.Fatalf("the session was resumed, want a fallback: run mode %q", j.RunMode)
	}
	fb := readFile(t, filepath.Join(m.JobDir(st.ID), fallbackPromptFile))
	requireOnce(t, "the fallback prompt", fb, reviewHeading, uiHeading)
}

// F2: a card without the keys adds nothing to a resumed prompt, and neither does a review job's resume.
func TestResumeWithoutCardKeysHasNoBlock(t *testing.T) {
	m := newManager(t)
	sessionsHome(t, true)
	repo := newRepo(t)
	writeCardFor(t, m, repo, "")
	st := startAndStop(t, m, func(r *StartRequest) { r.Kind, r.RepoPath = "fix", repo })
	if _, err := m.Resume(st.ID, "go on", Limits{MaxBudgetUSD: 4}); err != nil {
		t.Fatal(err)
	}
	runToEnd(t, m, st.ID)
	jd := m.JobDir(st.ID)
	for _, f := range []string{resumePromptFile, fallbackPromptFile} {
		if s := readFile(t, filepath.Join(jd, f)); strings.Contains(s, reviewHeading) || strings.Contains(s, uiHeading) {
			t.Errorf("%s has a card block for a card without the keys", f)
		}
	}
}

// Baseline: the prompts of a card without the new keys, and the done line without a note, are what they
// were before S6 byte for byte. The fixtures were written by the code at 6d81853 (the commit before S6):
// the job id and the job dir are replaced by <ID> and <JOBDIR>.
func TestPreS6BaselinePrompts(t *testing.T) {
	for _, kind := range []string{"fix", "general"} {
		m := newManager(t)
		j := get(t, m, enqueue(t, m, "do the work", func(r *StartRequest) { r.Kind = kind }).ID)
		card := parseTestCard(t, "")
		got, err := m.BuildPrompt(j, "", []Card{card}, MemoryNotes{})
		if err != nil {
			t.Fatal(err)
		}
		got = strings.ReplaceAll(got, m.JobDir(j.ID), "<JOBDIR>")
		got = strings.ReplaceAll(got, j.ID, "<ID>")
		want, err := os.ReadFile(filepath.Join("testdata", "pre-s6", "prompt-"+kind+".golden"))
		if err != nil {
			t.Fatal(err)
		}
		if got != string(want) {
			t.Errorf("kind %s: the prompt of a card without the new keys differs from the pre-S6 baseline", kind)
		}
	}
}

func TestPreS6BaselineDoneLine(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("testdata", "pre-s6", "doneline.golden"))
	if err != nil {
		t.Fatal(err)
	}
	j := store.Job{ID: "job-fixed", Title: "fix it", Kind: "fix", Status: store.StatusSucceeded, Turns: 7, CostUSD: 0.42,
		DurationSec: 600, Model: "sonnet", ReasoningEffort: "high", Attempt: 1, MaxTurns: 40, MaxBudgetUSD: 5, TimeoutMinutes: 60}
	if got := DoneLine(&j, 0); got != string(want) {
		t.Errorf("DoneLine differs from the pre-S6 baseline:\n got %q\nwant %q", got, want)
	}
	// The line as notify builds it, for a repository job of a ui: true card with no UI diff: no note.
	m, fixture, _ := uiNoteFixture(t, "ui: true\n")
	m.recordUINote(fixture, store.StatusSucceeded)
	j.ID, j.RepoPath, j.BaseCommit = fixture.ID, fixture.RepoPath, fixture.BaseCommit
	wantFor := strings.Replace(string(want), "job-fixed", fixture.ID, 1)
	if got := m.DoneLineFor(&j); got != wantFor {
		t.Errorf("DoneLineFor without a note differs from the pre-S6 baseline:\n got %q\nwant %q", got, wantFor)
	}
}
