package jobs

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// MaxIntentionRunes caps an intention's text (longer text is cut, Printable-style).
const MaxIntentionRunes = 500

// IntentionStatuses and IntentionPriorities are the accepted values (DefaultIntentionPriority when none).
var (
	IntentionStatuses   = []string{store.IntentionOpen, store.IntentionDone, store.IntentionDropped}
	IntentionPriorities = []string{"low", "normal", "high"}
)

// DefaultIntentionPriority is the priority of an intention added without one.
const DefaultIntentionPriority = "normal"

// intentionText validates an intention's text: required, valid UTF-8, no control or format
// character (a goal is one line), then Printable capped to MaxIntentionRunes.
func intentionText(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", errors.New("text is not valid UTF-8")
	}
	if containsControl(s) {
		return "", errors.New("text contains a control character (newline, tab, escape, …); an intention is one line")
	}
	t := Printable(s, MaxIntentionRunes)
	if t == "" {
		return "", errors.New("text is required")
	}
	return t, nil
}

func checkIntentionStatus(s string) error {
	if !slices.Contains(IntentionStatuses, s) {
		return fmt.Errorf("status %q must be one of %s", Printable(s, 40), strings.Join(IntentionStatuses, ", "))
	}
	return nil
}

// AddIntention adds one of the agent's own goals, open. due is "" or a date YYYY-MM-DD; priority
// is "" (normal) or one of IntentionPriorities.
func (m *Manager) AddIntention(text, due, priority string) (*store.Intention, error) {
	t, err := intentionText(text)
	if err != nil {
		return nil, err
	}
	if due != "" {
		if _, err := time.Parse(time.DateOnly, due); err != nil {
			return nil, fmt.Errorf("due %q must be a date YYYY-MM-DD", Printable(due, 40))
		}
	}
	if priority == "" {
		priority = DefaultIntentionPriority
	}
	if !slices.Contains(IntentionPriorities, priority) {
		return nil, fmt.Errorf("priority %q must be one of %s", Printable(priority, 40), strings.Join(IntentionPriorities, ", "))
	}
	return m.Store.AddIntention(t, due, priority, m.Now())
}

// ListIntentions returns the agent's intentions, oldest first; status "" means all of them.
func (m *Manager) ListIntentions(status string) ([]*store.Intention, error) {
	if status != "" {
		if err := checkIntentionStatus(status); err != nil {
			return nil, err
		}
	}
	return m.Store.ListIntentions(status)
}

// SetIntention edits an intention in place: its status, its text or both (at least one). There is
// no delete: a goal given up is dropped.
func (m *Manager) SetIntention(id int64, text, status string) (*store.Intention, error) {
	if id <= 0 {
		return nil, errors.New("id is required")
	}
	if text == "" && status == "" {
		return nil, errors.New("give a status, a text or both")
	}
	if text != "" {
		t, err := intentionText(text)
		if err != nil {
			return nil, err
		}
		text = t
	}
	if status != "" {
		if err := checkIntentionStatus(status); err != nil {
			return nil, err
		}
	}
	it, err := m.Store.SetIntention(id, text, status, m.Now())
	if errors.Is(err, store.ErrIntentionNotFound) {
		return nil, fmt.Errorf("intention %d not found", id)
	}
	return it, err
}
