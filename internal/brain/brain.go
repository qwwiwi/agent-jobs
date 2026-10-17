// Package brain is the agent's view of the team's shared memory and task service ("shared brain",
// gbrain). The memory, task and team MCP tools keep the reference's names but are backed by it
// instead of a store of their own (docs/DEVIATIONS.md #2, #3):
//
//   - memory_search, memory_already_exists → recall server, tool recall;
//   - memory_write → memory server, create_decision_note / create_runbook_note / create_error_pattern_note;
//   - task_create, task_list, task_update, task_get → tasks server, task_* tools (status changes go
//     through task_start / task_review / task_done / task_block / task_reopen);
//   - send_message, inbox → swarm server, notify / list_my_pending / ack.
//
// Brain is the interface the tools and the daemon use. Client talks to the real service over MCP
// streamable HTTP; Disabled is used when config.json has no brain; Fake is an in-memory brain for tests.
package brain

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// ErrNotConfigured is returned by every call when the agent has no brain configured.
var ErrNotConfigured = errors.New(`brain not configured: add a "brain" block to <state>/config.json (README, section "Shared brain")`)

// ErrNotFound is returned when the brain has no object with the requested id.
var ErrNotFound = errors.New("not found")

// Object is a JSON object as the brain returns it (a task, a delivery, a write result). Its shape is
// owned by the brain; agent-jobs passes it through.
type Object = map[string]any

// Note is one search hit from shared memory.
type Note struct {
	Path       string  `json:"path"`
	Scope      string  `json:"scope,omitempty"`
	SourceType string  `json:"source_type,omitempty"`
	Score      float64 `json:"score"`
	Snippet    string  `json:"snippet"`
	// FromJob is the job that wrote the note (set by agent-jobs from its own records and the note's
	// marker line, never trusted from the brain): such a note is unreviewed.
	FromJob string `json:"from_job,omitempty"`
}

// Note kinds memory_write can create.
const (
	KindDecision     = "decision"
	KindRunbook      = "runbook"
	KindErrorPattern = "error_pattern"
)

// NoteKinds lists the kinds memory_write accepts.
var NoteKinds = []string{KindDecision, KindRunbook, KindErrorPattern}

// NoteInput is a note to write. Category, Severity, TriggerCondition and PreventionRule are required
// for error_pattern and refused for other kinds; Related only applies to decision.
type NoteInput struct {
	Kind             string
	Title            string
	Body             string
	Tags             []string
	Related          []string
	Category         string
	Severity         string
	TriggerCondition string
	PreventionRule   string
	// Author overrides the client's author name ("" = the agent). Set by ForJob for a job's note.
	Author string
}

// Validate checks a note before it is sent.
func (n NoteInput) Validate() error {
	if !slices.Contains(NoteKinds, n.Kind) {
		return fmt.Errorf("kind %q must be one of %s", n.Kind, strings.Join(NoteKinds, ", "))
	}
	if strings.TrimSpace(n.Title) == "" || strings.TrimSpace(n.Body) == "" {
		return errors.New("title and body are required")
	}
	if len(n.Tags) == 0 {
		return errors.New("at least one tag is required")
	}
	ep := []string{n.Category, n.Severity, n.TriggerCondition, n.PreventionRule}
	if n.Kind == KindErrorPattern {
		if slices.Contains(ep, "") {
			return errors.New("error_pattern needs category, severity, trigger_condition and prevention_rule")
		}
	} else if strings.Join(ep, "") != "" {
		return errors.New("category, severity, trigger_condition and prevention_rule apply to error_pattern only")
	}
	if len(n.Related) > 0 && n.Kind != KindDecision {
		return errors.New("related applies to decision only")
	}
	if n.Author != "" && !reAuthor.MatchString(n.Author) {
		return fmt.Errorf("author %q is not an agent name or <agent>/job:<id>", n.Author)
	}
	return nil
}

// TaskInput is a new task.
type TaskInput struct {
	Title       string
	Description string
	Assignee    string
	Priority    string
}

// TaskFilter narrows task_list.
type TaskFilter struct {
	Assignee string
	Status   string
	Limit    int
}

// Task statuses task_update may move a task to, and the brain tool that does it. "reopen" sends a
// blocked task back to new and a task in review back to progress.
var statusTools = map[string]string{
	"progress": "task_start",
	"review":   "task_review",
	"done":     "task_done",
	"blocked":  "task_block",
	"reopen":   "task_reopen",
}

// TaskStatuses are the accepted values of TaskUpdate.Status.
var TaskStatuses = []string{"progress", "review", "done", "blocked", "reopen"}

// TaskUpdate changes a task: the non-empty fields are set first, then Status (if any) is applied as a
// transition with Note as its note (the reason for blocked).
type TaskUpdate struct {
	ID          int64
	Title       string
	Description string
	Assignee    string
	Priority    string
	Status      string
	Note        string
}

func (u TaskUpdate) hasFields() bool { return u.Title+u.Description+u.Assignee+u.Priority != "" }

// partialUpdate wraps a failed status change: when fields were set first, they stay set.
func partialUpdate(u TaskUpdate, err error) error {
	if !u.hasFields() {
		return err
	}
	return fmt.Errorf("task %d: fields updated, but the status change to %s failed: %w", u.ID, u.Status, err)
}

// Validate checks an update before it is sent.
func (u TaskUpdate) Validate() error {
	if u.ID <= 0 {
		return errors.New("task_id must be a positive integer")
	}
	if u.Status != "" && !slices.Contains(TaskStatuses, u.Status) {
		return fmt.Errorf("status %q must be one of %s", u.Status, strings.Join(TaskStatuses, ", "))
	}
	if !u.hasFields() && u.Status == "" {
		return errors.New("nothing to update: give a field or a status")
	}
	if u.Note != "" && u.Status == "" {
		return errors.New("note goes with a status change")
	}
	return nil
}

var (
	reAgentName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	reAuthor    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}(/job:[A-Za-z0-9][A-Za-z0-9-]{0,63})?$`)
)

// ValidateAgentName checks a recipient of send_message.
func ValidateAgentName(s string) error {
	if !reAgentName.MatchString(s) {
		return fmt.Errorf("agent %q is not an agent name", s)
	}
	return nil
}

// Brain is the shared memory and task service.
type Brain interface {
	// Search returns up to limit notes closest to query, best first.
	Search(ctx context.Context, query string, limit int) ([]Note, error)
	// Write creates a note and returns the brain's answer (path and metadata).
	Write(ctx context.Context, n NoteInput) (Object, error)
	CreateTask(ctx context.Context, t TaskInput) (Object, error)
	ListTasks(ctx context.Context, f TaskFilter) ([]Object, error)
	// UpdateTask applies u and returns the task as it is afterwards.
	UpdateTask(ctx context.Context, u TaskUpdate) (Object, error)
	// GetTask returns ErrNotFound for an unknown id.
	GetTask(ctx context.Context, id int64) (Object, error)
	// Send queues a message to another agent and returns the delivery.
	Send(ctx context.Context, to string, payload Object) (Object, error)
	// Inbox returns pending deliveries addressed to this agent.
	Inbox(ctx context.Context, limit int) ([]Object, error)
	// Ack marks a delivery as handled.
	Ack(ctx context.Context, deliveryID string) error
}

// Disabled is the brain of an agent without a brain block in its config: every call fails with
// ErrNotConfigured and nothing else changes.
type Disabled struct{}

var _ Brain = Disabled{}

func (Disabled) Search(context.Context, string, int) ([]Note, error)   { return nil, ErrNotConfigured }
func (Disabled) Write(context.Context, NoteInput) (Object, error)      { return nil, ErrNotConfigured }
func (Disabled) CreateTask(context.Context, TaskInput) (Object, error) { return nil, ErrNotConfigured }
func (Disabled) ListTasks(context.Context, TaskFilter) ([]Object, error) {
	return nil, ErrNotConfigured
}
func (Disabled) UpdateTask(context.Context, TaskUpdate) (Object, error) { return nil, ErrNotConfigured }
func (Disabled) GetTask(context.Context, int64) (Object, error)         { return nil, ErrNotConfigured }
func (Disabled) Send(context.Context, string, Object) (Object, error)   { return nil, ErrNotConfigured }
func (Disabled) Inbox(context.Context, int) ([]Object, error)           { return nil, ErrNotConfigured }
func (Disabled) Ack(context.Context, string) error                      { return ErrNotConfigured }

// IsDisabled reports whether b is the no-brain implementation (or nil).
func IsDisabled(b Brain) bool {
	if b == nil {
		return true
	}
	_, ok := b.(Disabled)
	return ok
}
