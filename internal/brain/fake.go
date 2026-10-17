package brain

import (
	"context"
	"fmt"
	"slices"
	"sync"
)

// Fake is an in-memory brain for tests. Search returns Notes (best first, as given); Err makes every
// call fail; Block makes every call wait until its context ends (an unreachable brain).
type Fake struct {
	mu      sync.Mutex
	Notes   []Note
	Err     error
	Block   bool
	Queries []string
	Written []NoteInput
	tasks   []Object
	Sent    []Object
	Pending []Object
	Acked   []string
	// WriteAnswer, when set, is Write's answer instead of the real brain's {"result": "created: <path>"}.
	WriteAnswer Object
}

var _ Brain = (*Fake)(nil)

func (f *Fake) enter(ctx context.Context) error {
	f.mu.Lock()
	block, err := f.Block, f.Err
	f.mu.Unlock()
	if block {
		<-ctx.Done()
		return ctx.Err()
	}
	return err
}

// Search records the query and returns up to limit notes.
func (f *Fake) Search(ctx context.Context, query string, limit int) ([]Note, error) {
	if err := f.enter(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Queries = append(f.Queries, query)
	n := slices.Clone(f.Notes)
	if len(n) > limit {
		n = n[:limit]
	}
	if n == nil {
		n = []Note{}
	}
	return n, nil
}

// Write records the note and answers as the real brain does: {"result": "created: <path>"}.
func (f *Fake) Write(ctx context.Context, n NoteInput) (Object, error) {
	if err := f.enter(ctx); err != nil {
		return nil, err
	}
	if err := n.Validate(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Written = append(f.Written, n)
	if f.WriteAnswer != nil {
		return cloneObj(f.WriteAnswer), nil
	}
	return Object{"result": fmt.Sprintf("created: fake/%s/%d.md", n.Kind, len(f.Written))}, nil
}

// CreateTask adds a task with status new.
func (f *Fake) CreateTask(ctx context.Context, t TaskInput) (Object, error) {
	if err := f.enter(ctx); err != nil {
		return nil, err
	}
	if t.Title == "" {
		return nil, fmt.Errorf("title is required")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	pr := t.Priority
	if pr == "" {
		pr = "medium"
	}
	o := Object{"id": float64(len(f.tasks) + 1), "title": t.Title, "description": t.Description,
		"assignee": t.Assignee, "priority": pr, "status": "new"}
	f.tasks = append(f.tasks, o)
	return cloneObj(o), nil
}

func cloneObj(o Object) Object {
	c := Object{}
	for k, v := range o {
		c[k] = v
	}
	return c
}

// ListTasks filters by assignee and status.
func (f *Fake) ListTasks(ctx context.Context, fl TaskFilter) ([]Object, error) {
	if err := f.enter(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []Object{}
	for _, t := range f.tasks {
		if (fl.Assignee == "" || t["assignee"] == fl.Assignee) && (fl.Status == "" || t["status"] == fl.Status) && len(out) < fl.Limit {
			out = append(out, cloneObj(t))
		}
	}
	return out, nil
}

// fakeTransitions are the brain's allowed status moves: target status by (tool status, from).
var fakeTransitions = map[string]map[string]string{
	"progress": {"new": "progress", "blocked": "progress"},
	"review":   {"progress": "review"},
	"done":     {"review": "done"},
	"blocked":  {"new": "blocked", "progress": "blocked"},
	"reopen":   {"blocked": "new", "review": "progress"},
}

// UpdateTask sets fields and applies a transition with the real brain's rules.
func (f *Fake) UpdateTask(ctx context.Context, u TaskUpdate) (Object, error) {
	if err := f.enter(ctx); err != nil {
		return nil, err
	}
	if err := u.Validate(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.task(u.ID)
	if t == nil {
		return nil, fmt.Errorf("task %d: %w", u.ID, ErrNotFound)
	}
	// Fields first, then the transition, as the real client does (two calls, not atomic).
	for k, v := range map[string]string{"title": u.Title, "description": u.Description, "assignee": u.Assignee, "priority": u.Priority} {
		if v != "" {
			t[k] = v
		}
	}
	if u.Status != "" {
		next, ok := fakeTransitions[u.Status][fmt.Sprint(t["status"])]
		if !ok {
			return nil, partialUpdate(u, fmt.Errorf("brain %s: invalid transition from %v", statusTools[u.Status], t["status"]))
		}
		t["status"] = next
	}
	return cloneObj(t), nil
}

func (f *Fake) task(id int64) Object {
	if id < 1 || int(id) > len(f.tasks) {
		return nil
	}
	return f.tasks[id-1]
}

// GetTask returns a task or ErrNotFound.
func (f *Fake) GetTask(ctx context.Context, id int64) (Object, error) {
	if err := f.enter(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.task(id)
	if t == nil {
		return nil, fmt.Errorf("task %d: %w", id, ErrNotFound)
	}
	return cloneObj(t), nil
}

// Send records the delivery.
func (f *Fake) Send(ctx context.Context, to string, payload Object) (Object, error) {
	if err := f.enter(ctx); err != nil {
		return nil, err
	}
	if err := ValidateAgentName(to); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	d := Object{"task_id": fmt.Sprintf("d%d", len(f.Sent)+1), "to_agent": to, "payload": payload, "status": "pending"}
	f.Sent = append(f.Sent, d)
	return cloneObj(d), nil
}

// Inbox returns the pending deliveries not yet acked.
func (f *Fake) Inbox(ctx context.Context, limit int) ([]Object, error) {
	if err := f.enter(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []Object{}
	for _, d := range f.Pending {
		if !slices.Contains(f.Acked, fmt.Sprint(d["task_id"])) && len(out) < limit {
			out = append(out, cloneObj(d))
		}
	}
	return out, nil
}

// Ack records the delivery id; an unknown id is an error.
func (f *Fake) Ack(ctx context.Context, id string) error {
	if err := f.enter(ctx); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range f.Pending {
		if fmt.Sprint(d["task_id"]) == id {
			f.Acked = append(f.Acked, id)
			return nil
		}
	}
	return fmt.Errorf("delivery %q: %w", id, ErrNotFound)
}
