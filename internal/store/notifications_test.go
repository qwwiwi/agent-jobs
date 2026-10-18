package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

var testLimits = NotifyLimits{PerJob: 3, JobInterval: time.Minute, PerAgentHour: 20}

// A stage-1 database gains the owner_notifications table on Open; a row round-trips with its job
// (or none for the orchestrator), and the delivery result and the undelivered list hold.
func TestNotificationsRoundTripOnOldDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(stage1Schema); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	t0 := time.Unix(1770000000, 0)
	a, err := s.AddNotification("job-1", "need a decision", "high", t0, testLimits)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.AddNotification("", "from the session", "normal", t0, testLimits)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetNotificationResult(a.ID, false, "tmux: no session"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetNotificationResult(b.ID, true, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetNotificationResult(999, true, ""); err == nil {
		t.Fatal("missing notification updated")
	}
	got, err := s.GetNotification(a.ID)
	if err != nil || got.JobID != "job-1" || got.Text != "need a decision" || got.Urgency != "high" || got.Delivered ||
		got.Error != "tmux: no session" || !got.CreatedAt.Equal(t0) {
		t.Fatalf("job notification: %+v %v", got, err)
	}
	if got, err := s.GetNotification(b.ID); err != nil || got.JobID != "" || !got.Delivered {
		t.Fatalf("orchestrator notification: %+v %v", got, err)
	}
	var nulls int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM owner_notifications WHERE job_id IS NULL`).Scan(&nulls); err != nil || nulls != 1 {
		t.Fatalf("orchestrator job_id is not NULL: %d %v", nulls, err)
	}
	und, err := s.UndeliveredNotifications(t0.Add(-time.Hour))
	if err != nil || len(und) != 1 || und[0].ID != a.ID {
		t.Fatalf("undelivered: %+v %v", und, err)
	}
	if und, err := s.UndeliveredNotifications(t0.Add(time.Second)); err != nil || len(und) != 0 {
		t.Fatalf("undelivered since later: %+v %v", und, err)
	}
	_ = s.Close()
	if s, err = Open(path); err != nil { // a second Open keeps the rows
		t.Fatal(err)
	}
	if und, err := s.UndeliveredNotifications(t0.Add(-time.Hour)); err != nil || len(und) != 1 {
		t.Fatalf("after reopen: %+v %v", und, err)
	}
}

// Per job: one per minute and three in total; per agent: twenty within an hour, the orchestrator
// (no job) counted too. Every refusal wraps ErrNotifyLimit and stores nothing.
func TestNotificationLimits(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	t0 := time.Unix(1770000000, 0)
	if _, err := s.AddNotification("j", "1", "normal", t0, testLimits); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddNotification("j", "2", "normal", t0.Add(59*time.Second), testLimits); !errors.Is(err, ErrNotifyLimit) {
		t.Fatalf("second within a minute: %v", err)
	}
	if _, err := s.AddNotification("j", "2", "normal", t0.Add(time.Minute), testLimits); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddNotification("j", "3", "normal", t0.Add(2*time.Minute), testLimits); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddNotification("j", "4", "normal", t0.Add(time.Hour), testLimits); !errors.Is(err, ErrNotifyLimit) {
		t.Fatalf("fourth for a job: %v", err)
	}
	// Another job is not held by j's limits.
	if _, err := s.AddNotification("k", "1", "normal", t0.Add(2*time.Minute), testLimits); err != nil {
		t.Fatal(err)
	}
	// 4 so far within the hour; the orchestrator fills the agent's hour to 20.
	for i := 0; i < 16; i++ {
		if _, err := s.AddNotification("", "o", "normal", t0.Add(3*time.Minute), testLimits); err != nil {
			t.Fatalf("orchestrator %d: %v", i, err)
		}
	}
	if _, err := s.AddNotification("", "o", "normal", t0.Add(3*time.Minute), testLimits); !errors.Is(err, ErrNotifyLimit) {
		t.Fatalf("21st within an hour: %v", err)
	}
	if _, err := s.AddNotification("m", "1", "normal", t0.Add(4*time.Minute), testLimits); !errors.Is(err, ErrNotifyLimit) {
		t.Fatalf("a job past the agent's hour: %v", err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM owner_notifications`).Scan(&n); err != nil || n != 20 {
		t.Fatalf("rows: %d %v", n, err)
	}
	// An hour after the first ones the agent may send again.
	if _, err := s.AddNotification("", "o", "normal", t0.Add(61*time.Minute), testLimits); err != nil {
		t.Fatal(err)
	}
}

// Concurrent callers on two stores of one database never pass the per-job limit together.
func TestNotificationLimitIsAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s1.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	lim := NotifyLimits{PerJob: 3}
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	now := time.Unix(1770000000, 0)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(s *Store) {
			defer wg.Done()
			if _, err := s.AddNotification("j", "x", "normal", now, lim); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			} else if !errors.Is(err, ErrNotifyLimit) {
				t.Errorf("unexpected error: %v", err)
			}
		}([]*Store{s1, s2}[i%2])
	}
	wg.Wait()
	if ok != 3 {
		t.Fatalf("%d notifications passed a limit of 3", ok)
	}
}
