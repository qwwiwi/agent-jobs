package jobs

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

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

// hasUICheckEvidence reports whether <jobDir>/ui-check/ holds the output of a browser check: a regular
// file (never a link) results.json or a .png/.jpg/.jpeg/.webp screenshot. It does not judge the
// result; it says that a check left something.
func hasUICheckEvidence(jobDir string) bool {
	entries, err := os.ReadDir(filepath.Join(jobDir, "ui-check"))
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.Type() != 0 { // not a regular file: a link, a directory, a device
			continue
		}
		name := strings.ToLower(e.Name())
		if name == "results.json" || slices.Contains([]string{".png", ".jpg", ".jpeg", ".webp"}, filepath.Ext(name)) {
			return true
		}
	}
	return false
}

// maxUINoteFiles caps the changed UI files the note names.
const maxUINoteFiles = 3

// uiEvidenceNote is the job-done note of a succeeded fix/general job in a repository whose card says
// `ui: true`, whose diff against its base has a UI file (isUIPath) and whose folder has no ui-check/
// evidence (hasUICheckEvidence). "" for everything else and for every error (a broken card, a missing
// work tree, a bad base): the note is advice on a line, it never fails or holds back a job.
func (m *Manager) uiEvidenceNote(j *store.Job) string {
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
	// The work tree against the base: the commits and any tracked change still uncommitted.
	out, err := gitLong(work, "diff", "--name-only", "-z", j.BaseCommit)
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
	return "ui: true but no ui-check/ evidence for " + capList(files, maxUINoteFiles)
}
