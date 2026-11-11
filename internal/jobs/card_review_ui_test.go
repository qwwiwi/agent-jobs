package jobs

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

const cardBodyForTests = "# P\n\n## Stack\n- Go\n"

func cardWith(front string) []byte {
	return []byte("---\nrepo: /src/p\n" + front + "---\n" + cardBodyForTests)
}

// G14: review_questions is a comma list (or a repeated key) of known ids, kept in canonical order.
func TestParseCardReviewQuestions(t *testing.T) {
	c, err := ParseCard("p", cardWith("review_questions: rollback, race\nreview_questions: race, residual-risk\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"race", "rollback", "residual-risk"}
	if !slices.Equal(c.ReviewQuestions, want) {
		t.Fatalf("ReviewQuestions = %v, want %v", c.ReviewQuestions, want)
	}
	for _, id := range []string{"race", "retry", "dependency", "rollback", "residual-risk"} {
		if _, err := ParseCard("p", cardWith("review_questions: "+id+"\n")); err != nil {
			t.Errorf("known value %q: %v", id, err)
		}
	}
}

func TestParseCardReviewQuestionsUnknownValueFails(t *testing.T) {
	for _, v := range []string{"speed", "race, speed", "Race", "race,,retry", "race,"} {
		_, err := ParseCard("p", cardWith("review_questions: "+v+"\n"))
		// The key must be known; it is the value that is refused.
		if err == nil || !strings.Contains(err.Error(), "review_questions") || strings.Contains(err.Error(), "unknown key") {
			t.Errorf("review_questions: %q: err = %v, want a refused value", v, err)
		}
	}
	if _, err := ParseCard("p", cardWith("review_questions:\n")); err == nil {
		t.Error("an empty review_questions value was accepted")
	}
}

// G15: ui is a boolean key given at most once.
func TestParseCardUI(t *testing.T) {
	c, err := ParseCard("p", cardWith("ui: true\n"))
	if err != nil || !c.UI {
		t.Fatalf("ui: true -> %+v, %v", c, err)
	}
	c, err = ParseCard("p", cardWith("ui: false\n"))
	if err != nil || c.UI {
		t.Fatalf("ui: false -> %+v, %v", c, err)
	}
	for _, v := range []string{"yes", "1", "True", "maybe"} {
		if _, err := ParseCard("p", cardWith("ui: "+v+"\n")); err == nil || !strings.Contains(err.Error(), "ui") {
			t.Errorf("ui: %q: err = %v", v, err)
		}
	}
	if _, err := ParseCard("p", cardWith("ui: true\nui: false\n")); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Errorf("ui given twice: err = %v", err)
	}
}

// A card without the new keys is what it was: no questions, no ui, and the same unknown-key refusal.
func TestParseCardWithoutNewKeysIsUnchanged(t *testing.T) {
	c, err := ParseCard("p", []byte("---\nrepo: /src/p\nremote: git@github.com:O/P.git\n---\n"+cardBodyForTests))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.ReviewQuestions) != 0 || c.UI {
		t.Fatalf("new fields set on a card without the keys: %+v", c)
	}
	if _, err := ParseCard("p", cardWith("colour: red\n")); err == nil || !strings.Contains(err.Error(), "unknown key") {
		t.Fatalf("unknown key: err = %v", err)
	}
}

func cardPrompt(t *testing.T, kind string, cards []Card) string {
	t.Helper()
	m := newManager(t)
	j := enqueue(t, m, "do the work", func(r *StartRequest) { r.Kind = kind })
	p, err := m.BuildPrompt(get(t, m, j.ID), "", cards, MemoryNotes{})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func parseTestCard(t *testing.T, front string) Card {
	t.Helper()
	c, err := ParseCard("p", cardWith(front))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// G14: the matching questions, and only they, are in the fix and general prompts, with the order to
// answer them in the report.
func TestPromptCarriesCardReviewQuestions(t *testing.T) {
	card := parseTestCard(t, "review_questions: race, rollback\n")
	for _, kind := range []string{"fix", "general"} {
		p := cardPrompt(t, kind, []Card{card})
		for _, want := range []string{"### Review questions", "race:", "rollback:", "answer", "report"} {
			if !strings.Contains(p, want) {
				t.Errorf("kind %s: prompt lacks %q", kind, want)
			}
		}
		for _, absent := range []string{"retry:", "dependency:", "residual-risk:"} {
			if strings.Contains(p, absent) {
				t.Errorf("kind %s: prompt has the unrequested question %q", kind, absent)
			}
		}
	}
	// Two matching cards: the union, each question once.
	other := parseTestCard(t, "review_questions: race, retry\n")
	p := cardPrompt(t, "fix", []Card{card, other})
	if n := strings.Count(p, "\n- race:"); n != 1 {
		t.Errorf("race asked %d times, want once", n)
	}
	if !strings.Contains(p, "- retry:") || !strings.Contains(p, "- rollback:") {
		t.Error("union of two cards lost a question")
	}
}

// G15: ui: true is stated as acceptance in the prompt.
func TestPromptStatesUIAcceptance(t *testing.T) {
	card := parseTestCard(t, "ui: true\n")
	for _, kind := range []string{"fix", "general"} {
		p := cardPrompt(t, kind, []Card{card})
		for _, want := range []string{"### UI acceptance", "ui-check/", "360x740", "not optional"} {
			if !strings.Contains(p, want) {
				t.Errorf("kind %s: prompt lacks %q", kind, want)
			}
		}
	}
}

// A card without the keys adds nothing to the prompt, and a research or review job gets neither block.
func TestPromptWithoutCardKeysIsUnchanged(t *testing.T) {
	plain := parseTestCard(t, "")
	for _, kind := range []string{"fix", "general"} {
		p := cardPrompt(t, kind, []Card{plain})
		for _, absent := range []string{"### Review questions", "### UI acceptance"} {
			if strings.Contains(p, absent) {
				t.Errorf("kind %s: plain card added %q", kind, absent)
			}
		}
	}
	// Byte for byte: the plain card adds its own section and nothing else.
	m := newManager(t)
	j := get(t, m, enqueue(t, m, "do the work", func(r *StartRequest) { r.Kind = "fix" }).ID)
	without, err := m.BuildPrompt(j, "", nil, MemoryNotes{})
	if err != nil {
		t.Fatal(err)
	}
	with, err := m.BuildPrompt(j, "", []Card{plain}, MemoryNotes{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Replace(with, RenderCards([]Card{plain}), "", 1) != without {
		t.Error("a card without the new keys changed the prompt beyond its own section")
	}
	both := parseTestCard(t, "review_questions: race\nui: true\n")
	for _, kind := range []string{"research", "review"} {
		p := cardPrompt(t, kind, []Card{both})
		for _, absent := range []string{"### Review questions", "### UI acceptance"} {
			if strings.Contains(p, absent) {
				t.Errorf("kind %s got %q", kind, absent)
			}
		}
	}
}

func TestIsUIPath(t *testing.T) {
	for p, want := range map[string]bool{
		"web/app.css": true, "a/b.SCSS": true, "index.html": true, "x/Page.tsx": true, "x/y.jsx": true,
		"c.vue": true, "c.svelte": true, "t/base.tmpl": true, "t/page.gohtml": true, "t/p.templ": true,
		"main.go": false, "README.md": false, "docs/page.md": false, "script.js": false,
		"node_modules/pkg/index.html": false, "web/vendor/x.css": false, "ui-check/results.json": false,
	} {
		if got := isUIPath(p); got != want {
			t.Errorf("isUIPath(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestHasUICheckEvidence(t *testing.T) {
	dir := t.TempDir()
	if hasUICheckEvidence(dir) {
		t.Fatal("no ui-check/ directory counts as evidence")
	}
	ev := filepath.Join(dir, "ui-check")
	if err := os.Mkdir(ev, 0o700); err != nil {
		t.Fatal(err)
	}
	if hasUICheckEvidence(dir) {
		t.Fatal("an empty ui-check/ counts as evidence")
	}
	put := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(ev, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	put("notes.txt", "x")
	if hasUICheckEvidence(dir) {
		t.Fatal("a text file counts as evidence")
	}
	if err := os.Symlink("/etc/hostname", filepath.Join(ev, "shot.png")); err != nil {
		t.Fatal(err)
	}
	if hasUICheckEvidence(dir) {
		t.Fatal("a symlink counts as evidence")
	}
	for _, empty := range []string{"", "{}", " {} \n", "[]", "\n"} {
		put("results.json", empty)
		if hasUICheckEvidence(dir) {
			t.Fatalf("results.json %q counts as evidence", empty)
		}
	}
	put("shot2.png", "")
	if hasUICheckEvidence(dir) {
		t.Fatal("an empty screenshot counts as evidence")
	}
	put("results.json", `{"runs":[{"device":"360x740","ok":true}]}`)
	if !hasUICheckEvidence(dir) {
		t.Fatal("a results.json with content is not evidence")
	}
	if err := os.Remove(filepath.Join(ev, "results.json")); err != nil {
		t.Fatal(err)
	}
	put("360x740-light.png", "png")
	if !hasUICheckEvidence(dir) {
		t.Fatal("a screenshot is not evidence")
	}
}

// A ui-check/ that is itself a link does not count, whatever it points at.
func TestHasUICheckEvidenceSymlinkedDir(t *testing.T) {
	dir := t.TempDir()
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "shot.png"), []byte("png"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "ui-check")); err != nil {
		t.Fatal(err)
	}
	if hasUICheckEvidence(dir) {
		t.Fatal("a symlinked ui-check/ counts as evidence")
	}
}

// uiNoteFixture is a manager with a repo, a ui: true card for it, and a job whose work tree is a
// worktree of the repo at the base commit.
func uiNoteFixture(t *testing.T, cardFront string) (*Manager, *store.Job, string) {
	t.Helper()
	m := newManager(t)
	repo := newRepo(t)
	if err := os.MkdirAll(m.CardsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	card := "---\nrepo: " + repo + "\n" + cardFront + "---\n" + cardBodyForTests
	if err := os.WriteFile(filepath.Join(m.CardsDir(), "p.md"), []byte(card), 0o600); err != nil {
		t.Fatal(err)
	}
	j := enqueue(t, m, "ui work", func(r *StartRequest) { r.Kind, r.RepoPath = "fix", repo })
	j = get(t, m, j.ID)
	j.BaseCommit = gitT(t, repo, "rev-parse", "HEAD")
	j.Status = store.StatusSucceeded
	dir := m.JobDir(j.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(dir, "work")
	gitT(t, repo, "worktree", "add", "--detach", work, j.BaseCommit)
	gitT(t, work, "config", "user.name", "T")
	gitT(t, work, "config", "user.email", "t@example.com")
	return m, j, work
}

func commitFile(t *testing.T, work, rel string) {
	t.Helper()
	p := filepath.Join(work, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, work, "add", ".")
	gitT(t, work, "commit", "-q", "-m", "change "+rel)
}

const uiNoteText = "ui: true but no ui-check/ evidence"

// uiNote is the note of the job-done line as the daemon gets it: computed once at the finish
// (recordUINote) and read back from what the finish stored (uiEvidenceNote).
func uiNote(t *testing.T, m *Manager, j *store.Job) string {
	t.Helper()
	m.recordUINote(j, j.Status)
	return m.uiEvidenceNote(j)
}

// The real git, found once before any fake is on PATH (a second fakeGitDiff in one test must not call the first).
var (
	realGitOnce sync.Once
	realGitPath string
	realGitErr  error
)

// fakeGitDiff puts a git first on PATH that runs body (a sh snippet) for a `diff` call and the real git
// for every other call.
func fakeGitDiff(t *testing.T, body string) {
	t.Helper()
	realGitOnce.Do(func() { realGitPath, realGitErr = exec.LookPath("git") })
	if realGitErr != nil {
		t.Fatal(realGitErr)
	}
	real := realGitPath
	if strings.ContainsRune(real, '\'') {
		t.Fatalf("git path %q has a quote", real)
	}
	fakeGitOnPath(t, "for a in \"$@\"; do\n  if [ \"$a\" = diff ]; then\n"+body+"\n  fi\ndone\nexec '"+real+"' \"$@\"\n")
}

// shortUINoteTimeout sets the bound of the finish-time note to d for the test.
func shortUINoteTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	orig := uiNoteTimeout
	uiNoteTimeout = d
	t.Cleanup(func() { uiNoteTimeout = orig })
}

func TestUIEvidenceNote(t *testing.T) {
	m, j, work := uiNoteFixture(t, "ui: true\n")
	if n := uiNote(t, m, j); n != "" {
		t.Fatalf("no diff: note %q", n)
	}
	commitFile(t, work, "docs/notes.txt")
	if n := uiNote(t, m, j); n != "" {
		t.Fatalf("a non-UI diff: note %q", n)
	}
	commitFile(t, work, "web/app.css")
	n := uiNote(t, m, j)
	if !strings.Contains(n, uiNoteText) || !strings.Contains(n, "web/app.css") {
		t.Fatalf("UI diff without evidence: note %q", n)
	}
	// The note is on the job-done line.
	if line := m.DoneLineFor(j); !strings.Contains(line, uiNoteText) {
		t.Fatalf("job-done line lacks the note: %s", line)
	}
	// Evidence silences it.
	ev := filepath.Join(m.JobDir(j.ID), "ui-check")
	if err := os.Mkdir(ev, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ev, "results.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if n := uiNote(t, m, j); !strings.Contains(n, uiNoteText) {
		t.Fatalf("an empty {} results.json silenced the note: %q", n)
	}
	if err := os.WriteFile(filepath.Join(ev, "results.json"), []byte(`{"runs":[{"ok":true}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if n := uiNote(t, m, j); n != "" {
		t.Fatalf("with evidence: note %q", n)
	}
	// Evidence left by an earlier attempt counts: the folder is the job's, not the attempt's.
	if err := os.MkdirAll(filepath.Join(m.JobDir(j.ID), "attempts", "1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if n := uiNote(t, m, j); n != "" {
		t.Fatalf("evidence from an earlier attempt: note %q", n)
	}
	// Only a succeeded job gets it.
	if err := os.RemoveAll(ev); err != nil {
		t.Fatal(err)
	}
	j.Status = store.StatusFailed
	if n := uiNote(t, m, j); n != "" {
		t.Fatalf("failed job: note %q", n)
	}
}

func TestUIEvidenceNoteNeedsUICard(t *testing.T) {
	for _, front := range []string{"", "ui: false\n", "review_questions: race\n"} {
		m, j, work := uiNoteFixture(t, front)
		commitFile(t, work, "web/app.css")
		if n := uiNote(t, m, j); n != "" {
			t.Errorf("card front %q: note %q", front, n)
		}
	}
}

// The note never fails a job: a missing work tree, a broken card or a bad base give no note and no panic.
func TestUIEvidenceNoteNeverFails(t *testing.T) {
	m, j, work := uiNoteFixture(t, "ui: true\n")
	commitFile(t, work, "web/app.css")
	bad := *j
	bad.BaseCommit = "deadbeef"
	if n := uiNote(t, m, &bad); n != "" {
		t.Errorf("unknown base: note %q", n)
	}
	if err := os.WriteFile(filepath.Join(m.CardsDir(), "p.md"), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if n := uiNote(t, m, j); n != "" {
		t.Errorf("broken card: note %q", n)
	}
	if line := m.DoneLineFor(j); !strings.HasPrefix(line, doneLinePrefix) {
		t.Errorf("job-done line is broken: %s", line)
	}
	if err := os.RemoveAll(work); err != nil {
		t.Fatal(err)
	}
	if n := uiNote(t, m, j); n != "" {
		t.Errorf("no work tree: note %q", n)
	}
}

// A new card shows the optional keys as comments: still a valid card with neither set, and a comment
// is skipped by an older daemon's parser too.
func TestNewCardTemplateShowsOptionalKeys(t *testing.T) {
	m := newManager(t)
	repo := newRepo(t)
	p, err := m.NewCard(repo, "tpl")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# review_questions: race, retry, dependency, rollback, residual-risk", "# ui: true"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("new card lacks the commented key %q", want)
		}
	}
	c, err := ParseCard("tpl", b)
	if err != nil || len(c.ReviewQuestions) != 0 || c.UI {
		t.Fatalf("new card parses to %+v, %v", c, err)
	}
}
