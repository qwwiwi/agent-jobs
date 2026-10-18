package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// A suppressed wake was a model call: it holds the decision interval, so clearing the blocker and
// advancing one daemon tick does not ask the model again (it still is no wake of the day).
func TestHeartbeatSuppressedHoldsInterval(t *testing.T) {
	e := newHeartbeat(t)
	m := e.m
	clock := &hbClock{t: hbNoon}
	m.Now = clock.Now
	e.answer(t, `{"wake": true, "reason": "x"}`)
	release := e.holdCalls(t)
	done := runAsync(m, HeartbeatTriggerDaemon)
	e.waitHeld(t)
	n, err := m.Store.AddNotification("", "x", "normal", hbNoon, store.NotifyLimits{})
	if err != nil {
		t.Fatal(err)
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	h, err := m.Store.LastHeartbeat()
	if err != nil || h == nil || h.Decision != store.HeartbeatSuppressed {
		t.Fatalf("row: %+v %v", h, err)
	}
	// The blocker clears and the next daemon tick comes.
	if err := m.Store.SetNotificationResult(n.ID, true, ""); err != nil {
		t.Fatal(err)
	}
	clock.Set(hbNoon.Add(TickInterval))
	if !m.StartHeartbeat(context.Background()) {
		t.Fatal("StartHeartbeat did not run")
	}
	m.WaitHeartbeat()
	if c := e.calls(); c != 1 {
		t.Fatalf("model calls %d, want 1", c)
	}
	if r, _, err := m.Store.HeartbeatGate(); err != nil || !strings.Contains(r, "interval: last decision") {
		t.Fatalf("gate: %q %v", r, err)
	}
	if n, _ := m.Store.CountHeartbeatWakes(localMidnight(hbNoon, time.UTC)); n != 0 {
		t.Fatalf("wakes today %d", n)
	}
	e.noLineTyped(t)
}

// Delivery eligibility is re-checked after tmux.lock is taken, right before typing: a wake that
// waited for the lock across the start of the night window is suppressed and nothing is typed.
func TestHeartbeatRecheckUnderTmuxLock(t *testing.T) {
	e := newHeartbeat(t)
	m := e.m
	s, en := 22, 7
	m.Cfg.Heartbeat.NightStartHour, m.Cfg.Heartbeat.NightEndHour = &s, &en
	start := time.Date(2026, 10, 18, 21, 59, 30, 0, time.UTC)
	clock := &hbClock{t: start}
	m.Now = clock.Now
	e.answer(t, `{"wake": true, "reason": "evening check"}`)
	f, err := os.OpenFile(filepath.Join(m.StateDir, "tmux.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	// The sync point: the delivery found tmux.lock held and waits for it, so the decision is in and
	// the re-check can only come after the lock is released.
	waiting := make(chan struct{})
	var once sync.Once
	tmuxLockWaiting = func() { once.Do(func() { close(waiting) }) }
	t.Cleanup(func() { tmuxLockWaiting = func() {} })
	done := runAsync(m, HeartbeatTriggerDaemon)
	select {
	case <-waiting:
	case <-time.After(10 * time.Second):
		t.Fatal("the delivery never waited for tmux.lock")
	}
	if c := e.calls(); c != 1 {
		t.Fatalf("model calls %d before the lock wait, want 1", c)
	}
	clock.Set(start.Add(time.Minute)) // 22:00:30, inside the night window
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	h, err := m.Store.LastHeartbeat()
	if err != nil || h == nil || h.Decision != store.HeartbeatSuppressed || h.Delivered ||
		!strings.Contains(h.Error, "night window 22:00-07:00") {
		t.Fatalf("row: %+v %v", h, err)
	}
	e.noLineTyped(t)
}

// A decision call that hangs never blocks the daemon's ticks: Tick and StartHeartbeat keep
// returning while it is held, and no second decision starts.
func TestHeartbeatHungCallDoesNotBlockTicks(t *testing.T) {
	e := newHeartbeat(t)
	m := e.m
	e.answer(t, `{"wake": false, "reason": "nothing to do"}`)
	release := e.holdCalls(t)
	defer m.WaitHeartbeat()
	defer release()
	if !m.StartHeartbeat(context.Background()) {
		t.Fatal("StartHeartbeat did not run")
	}
	e.waitHeld(t)
	for i := range 3 {
		tickDone := make(chan error, 1)
		go func() {
			err := m.Tick()
			if m.StartHeartbeat(context.Background()) {
				err = fmt.Errorf("a second heartbeat started while one is held")
			}
			tickDone <- err
		}()
		select {
		case err := <-tickDone:
			if err != nil {
				t.Fatalf("tick %d: %v", i, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("tick %d blocked by the held decision call", i)
		}
	}
	if p, err := m.Store.PendingHeartbeat(); err != nil || p == nil {
		t.Fatalf("pending: %+v %v", p, err)
	}
	release()
	m.WaitHeartbeat()
	if h, err := m.Store.LastHeartbeat(); err != nil || h.Decision != store.HeartbeatSleep || e.calls() != 1 {
		t.Fatalf("row: %+v %v, calls %d", h, err, e.calls())
	}
}

// A row an earlier daemon stored survives a reopen of the database and reads back with the model's
// reason only under untrusted_model_reason (heartbeat show --json, the view, the status).
func TestHeartbeatOldRowAfterReopen(t *testing.T) {
	e := newHeartbeat(t)
	m := e.m
	path := filepath.Join(m.StateDir, "jobs.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	res, err := db.Exec(`INSERT INTO heartbeats (claimed_at, finished_at, trigger, decision, reason, cost_usd, delivered)
		VALUES (?, ?, 'daemon', 'wake', 'OLD_REASON_MARKER', 0.01, 1)`, hbNoon.Add(-2*time.Hour).Unix(), hbNoon.Add(-2*time.Hour).Unix())
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.Close(); err != nil {
		t.Fatal(err)
	}
	if m.Store, err = store.Open(path); err != nil {
		t.Fatal(err)
	}
	onlyUntrusted := func(what string, b []byte) {
		t.Helper()
		if !strings.Contains(string(b), `"untrusted_model_reason":"OLD_REASON_MARKER"`) || strings.Contains(string(b), `"reason"`) {
			t.Fatalf("%s: %s", what, b)
		}
	}
	v, err := m.HeartbeatDecision(id)
	if err != nil || v.Decision != store.HeartbeatWake || !v.Delivered {
		t.Fatalf("view: %+v %v", v, err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	onlyUntrusted("view", b)
	st, err := m.Heartbeat()
	if err != nil || st.Last == nil || st.Last.ID != id || st.WakesToday != 1 {
		t.Fatalf("status: %+v %v", st, err)
	}
	if b, err = json.Marshal(st.Last); err != nil {
		t.Fatal(err)
	}
	onlyUntrusted("status", b)
	out, err := exec.Command(selfBin, "heartbeat", "show", fmt.Sprint(id), "--agent", "test", "--json").Output()
	if err != nil {
		t.Fatalf("show: %v\n%s", err, out)
	}
	onlyUntrusted("show --json", []byte(strings.Join(strings.Fields(string(out)), "")))
}

// A tmux.lock that stays held past TmuxDeliveryTimeout: nothing was re-checked or typed; the wake
// is stored, not delivered, with the lock error.
func TestHeartbeatTmuxLockTimeoutNotDelivered(t *testing.T) {
	e := newHeartbeat(t)
	m := e.m
	old := TmuxDeliveryTimeout
	TmuxDeliveryTimeout = 300 * time.Millisecond
	t.Cleanup(func() { TmuxDeliveryTimeout = old })
	f, err := os.OpenFile(filepath.Join(m.StateDir, "tmux.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	e.answer(t, `{"wake": true, "reason": "x"}`)
	h, err := m.RunHeartbeat(context.Background(), HeartbeatTriggerDaemon)
	if err != nil {
		t.Fatal(err)
	}
	if h.Decision != store.HeartbeatWake || h.Delivered || !strings.Contains(h.Error, "tmux.lock still held") {
		t.Fatalf("row: %+v", h)
	}
	e.noLineTyped(t)
}
