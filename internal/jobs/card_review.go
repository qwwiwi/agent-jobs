package jobs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// reviewQuestion is one card `review_questions:` id and the question a fix/general job answers.
type reviewQuestion struct {
	ID   string
	Text string
}

// reviewQuestions is the closed set of ids, in the order they are asked.
var reviewQuestions = []reviewQuestion{
	{"race", "Which two actors (processes, goroutines, requests, jobs) can touch the same state at once, and what keeps their order safe?"},
	{"retry", "What happens if this step runs twice or is resumed after a stop (a second attempt, a repeated request)? Is the second run harmless?"},
	{"dependency", "What happens when something this change depends on (a service, file, network call, other job) is missing, slow or fails? Is the failure visible, not silent?"},
	{"rollback", "How is this change undone, and does an older version of the code still read the data and files this change writes?"},
	{"residual-risk", "What can still go wrong after this change, and what did you deliberately not cover?"},
}

// parseReviewQuestions reads one `review_questions:` value: ids separated by commas. An unknown or
// empty id is an error naming it, so a typo never silently drops a question.
func parseReviewQuestions(v string) ([]string, error) {
	var ids []string
	for _, id := range strings.Split(v, ",") {
		id = strings.TrimSpace(id)
		if !slices.ContainsFunc(reviewQuestions, func(q reviewQuestion) bool { return q.ID == id }) {
			return nil, fmt.Errorf("review_questions: %q is not a known question (want %s)", id, reviewQuestionIDs())
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// reviewQuestionIDs lists the known ids for error messages.
func reviewQuestionIDs() string {
	ids := make([]string, len(reviewQuestions))
	for i, q := range reviewQuestions {
		ids[i] = q.ID
	}
	return strings.Join(ids, ", ")
}

// canonicalReviewQuestions returns the ids of asked once each, in the order of reviewQuestions.
func canonicalReviewQuestions(asked []string) []string {
	var out []string
	for _, q := range reviewQuestions {
		if slices.Contains(asked, q.ID) {
			out = append(out, q.ID)
		}
	}
	return out
}

// renderCardChecklist is the part of a fix/general prompt that comes from the matching cards: the
// review questions to answer in the report (the union over the cards) and, when any card has ui: true,
// the screenshot matrix as acceptance. "" when no card asks for either, so a card without the keys
// leaves the prompt as it was.
func renderCardChecklist(cards []Card) string {
	var asked []string
	ui := false
	for _, c := range cards {
		asked = append(asked, c.ReviewQuestions...)
		ui = ui || c.UI
	}
	var b strings.Builder
	if ids := canonicalReviewQuestions(asked); len(ids) > 0 {
		b.WriteString("\n### Review questions\n\nThe project card asks these. Answer each one in the report, under a `### Review questions` heading in Findings / Changes, one line per id: `<id>: <answer>`. An answer says what the code does, with file:line; \"n/a\" needs a reason.\n\n")
		for _, id := range ids {
			for _, q := range reviewQuestions {
				if q.ID == id {
					fmt.Fprintf(&b, "- %s: %s\n", q.ID, q.Text)
				}
			}
		}
	}
	if ui {
		b.WriteString("\n### UI acceptance\n\nThe project card sets `ui: true`: the screenshot matrix is part of acceptance, not optional. When your diff touches UI, the job is not done until the headless browser check above has run on every changed page and its screenshots and results.json are in `<job dir>/ui-check/`. The job-done line is flagged when a UI diff has no `ui-check/` evidence.\n")
	}
	return b.String()
}

// cardChecklistFor is renderCardChecklist for the kinds that get it (fix and general, CoderKinds): "" for
// every other kind. A first prompt, the fallback prompt and the resumed session's update all take it
// from here.
func cardChecklistFor(j *store.Job, cards []Card) string {
	if !slices.Contains(CoderKinds, j.Kind) {
		return ""
	}
	return renderCardChecklist(cards)
}

// uiExtensions are the file extensions that make a path a UI file: styles, markup and component or
// template files. A path rule only (no content sniffing, no model call); static-site renderers and
// design tokens written in other extensions are not recognised.
var uiExtensions = []string{".css", ".scss", ".sass", ".less", ".html", ".htm", ".jsx", ".tsx", ".vue", ".svelte",
	".tmpl", ".gohtml", ".templ", ".hbs", ".ejs", ".njk"}

// uiSkipDirs are directories whose files are never the job's own UI.
var uiSkipDirs = []string{"node_modules", "vendor", "ui-check"}

// isUIPath reports whether a repository-relative path is a UI file by uiExtensions, outside uiSkipDirs.
func isUIPath(p string) bool {
	for _, part := range strings.Split(filepath.ToSlash(p), "/") {
		if slices.Contains(uiSkipDirs, part) {
			return false
		}
	}
	return slices.Contains(uiExtensions, strings.ToLower(filepath.Ext(p)))
}

// hasUICheckEvidence reports whether <jobDir>/ui-check/ holds the output of a browser check: ui-check/
// is a real directory (a link to one does not count) with a non-empty regular file (never a link)
// results.json or a .png/.jpg/.jpeg/.webp screenshot. A results.json that is blank or only an empty
// `{}` / `[]` is no output. It does not judge the result; it says that a check left something. Whether
// the evidence came from this attempt or an earlier one does not matter: the folder is the job's.
func hasUICheckEvidence(jobDir string) bool {
	dir := filepath.Join(jobDir, "ui-check")
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		return false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.Type() != 0 { // not a regular file: a link, a directory, a device
			continue
		}
		name := strings.ToLower(e.Name())
		isShot := slices.Contains([]string{".png", ".jpg", ".jpeg", ".webp"}, filepath.Ext(name))
		if name != "results.json" && !isShot {
			continue
		}
		fi, err := e.Info()
		if err != nil || fi.Size() == 0 {
			continue
		}
		if name == "results.json" && blankResults(filepath.Join(dir, e.Name())) {
			continue
		}
		return true
	}
	return false
}

// blankResults reports whether the results file has nothing but white space, `{}` or `[]`: what a browser
// check that did not run leaves. An unreadable file counts as blank.
func blankResults(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return true
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 64))
	if err != nil {
		return true
	}
	t := strings.TrimSpace(string(b))
	return t == "" || t == "{}" || t == "[]"
}

// maxUINoteFiles caps the changed UI files the note names.
const maxUINoteFiles = 3

// uiNoteMaxOutput caps the diff output read for the note and uiNoteMaxStderr what is kept of the diff's
// stderr; uiNoteMaxName caps one file name inside the note and uiNoteMaxRunes the whole note.
const (
	uiNoteMaxOutput = 256 << 10
	uiNoteMaxStderr = 4 << 10
	uiNoteMaxName   = 80
	uiNoteMaxRunes  = 300
)

// uiNoteTimeout bounds the whole work of the note at the finish (the card lookup and the diff). Past it
// the note is dropped. A variable only so that tests shorten it.
var uiNoteTimeout = 5 * time.Second

// errUINoteOutput is a diff output past uiNoteMaxOutput.
var errUINoteOutput = errors.New("diff output over the cap")

// recordUINote works out the job-done note once, when the attempt of j finishes with status, and stores
// it in the daemon's store on the job row, bound to that attempt (store.SetUINote). The job's own folder
// holds no part of it: nothing the job can write is ever read back as the note. The order: the stored
// note is cleared first, then the note is worked out (bounded by uiNoteTimeout), then it is stored with
// the attempt number, all before the row turns terminal. Both writes only touch an attempt that has not
// ended (the live status and the launch token; only the store also checks the attempt number), so a
// finish repeated for an ended attempt changes nothing. It never fails the finish: a clear
// that fails, a timeout, any error or a store write that fails mean no note, and the note of another
// attempt is never shown (uiEvidenceNote asks for the line's own attempt).
func (m *Manager) recordUINote(j *store.Job, status string) {
	if err := m.Store.ClearUINote(j.ID, j.LaunchToken); err != nil {
		if !errors.Is(err, store.ErrImmutable) { // ErrImmutable: the attempt already ended, nothing to do
			m.Log.Printf("job %s: ui note: clear: %v", j.ID, err)
		}
		return
	}
	if status != store.StatusSucceeded {
		return
	}
	note := m.boundedUINote(j)
	if note == "" {
		return
	}
	if err := m.Store.SetUINote(j.ID, j.LaunchToken, j.Attempt, note); err != nil {
		m.Log.Printf("job %s: ui note: store: %v", j.ID, err)
	}
}

// boundedUINote is uiNoteFor under uiNoteTimeout. The card lookup takes no deadline of its own, so the
// work runs on its own goroutine that the finish stops waiting for at the bound; the diff is killed at
// the same deadline, and the lookup's git calls end at their own bounds. A late result is dropped.
func (m *Manager) boundedUINote(j *store.Job) string {
	ctx, cancel := context.WithTimeout(context.Background(), uiNoteTimeout)
	defer cancel()
	jc := *j
	jc.Status = store.StatusSucceeded
	out := make(chan string, 1)
	go func() { out <- m.uiNoteFor(ctx, &jc) }()
	select {
	case n := <-out:
		return n
	case <-ctx.Done():
		m.Log.Printf("job %s: ui note: not ready within %s, no note", j.ID, uiNoteTimeout)
		return ""
	}
}

// uiEvidenceNote is the job-done note of a succeeded job: the value the daemon stored at the finish of
// this very attempt (j.Attempt), "" when there is none, when the stored one belongs to another attempt
// or when it cannot be read. It reads one row and never runs git or opens a file of the job.
func (m *Manager) uiEvidenceNote(j *store.Job) string {
	if j.Status != store.StatusSucceeded {
		return ""
	}
	note, err := m.Store.UINote(j.ID, j.Attempt)
	if err != nil {
		m.Log.Printf("job %s: ui note: read: %v", j.ID, err)
		return ""
	}
	return sanitizeUINote(note)
}

// uiNoteField makes s safe as one field of the one-line job-done format: printable characters only (no
// line break, no control or format character), no field separator of its own ("·" becomes "-"), at most
// max runes. "" stays "".
func uiNoteField(s string, max int) string {
	return Printable(strings.ReplaceAll(RedactPrintable(s, 0), "·", "-"), max)
}

// sanitizeUINote is a whole note under the hard cap (applied when it is built and again when it is read).
func sanitizeUINote(s string) string { return uiNoteField(s, uiNoteMaxRunes) }

// uiNoteNameRune reports whether r may stand in a file name of the note: an ASCII letter, a digit or one
// of ". _ / -".
func uiNoteNameRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
		r == '.' || r == '_' || r == '/' || r == '-'
}

// uiNoteName is a changed file's name as the note shows it. A file name is the job's own text, so it
// passes an allowlist instead of an escape per separator: every rune that is not uiNoteNameRune (a
// space, ";", "·", ":", a quote, a bracket, a control character, a non-ASCII letter, a broken byte)
// becomes "_". What is left cannot spell a separator of the job-done formats: the fields' " · ", the
// "; " between the jobs of a coalesced line, the ", " and " (+k more)" of the list. Redacted first (a
// value changed by the allowlist no longer matches its format), then cut to max bytes, a cut name
// ending in "...".
func uiNoteName(name string, max int) string {
	out := strings.Map(func(r rune) rune {
		if uiNoteNameRune(r) {
			return r
		}
		return '_'
	}, strings.ToValidUTF8(RedactText(name), "_"))
	if len(out) > max {
		out = out[:max-3] + "..."
	}
	return out
}

// uiNoteNames is the bounded list of changed UI files as the note names them: at most maxUINoteFiles
// names, each through the allowlist and cut to uiNoteMaxName (uiNoteName), then "(+k more)". This is the
// one place where file names enter the note text.
func uiNoteNames(files []string) string {
	names := make([]string, 0, maxUINoteFiles)
	for _, f := range files[:min(len(files), maxUINoteFiles)] {
		names = append(names, uiNoteName(f, uiNoteMaxName))
	}
	return capList(append(names, files[len(names):]...), maxUINoteFiles)
}

// uiNoteFor works out the note of a succeeded fix/general job in a repository whose card says
// `ui: true`, whose diff against its base has a UI file (isUIPath) and whose folder has no ui-check/
// evidence (hasUICheckEvidence). "" for everything else and for every error (a broken card, a missing
// work tree, a bad base, a diff past ctx or the output cap): the note is advice on a line, it never
// fails or holds back a job.
func (m *Manager) uiNoteFor(ctx context.Context, j *store.Job) string {
	if !slices.Contains(CoderKinds, j.Kind) || j.Status != store.StatusSucceeded || j.RepoPath == "" || j.BaseCommit == "" {
		return ""
	}
	dir := m.JobDir(j.ID)
	if hasUICheckEvidence(dir) {
		return ""
	}
	cards, err := m.CardsFor(j.RepoPath)
	if err != nil {
		m.Log.Printf("job %s: ui note: cards: %v", j.ID, RedactText(err.Error()))
		return ""
	}
	if !slices.ContainsFunc(cards, func(c Card) bool { return c.UI }) {
		return ""
	}
	work := filepath.Join(dir, "work")
	if _, err := os.Stat(work); err != nil {
		return ""
	}
	// The work tree against the base: the commits and any tracked change still uncommitted. Names only.
	out, err := cappedGit(ctx, work, uiNoteMaxOutput, "diff", "--name-only", "-z", j.BaseCommit)
	if err != nil {
		m.Log.Printf("job %s: ui note: diff: %v", j.ID, RedactText(err.Error()))
		return ""
	}
	var files []string
	for _, p := range strings.Split(out, "\x00") {
		if p != "" && isUIPath(p) {
			files = append(files, p)
		}
	}
	if len(files) == 0 {
		return ""
	}
	return sanitizeUINote("ui: true but no ui-check/ evidence for " + uiNoteNames(files))
}

// cappedWriter keeps at most max bytes; the write that would pass it marks the output over and calls stop
// (which ends git), and what follows is dropped.
type cappedWriter struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	max  int
	over bool
	stop context.CancelFunc
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.over || w.buf.Len()+len(p) > w.max {
		if !w.over {
			w.over = true
			w.stop()
		}
		return len(p), nil
	}
	return w.buf.Write(p)
}

// headWriter keeps the first max bytes written to it and discards the rest: a diagnostic stream of any
// size costs max bytes of memory. Safe for a writer that outlives the reader (an abandoned wait).
type headWriter struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (w *headWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if room := w.max - len(w.buf); room > 0 {
		w.buf = append(w.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

func (w *headWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.buf)
}

// cappedGit runs git -C dir args with the daemon's git environment until ctx ends (git's process group
// is then killed), reading at most max bytes of stdout while it streams: more is errUINoteOutput and git
// is stopped at once. Of stderr only the first uiNoteMaxStderr bytes are kept (for the error), the rest
// is discarded as it comes. Stdout is returned untrimmed.
func cappedGit(ctx context.Context, dir string, max int, args ...string) (string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	out := &cappedWriter{max: max, stop: cancel}
	errOut := &headWriter{max: uiNoteMaxStderr}
	p, err := startGit("", daemonGitEnv(), func(c *exec.Cmd) { c.Stdout, c.Stderr = out, errOut }, append([]string{"-C", dir}, args...)...)
	if err != nil {
		return "", err
	}
	err = p.wait(ctx)
	out.mu.Lock()
	defer out.mu.Unlock()
	if out.over {
		return "", errUINoteOutput
	}
	if err != nil {
		if errors.Is(err, errGitTimeout) {
			return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
		}
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errOut.String()))
	}
	return out.buf.String(), nil
}
