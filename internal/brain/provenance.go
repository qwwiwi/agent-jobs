package brain

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// JobTagPrefix marks a note written by a job: the tag is JobTagPrefix + job id. Only ForJob adds
// it; a caller's own tags with this prefix are dropped, so the tag can be neither forged nor removed.
const JobTagPrefix = "agent-jobs-job:"

var (
	reJobID     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,63}$`)
	reJobMarker = regexp.MustCompile(`agent-jobs job ([A-Za-z0-9][A-Za-z0-9-]{0,63}) wrote this note`)
)

// jobMarker is the first line of a job's note body, so a reader (and the brief) can tell the note
// from the agent's own even where the brain returns no tags.
func jobMarker(jobID string) string {
	return fmt.Sprintf("_agent-jobs job %s wrote this note; it is unreviewed._\n\n", jobID)
}

// StripJobTags returns tags without any job tag (prefix compared case-insensitively, spaces trimmed).
func StripJobTags(tags []string) []string {
	return slices.DeleteFunc(slices.Clone(tags), func(t string) bool {
		return strings.HasPrefix(strings.ToLower(strings.TrimSpace(t)), JobTagPrefix)
	})
}

// ForJob gives a note written by job jobID of agent its provenance: the caller's job tags are
// replaced by the job's own, the author is <agent>/job:<id>, and the body starts with a marker line.
// A job cannot write a decision (decisions are immutable; a job's claim needs a review first).
// ForJob is idempotent, so the job's MCP server and the daemon may both apply it.
func ForJob(n NoteInput, agent, jobID string) (NoteInput, error) {
	if !reJobID.MatchString(jobID) {
		return n, fmt.Errorf("job id %q is not a job id", jobID)
	}
	if n.Kind == KindDecision {
		return n, errors.New("a job cannot write a decision note (immutable): write a runbook or an error_pattern, " +
			"or put the decision in the report for the orchestrator")
	}
	n.Tags = append(StripJobTags(n.Tags), JobTagPrefix+jobID)
	n.Author = agent + "/job:" + jobID
	if m := jobMarker(jobID); !strings.HasPrefix(n.Body, m) {
		n.Body = m + n.Body
	}
	return n, nil
}

// NoteJob returns the id of the job that wrote n according to the marker line in its snippet, or "".
// A snippet that does not include the marker line says nothing; the daemon also keeps the paths its
// jobs wrote (jobs.Manager.MarkJobNotes).
func NoteJob(n Note) string {
	if m := reJobMarker.FindStringSubmatch(n.Snippet); m != nil {
		return m[1]
	}
	return ""
}
