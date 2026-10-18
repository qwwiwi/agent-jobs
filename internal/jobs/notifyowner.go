package jobs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// notify_owner: a message for the owner through the agent's existing channel (DEVIATIONS #4).
// agent-jobs never talks to Telegram itself: the notification is one tagged line typed into the
// orchestrator session (the same tmux path as the job-done line), and that session relays it.

// MaxNotifyRunes caps a notification's text.
const MaxNotifyRunes = 300

// NotifyTag starts every notify_owner line in the session.
const NotifyTag = "[jobs:notify-owner]"

// Urgencies of a notification (DefaultUrgency when none is given).
var Urgencies = []string{"normal", "high"}

// DefaultUrgency is the urgency of a notification sent without one.
const DefaultUrgency = "normal"

// NotifyLimits bound notify_owner: per job at most 3 and at most one a minute, per agent at most 20
// within an hour (the orchestrator's own notifications count there too).
var NotifyLimits = store.NotifyLimits{PerJob: 3, JobInterval: time.Minute, PerAgentHour: 20}

// UndeliveredWindow is how far back status and system_doctor look for undelivered notifications.
const UndeliveredWindow = 24 * time.Hour

// notifyText validates a notification's text: required, valid UTF-8, one line (no control or
// format character, bidi overrides included), then Printable capped to MaxNotifyRunes and defanged
// so it cannot pass for another tag (defangTags).
func notifyText(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", errors.New("text is not valid UTF-8")
	}
	if containsControl(s) {
		return "", errors.New("text contains a control or format character (newline, tab, escape, bidi, …); a notification is one line")
	}
	t := defangTags(Printable(s, MaxNotifyRunes))
	if t == "" {
		return "", errors.New("text is required")
	}
	return t, nil
}

// defangTags turns every "[jobs:" (ASCII letters in any case) into "(jobs:" and a leading "[" into
// "(", so the text of a notification never reads as a tag of its own, wherever the session shows it.
func defangTags(s string) string {
	b := []byte(s)
	for i := 0; i+len("[jobs:") <= len(b); i++ {
		if b[i] == '[' && strings.EqualFold(string(b[i+1:i+len("[jobs:")]), "jobs:") && isASCII(b[i+1:i+len("[jobs:")]) {
			b[i] = '('
		}
	}
	if len(b) > 0 && b[0] == '[' {
		b[0] = '('
	}
	return string(b)
}

func isASCII(b []byte) bool {
	for _, c := range b {
		if c >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// NotifyLine is the line typed into the session for a notification.
func NotifyLine(n *store.Notification) string {
	line := NotifyTag
	if n.Urgency == "high" {
		line += " urgent ·"
	}
	if n.JobID != "" {
		line += " job " + Printable(n.JobID, 64) + ":"
	}
	return line + " " + n.Text
}

// NotifyOwner records a notification for the owner (jobID "" = the orchestrator itself) within
// NotifyLimits and types its line into the notify target. A delivery that fails (no target
// configured, tmux refused) keeps the row with delivered=false and its error, and is returned as an
// error naming the row; status and system_doctor list such rows.
func (m *Manager) NotifyOwner(jobID, text, urgency string) (*store.Notification, error) {
	t, err := notifyText(text)
	if err != nil {
		return nil, err
	}
	if urgency == "" {
		urgency = DefaultUrgency
	}
	if !slices.Contains(Urgencies, urgency) {
		return nil, fmt.Errorf("urgency %q must be one of %s", Printable(urgency, 40), strings.Join(Urgencies, ", "))
	}
	n, err := m.Store.AddNotification(jobID, t, urgency, m.Now(), NotifyLimits)
	if err != nil {
		return nil, err
	}
	var derr error
	if target := m.Cfg.NotifyTmuxTarget; target == "" {
		derr = errors.New("notify off: notify_tmux_target is not configured")
	} else {
		derr = m.sendTmuxLocked(target, NotifyLine(n))
	}
	if derr != nil {
		n.Error = derr.Error()
		m.Log.Printf("notify_owner %d (job %q): not delivered: %v", n.ID, jobID, derr)
	} else {
		n.Delivered = true
	}
	if err := m.Store.SetNotificationResult(n.ID, n.Delivered, n.Error); err != nil {
		return n, err
	}
	if derr != nil {
		return n, fmt.Errorf("notification %d stored but not delivered to the session: %w", n.ID, derr)
	}
	return n, nil
}

// UndeliveredNotifications are the notifications of the last UndeliveredWindow that did not reach
// the session.
func (m *Manager) UndeliveredNotifications() ([]*store.Notification, error) {
	return m.Store.UndeliveredNotifications(m.Now().Add(-UndeliveredWindow))
}

// TmuxDeliveryTimeout bounds one delivery (waiting for tmux.lock and every tmux call); a variable
// so tests can shorten it.
var TmuxDeliveryTimeout = 10 * time.Second

// errTmuxTimeout is a delivery that did not finish within TmuxDeliveryTimeout.
var errTmuxTimeout = errors.New("tmux delivery timed out")

// sendTmuxLocked is sendTmux under <state>/tmux.lock (flock): the daemon's job-done lines and
// notifications from the proxy or the orchestrator's process never interleave their keystrokes.
// The whole delivery is bounded by TmuxDeliveryTimeout: the lock is polled (LOCK_NB) until then and
// every tmux call is killed at that deadline, so a hung tmux or lock holder never blocks the caller
// (the daemon's tick, a proxy call) for longer.
func (m *Manager) sendTmuxLocked(target, line string) error {
	return m.withTmuxLock(func(ctx context.Context) error { return m.sendTmux(ctx, target, line) })
}

// withTmuxLock runs fn holding <state>/tmux.lock, with a context that ends TmuxDeliveryTimeout
// after the call started (waiting for the lock included). An error taking the lock is returned
// without running fn.
func (m *Manager) withTmuxLock(fn func(ctx context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), TmuxDeliveryTimeout)
	defer cancel()
	f, err := os.OpenFile(filepath.Join(m.StateDir, "tmux.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open tmux lock: %w", err)
	}
	defer f.Close()
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			return fmt.Errorf("tmux lock: %w", err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: tmux.lock still held by another delivery after %s", errTmuxTimeout, TmuxDeliveryTimeout)
		case <-time.After(20 * time.Millisecond):
		}
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	return fn(ctx)
}
