package jobs

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/qwwiwi/agent-jobs/internal/checklist"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// The values of job_start's size (Wave F, F9 (a)).
const (
	SizeSmall = "small"
	SizeLarge = "large"
)

// SizeKinds are the kinds that must declare a size: the ones that change code. Every other kind (review,
// research, the gate kinds slicing, judge and briefing) is exempt: it may carry a size, which is
// stored and never checked.
var SizeKinds = []string{"fix", "general"}

// SizeRuleNote is the one-line large/small rule, shown by the brief-reminder hook and in refusals.
const SizeRuleNote = "size: small for a fix or a single job; large for a new project or a feature with more than 5 " +
	"requirements, several jobs or more than a day of work (needs the repo's approved brief/CHECKLIST.md and covers " +
	"with its live requirement IDs)"

// checkSize validates r.Size against the job's kind and its covers (already checked by coverContract:
// cover is the snapshot of the covered rows of the repository's latest approved, unchanged checklist,
// nil without covers) and returns the size to store.
//
//   - Any kind: size is "", small or large.
//   - fix / general without a size: refused when r.SizeRule is set (job_start, agent-jobs start) and
//     config size_required is on; otherwise allowed and logged by Enqueue.
//   - fix / general with size large: covers must be non-empty, and every covered row a live requirement
//     (an R, Ri or G row with contract live). This holds whether size_required is on or off.
func (m *Manager) checkSize(r StartRequest, covers []string, cover *store.CoverContract) (string, error) {
	size := strings.TrimSpace(r.Size)
	if size != "" && size != SizeSmall && size != SizeLarge {
		return "", fmt.Errorf("size %q must be small or large", r.Size)
	}
	if !slices.Contains(SizeKinds, r.Kind) {
		return size, nil
	}
	switch size {
	case "":
		if r.SizeRule && m.Config().SizeRequired {
			return "", fmt.Errorf("size is required for a %s job (config size_required is on): %s", r.Kind, SizeRuleNote)
		}
	case SizeLarge:
		if len(covers) == 0 || cover == nil {
			return "", errors.New("size large needs covers: the live requirement IDs of the repo's approved " +
				checklist.ChecklistRel + " that this job works on")
		}
		var notLive []string
		for _, row := range cover.Rows {
			if !(checklist.Row{ID: row.ID, Contract: row.Contract}).Live() {
				notLive = append(notLive, row.ID+" ("+row.Contract+")")
			}
		}
		if len(notLive) > 0 {
			return "", fmt.Errorf("size large: covers may name only live requirements (R, Ri or G rows with contract live) "+
				"of checklist %s version %d; not live: %s", cover.ChecklistID, cover.Version, strings.Join(notLive, ", "))
		}
	}
	return size, nil
}

// logSize logs the size of a queued fix or general job: small, large, or none given (allowed while
// size_required is off; with it on, only a schedule firing, which does not take the rule, is queued so).
func (m *Manager) logSize(j *store.Job) {
	if !slices.Contains(SizeKinds, j.Kind) || m.Log == nil {
		return
	}
	switch j.Size {
	case "":
		why := "size_required is off"
		if m.Config().SizeRequired {
			why = "size_required is on; exempt: a schedule firing"
		}
		m.Log.Printf("job %s (%s): queued without size (%s)", j.ID, j.Kind, why)
	case SizeLarge:
		m.Log.Printf("job %s (%s): size large, covers %s of checklist %s", j.ID, j.Kind, strings.Join(j.Covers, ", "),
			j.CoverContract.ChecklistID)
	default:
		m.Log.Printf("job %s (%s): size %s", j.ID, j.Kind, j.Size)
	}
}
