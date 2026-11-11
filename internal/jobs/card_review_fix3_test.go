package jobs

import (
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// Review aa1c, blocking: "; " separates the jobs of a coalesced job-done line, so a changed file whose
// name carries it must not add job entries. The line of two jobs has exactly two job segments and none
// of the forged text.
func TestUINoteFileNameCannotForgeCoalescedJobs(t *testing.T) {
	m, j, work := uiNoteFixture(t, "ui: true\n")
	commitFile(t, work, "web/x.css; foreign-job Forged succeeded; tail.css")
	m.recordUINote(j, store.StatusSucceeded)
	other := enqueue(t, m, "second", nil)
	other = get(t, m, other.ID)
	other.Status = store.StatusSucceeded

	line := m.coalescedLine([]*store.Job{j, other})
	rest, ok := strings.CutPrefix(line, "[jobs:job-done] 2 jobs: ")
	if !ok {
		t.Fatalf("not a line of two jobs: %q", line)
	}
	if !strings.Contains(rest, uiNoteText) {
		t.Fatalf("the note is missing, the test proves nothing: %q", line)
	}
	if parts := strings.Split(rest, "; "); len(parts) != 2 {
		t.Fatalf("two jobs gave %d job segments: %q", len(parts), parts)
	}
	for _, forged := range []string{"foreign-job Forged", "Forged succeeded", "; tail.css"} {
		if strings.Contains(line, forged) {
			t.Fatalf("the line carries the forged text %q: %q", forged, line)
		}
	}
}

// A file name enters the note through an allowlist: ASCII letters, digits and . _ / - stay, every
// other rune becomes "_", and the name keeps its length cap.
func TestUINoteNamesAllowlist(t *testing.T) {
	long := "web/" + strings.Repeat("long", 50) + ".css"
	for _, c := range []struct{ what, in, want string }{
		{"plain", "web/app-1_v2.min.css", "web/app-1_v2.min.css"},
		{"space", "web/a b.css", "web/a_b.css"},
		{"semicolon", "web/a; b.css", "web/a__b.css"},
		{"middle dot", "web/a · b.css", "web/a___b.css"},
		{"newline", "web/a\nb.css", "web/a_b.css"},
		{"escape", "web/a\x1b[31mb.css", "web/a__31mb.css"},
		{"non-ASCII letter", "web/стиль-\u00e9.css", "web/_____-_.css"},
		{"punctuation", "web/a:b,c(d)[e]\"f'.css", "web/a_b_c_d__e__f_.css"},
		{"invalid UTF-8", "web/a\xffb.css", "web/a_b.css"},
		{"very long", long, long[:uiNoteMaxName-3] + "..."},
	} {
		got := uiNoteNames([]string{c.in})
		if got != c.want {
			t.Errorf("%s: %q gave %q, want %q", c.what, c.in, got, c.want)
		}
		if n := len(got); n > uiNoteMaxName {
			t.Errorf("%s: the name has %d bytes, cap %d", c.what, n, uiNoteMaxName)
		}
		if i := strings.IndexFunc(got, func(r rune) bool {
			return !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._/-", r)
		}); i >= 0 {
			t.Errorf("%s: %q carries a rune outside the allowlist at %d", c.what, got, i)
		}
	}
	// The words between the names are the daemon's own.
	if got, want := uiNoteNames([]string{"a b.css", "c;d.css", "e.css", "f.css"}), "a_b.css, c_d.css, e.css (+1 more)"; got != want {
		t.Errorf("list: got %q, want %q", got, want)
	}
}
