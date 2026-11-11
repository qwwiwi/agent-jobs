package jobs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// failUINoteWrites makes every write of the stored note fail (when = "") or only the ones matching when.
func failUINoteWrites(t *testing.T, m *Manager, when string) {
	t.Helper()
	if when != "" {
		when = " WHEN " + when
	}
	if _, err := rawDB(t, m).Exec(`CREATE TRIGGER ui_note_fail BEFORE UPDATE OF ui_note ON jobs` + when + `
		BEGIN SELECT RAISE(ABORT, 'simulated write failure'); END`); err != nil {
		t.Fatalf("trigger: %v", err)
	}
}

// seedForeignNoteFile plants what a job (or an earlier build) could leave in its own folder: a
// ui-note.txt naming another job's file, and a non-empty directory at the old temp name.
func seedForeignNoteFile(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "ui-note.txt.tmp", "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	foreign := uiNoteText + " for web/foreign.css\n"
	if err := os.WriteFile(filepath.Join(dir, "ui-note.txt"), []byte(foreign), 0o600); err != nil {
		t.Fatal(err)
	}
}

// F1 (control review 8ca5): a ui-note.txt in the job's folder is never the note. The reviewer's
// reproduction: a foreign file is there and the replacement cannot be written; the line shows this
// attempt's own note, worked out by the daemon, and never the file's.
func TestForeignUINoteFileIsNeverShown(t *testing.T) {
	m, j, work := uiNoteFixture(t, "ui: true\n")
	commitFile(t, work, "web/current.css")
	seedForeignNoteFile(t, m.JobDir(j.ID))
	m.recordUINote(j, store.StatusSucceeded)
	line := m.DoneLineFor(j)
	if strings.Contains(line, "web/foreign.css") {
		t.Fatalf("the line shows the note of a file in the job dir: %s", line)
	}
	if !strings.Contains(line, uiNoteText) || !strings.Contains(line, "web/current.css") {
		t.Fatalf("the line lacks this attempt's note: %s", line)
	}
	// And with no note of this attempt at all (no UI diff), the file still is not one.
	m2, j2, _ := uiNoteFixture(t, "ui: true\n")
	seedForeignNoteFile(t, m2.JobDir(j2.ID))
	m2.recordUINote(j2, store.StatusSucceeded)
	if line := m2.DoneLineFor(j2); strings.Contains(line, uiNoteText) {
		t.Fatalf("a planted file became the note: %s", line)
	}
}

// F1: the stored note belongs to the attempt it was worked out for. The line of the next attempt never
// shows it, also when nothing cleared it.
func TestUINoteOfEarlierAttemptIsNotShown(t *testing.T) {
	m, j, work := uiNoteFixture(t, "ui: true\n")
	commitFile(t, work, "web/app.css")
	m.recordUINote(j, store.StatusSucceeded)
	if line := m.DoneLineFor(j); !strings.Contains(line, "web/app.css") {
		t.Fatalf("precondition: attempt %d has no note: %s", j.Attempt, line)
	}
	if _, err := rawDB(t, m).Exec(`UPDATE jobs SET attempt=attempt+1 WHERE id=?`, j.ID); err != nil {
		t.Fatal(err)
	}
	next := get(t, m, j.ID)
	next.Status = store.StatusSucceeded
	if next.Attempt != j.Attempt+1 {
		t.Fatalf("attempt: %d, want %d", next.Attempt, j.Attempt+1)
	}
	if line := m.DoneLineFor(next); strings.Contains(line, uiNoteText) {
		t.Fatalf("the line of attempt %d shows the note of attempt %d: %s", next.Attempt, j.Attempt, line)
	}
	// The earlier attempt's own line still has it (a re-delivery).
	if line := m.DoneLineFor(j); !strings.Contains(line, "web/app.css") {
		t.Fatalf("attempt %d lost its note: %s", j.Attempt, line)
	}
}

// F1: a store write that fails means no note and never a failed finish: the job still ends succeeded,
// whichever of the two writes (the clear, the note itself) fails.
func TestFailedUINoteStoreWriteGivesNoNote(t *testing.T) {
	for name, when := range map[string]string{"clear fails": "", "store fails": "NEW.ui_note IS NOT NULL"} {
		t.Run(name, func(t *testing.T) {
			m := newManager(t)
			repo := newRepo(t)
			writeCardFor(t, m, repo, "ui: true\n")
			id := enqueue(t, m, "FAKE_UI_COMMIT", func(r *StartRequest) { r.Kind, r.RepoPath = "fix", repo }).ID
			failUINoteWrites(t, m, when)
			j := runToEnd(t, m, id)
			if j.Status != store.StatusSucceeded {
				t.Fatalf("a failed note write changed the finish: %s %q", j.Status, j.Error)
			}
			if line := m.DoneLineFor(j); strings.Contains(line, uiNoteText) {
				t.Fatalf("a note that was not stored is on the line: %s", line)
			}
		})
	}
}

// F1: a finish repeated for an attempt that already ended does not touch the stored note.
func TestUINoteNotRewrittenAfterFinish(t *testing.T) {
	m := newManager(t)
	repo := newRepo(t)
	writeCardFor(t, m, repo, "ui: true\n")
	id := enqueue(t, m, "FAKE_UI_COMMIT", func(r *StartRequest) { r.Kind, r.RepoPath = "fix", repo }).ID
	j := runToEnd(t, m, id)
	if line := m.DoneLineFor(j); !strings.Contains(line, "web/app.css") {
		t.Fatalf("precondition: no note: %s", line)
	}
	for _, status := range []string{store.StatusFailed, store.StatusSucceeded} {
		m.recordUINote(j, status)
		if line := m.DoneLineFor(j); !strings.Contains(line, "web/app.css") {
			t.Fatalf("a second finish step (%s) changed the stored note: %s", status, line)
		}
	}
}

// The note is one bounded piece of the one-line job-done format whatever the changed files are called:
// no line break, no field separator of its own, a hard cap on its length.
func TestUINoteCannotForgeTheDoneLine(t *testing.T) {
	m, j, work := uiNoteFixture(t, "ui: true\n")
	commitFile(t, work, "web/x · "+OwnerWaitsMarker+" · y.css")
	commitFile(t, work, "web/two\nlines\x1b[31m.css")
	commitFile(t, work, "web/"+strings.Repeat("long", 50)+".css")
	plain := m.DoneLineFor(j) // nothing stored yet
	m.recordUINote(j, store.StatusSucceeded)
	line := m.DoneLineFor(j)
	note, ok := strings.CutPrefix(line, plain+" · ")
	if !ok || !strings.HasPrefix(note, uiNoteText) {
		t.Fatalf("the note is not the line's last field: %q (without it %q)", line, plain)
	}
	if strings.ContainsAny(note, "\n\r\x1b·") {
		t.Fatalf("the note carries a control character or a field separator: %q", note)
	}
	if strings.Contains(note, "· "+OwnerWaitsMarker) {
		t.Fatalf("a file name forged a field: %q", note)
	}
	if n := len([]rune(note)); n > 300 {
		t.Fatalf("the note has %d runes, cap 300", n)
	}
}

// Review 8ca5, non-blocking: the bounded git call keeps a bounded piece of stderr too. A git that
// floods stderr and fails gives an error of a bounded size, not megabytes held in memory.
func TestCappedGitBoundsStderr(t *testing.T) {
	fakeGitOnPath(t, "head -c 4194304 /dev/zero | tr '\\000' 'e' >&2\nexit 3\n")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := cappedGit(ctx, t.TempDir(), uiNoteMaxOutput, "diff", "--name-only")
	if err == nil {
		t.Fatal("a failing git gave no error")
	}
	if n := len(err.Error()); n > 16<<10 {
		t.Fatalf("the error of a stderr flood has %d bytes: stderr is not capped", n)
	}
	if !strings.Contains(err.Error(), "eeee") {
		t.Fatalf("the kept head of stderr is missing: %q", err)
	}
}
