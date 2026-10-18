package store

import (
	"database/sql"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func insertJob(t *testing.T, st *Store, id string, ownerWaits bool, created time.Time) {
	t.Helper()
	if err := st.Insert(&Job{ID: id, Agent: "agent1", Title: "title " + id, Kind: "fix", Brief: "b",
		Status: StatusSucceeded, Model: "opus", MaxTurns: 10, MaxBudgetUSD: 1, TimeoutMinutes: 5, CreatedAt: created,
		OwnerWaits: ownerWaits}); err != nil {
		t.Fatal(err)
	}
}

func pendingIDs(t *testing.T, st *Store, now time.Time, maxAge time.Duration) ([]string, int) {
	t.Helper()
	list, expired, err := st.OwnerPendingList(now, maxAge)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, p := range list {
		ids = append(ids, p.JobID)
	}
	return ids, expired
}

// A delivered job-done line records «owner reply pending» only for an owner-waits job; a later line
// renews it; owner_replied clears one or all; records older than the max age expire.
func TestOwnerPendingLifecycle(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	t0 := time.Unix(1760000000, 0)
	insertJob(t, st, "plain", false, t0)
	insertJob(t, st, "w1", true, t0)
	insertJob(t, st, "w2", true, t0)
	for _, id := range []string{"plain", "w1"} {
		if err := st.MarkNotified(id, t0); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.MarkNotified("w2", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if j, _ := st.Get("plain"); j.NotifiedAt == nil {
		t.Fatal("notified_at not set")
	}
	list, _, err := st.OwnerPendingList(t0, 6*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].JobID != "w1" || list[0].Title != "title w1" || list[0].Status != StatusSucceeded ||
		!list[0].CreatedAt.Equal(t0) || list[1].JobID != "w2" {
		t.Fatalf("pending: %+v", list)
	}

	// w1 expires (older than 6 h at t0+6h30m), w2 does not.
	ids, expired := pendingIDs(t, st, t0.Add(6*time.Hour+30*time.Minute), 6*time.Hour)
	if !slices.Equal(ids, []string{"w2"}) || expired != 1 {
		t.Fatalf("after expiry: %v, expired %d", ids, expired)
	}
	// A renewed line (a resumed attempt) records it again with the new time.
	if err := st.MarkNotified("w1", t0.Add(7*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if ids, _ := pendingIDs(t, st, t0.Add(7*time.Hour), 6*time.Hour); !slices.Equal(ids, []string{"w2", "w1"}) {
		t.Fatalf("renewed: %v", ids)
	}

	cleared, err := st.ClearOwnerPending("w2", t0)
	if err != nil || !slices.Equal(cleared, []string{"w2"}) {
		t.Fatalf("clear w2: %v %v", cleared, err)
	}
	if cleared, err := st.ClearOwnerPending("w2", t0); err != nil || len(cleared) != 0 {
		t.Fatalf("clear w2 again: %v %v", cleared, err)
	}
	insertJob(t, st, "w3", true, t0)
	if err := st.MarkNotified("w3", t0.Add(7*time.Hour)); err != nil {
		t.Fatal(err)
	}
	cleared, err = st.ClearOwnerPending("", t0)
	slices.Sort(cleared)
	if err != nil || !slices.Equal(cleared, []string{"w1", "w3"}) {
		t.Fatalf("clear all: %v %v", cleared, err)
	}
	if ids, _ := pendingIDs(t, st, t0.Add(7*time.Hour), 6*time.Hour); len(ids) != 0 {
		t.Fatalf("after clear all: %v", ids)
	}
}

// A database from before owner waits (no owner_waits column, no owner_pending table) opens, keeps
// its rows (owner_waits false) and gains the table; reopening is a no-op.
func TestOpenMigratesOwnerPending(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(stage1Schema); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	for range 2 {
		st, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		j, err := st.Get("old-1")
		if err != nil || j.OwnerWaits {
			t.Fatalf("old row: %+v %v", j, err)
		}
		if err := st.MarkNotified("old-1", time.Unix(1760000100, 0)); err != nil {
			t.Fatal(err)
		}
		if ids, _ := pendingIDs(t, st, time.Unix(1760000100, 0), time.Hour); len(ids) != 0 {
			t.Fatalf("an old job is never pending: %v", ids)
		}
		_ = st.Close()
	}
}

// The record is written before the line goes out and survives a concurrent owner_replied: a reply
// between BeginOwnerPending and MarkNotified stays, a retried delivery of the same attempt does not
// reopen it, a new attempt (resume) does. A reply naming a finished job whose line has not begun
// closes it ahead; an unfinished or already notified job has nothing to clear.
func TestOwnerPendingBeginReplyMark(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	t0 := time.Unix(1760000000, 0)
	insertJob(t, st, "w", true, t0)
	insertJob(t, st, "plain", false, t0)
	open := func() []string {
		ids, _ := pendingIDs(t, st, t0, 6*time.Hour)
		return ids
	}
	if err := st.BeginOwnerPending("plain", OwnerDelivering, t0); err != nil || len(open()) != 0 {
		t.Fatalf("a plain job records nothing: %v %v", open(), err)
	}
	if err := st.BeginOwnerPending("w", "bogus", t0); err == nil {
		t.Fatal("a bad state must be refused")
	}
	if err := st.BeginOwnerPending("w", OwnerDelivering, t0); err != nil {
		t.Fatal(err)
	}
	list, _, _ := st.OwnerPendingList(t0, 6*time.Hour)
	if len(list) != 1 || list[0].State != OwnerDelivering {
		t.Fatalf("delivering: %+v", list)
	}
	if cleared, err := st.ClearOwnerPending("w", t0); err != nil || !slices.Equal(cleared, []string{"w"}) {
		t.Fatalf("clear while delivering: %v %v", cleared, err)
	}
	if err := st.MarkNotified("w", t0); err != nil || len(open()) != 0 {
		t.Fatalf("the mark brought the cleared record back: %v %v", open(), err)
	}
	if err := st.BeginOwnerPending("w", OwnerDelivering, t0); err != nil || len(open()) != 0 {
		t.Fatalf("a retry of the same attempt reopened it: %v %v", open(), err)
	}

	// A resume: new attempt, notified_at reset; its line records again and is delivered.
	if _, err := st.db.Exec(`UPDATE jobs SET attempt = attempt + 1, notified_at = NULL WHERE id = 'w'`); err != nil {
		t.Fatal(err)
	}
	if err := st.BeginOwnerPending("w", OwnerDelivering, t0); err != nil || !slices.Equal(open(), []string{"w"}) {
		t.Fatalf("a new attempt must reopen: %v %v", open(), err)
	}
	if err := st.MarkNotified("w", t0); err != nil {
		t.Fatal(err)
	}
	if list, _, _ := st.OwnerPendingList(t0, 6*time.Hour); len(list) != 1 || list[0].State != OwnerDelivered {
		t.Fatalf("delivered: %+v", list)
	}
	if _, err := st.ClearOwnerPending("", t0); err != nil {
		t.Fatal(err)
	}

	// Closing ahead: finished, not notified yet.
	if _, err := st.db.Exec(`UPDATE jobs SET attempt = attempt + 1, notified_at = NULL WHERE id = 'w'`); err != nil {
		t.Fatal(err)
	}
	if cleared, err := st.ClearOwnerPending("w", t0); err != nil || !slices.Equal(cleared, []string{"w"}) {
		t.Fatalf("clear ahead: %v %v", cleared, err)
	}
	if cleared, err := st.ClearOwnerPending("w", t0); err != nil || len(cleared) != 0 {
		t.Fatalf("clear ahead twice: %v %v", cleared, err)
	}
	if err := st.BeginOwnerPending("w", OwnerDelivering, t0); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkNotified("w", t0); err != nil || len(open()) != 0 {
		t.Fatalf("a line after the reply holds again: %v %v", open(), err)
	}
	insertJob(t, st, "q", true, t0)
	if _, err := st.db.Exec(`UPDATE jobs SET status = 'running' WHERE id = 'q'`); err != nil {
		t.Fatal(err)
	}
	if cleared, err := st.ClearOwnerPending("q", t0); err != nil || len(cleared) != 0 {
		t.Fatalf("a running job has nothing to clear: %v %v", cleared, err)
	}

	// Notify off: recorded as such, the mark keeps the state.
	insertJob(t, st, "off", true, t0)
	if err := st.BeginOwnerPending("off", OwnerNotifyOff, t0); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkNotified("off", t0); err != nil {
		t.Fatal(err)
	}
	if list, _, _ := st.OwnerPendingList(t0, 6*time.Hour); len(list) != 1 || list[0].State != OwnerNotifyOff {
		t.Fatalf("notify off: %+v", list)
	}
}

// The hook session: none until bound, the last bind wins, an empty id is refused.
func TestHookSession(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if id, _, err := st.HookSession(); err != nil || id != "" {
		t.Fatalf("unbound: %q %v", id, err)
	}
	t0 := time.Unix(1760000000, 0)
	for _, s := range []string{"s1", "s2"} {
		if err := st.BindHookSession(s, t0); err != nil {
			t.Fatal(err)
		}
	}
	if id, at, err := st.HookSession(); err != nil || id != "s2" || !at.Equal(t0) {
		t.Fatalf("bound: %q %v %v", id, at, err)
	}
	if err := st.BindHookSession("", t0); err == nil {
		t.Fatal("an empty session id must be refused")
	}
}

// An owner_pending table of the first owner-waits build (job_id, created_at) gains the new columns;
// its rows read as delivered and open.
func TestOpenMigratesOldOwnerPendingTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	insertJob(t, st, "w", true, time.Unix(1760000000, 0))
	if _, err := st.db.Exec(`DROP TABLE owner_pending`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`CREATE TABLE owner_pending (job_id TEXT PRIMARY KEY, created_at INTEGER NOT NULL);
		INSERT INTO owner_pending VALUES ('w', 1760000000)`); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	if st, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	list, _, err := st.OwnerPendingList(time.Unix(1760000000, 0), time.Hour)
	if err != nil || len(list) != 1 || list[0].State != OwnerDelivered {
		t.Fatalf("migrated: %+v %v", list, err)
	}
}

func ownerRows(t *testing.T, st *Store, id string) map[int]string {
	t.Helper()
	rows, err := st.db.Query(`SELECT attempt, state FROM owner_pending WHERE job_id = ?`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[int]string{}
	for rows.Next() {
		var a int
		var s string
		if err := rows.Scan(&a, &s); err != nil {
			t.Fatal(err)
		}
		out[a] = s
	}
	return out
}

// A line whose delivery keeps failing expires once and stays expired: later retries of the same
// attempt reuse the row and never block again. An acknowledged attempt stays acknowledged across
// expiry and retries.
func TestOwnerPendingExpiryIsFinal(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	t0 := time.Unix(1760000000, 0)
	const maxAge = 6 * time.Hour
	insertJob(t, st, "w", true, t0)
	if err := st.BeginOwnerPending("w", OwnerDelivering, t0); err != nil {
		t.Fatal(err)
	}
	if ids, _ := pendingIDs(t, st, t0.Add(time.Hour), maxAge); !slices.Equal(ids, []string{"w"}) {
		t.Fatalf("delivering: %v", ids)
	}
	if ids, expired := pendingIDs(t, st, t0.Add(7*time.Hour), maxAge); len(ids) != 0 || expired != 1 {
		t.Fatalf("after max age: %v, expired %d", ids, expired)
	}
	for i := 1; i <= 3; i++ {
		at := t0.Add(time.Duration(7*i) * time.Hour)
		if err := st.BeginOwnerPending("w", OwnerDelivering, at); err != nil {
			t.Fatal(err)
		}
		if ids, expired := pendingIDs(t, st, at, maxAge); len(ids) != 0 || expired != 0 {
			t.Fatalf("retry %d after expiry blocks again: %v, expired %d", i, ids, expired)
		}
	}
	if rows := ownerRows(t, st, "w"); len(rows) != 1 || rows[0] != "expired" {
		t.Fatalf("one expired row expected: %v", rows)
	}
	// A late successful delivery does not reopen it either.
	if err := st.MarkNotified("w", t0.Add(30*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if ids, _ := pendingIDs(t, st, t0.Add(30*time.Hour), maxAge); len(ids) != 0 {
		t.Fatalf("delivered after expiry: %v", ids)
	}

	insertJob(t, st, "a", true, t0)
	if err := st.BeginOwnerPending("a", OwnerDelivering, t0); err != nil {
		t.Fatal(err)
	}
	if cleared, err := st.ClearOwnerPending("a", t0); err != nil || !slices.Equal(cleared, []string{"a"}) {
		t.Fatalf("ack: %v %v", cleared, err)
	}
	_, _ = pendingIDs(t, st, t0.Add(7*time.Hour), maxAge)
	if err := st.BeginOwnerPending("a", OwnerDelivering, t0.Add(7*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if ids, _ := pendingIDs(t, st, t0.Add(7*time.Hour), maxAge); len(ids) != 0 {
		t.Fatalf("acknowledged then retried past max age blocks: %v", ids)
	}
	if rows := ownerRows(t, st, "a"); rows[0] != "acknowledged" {
		t.Fatalf("ack row lost: %v", rows)
	}
}

// Rows are pruned only once the job can no longer be retried (finished and notified) and they are
// older than the max age; a row whose line is still pending survives, so nothing recreates it.
func TestOwnerPendingPrune(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	t0 := time.Unix(1760000000, 0)
	const maxAge = 6 * time.Hour
	insertJob(t, st, "done", true, t0)
	insertJob(t, st, "retrying", true, t0)
	if err := st.BeginOwnerPending("done", OwnerDelivering, t0); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkNotified("done", t0); err != nil {
		t.Fatal(err)
	}
	if err := st.BeginOwnerPending("retrying", OwnerDelivering, t0); err != nil {
		t.Fatal(err)
	}
	if _, _ = pendingIDs(t, st, t0.Add(time.Hour), maxAge); len(ownerRows(t, st, "done")) != 1 {
		t.Fatal("pruned within max age")
	}
	_, _ = pendingIDs(t, st, t0.Add(7*time.Hour), maxAge)
	if rows := ownerRows(t, st, "done"); len(rows) != 0 {
		t.Fatalf("finished and notified, past max age: kept %v", rows)
	}
	if rows := ownerRows(t, st, "retrying"); len(rows) != 1 || rows[0] != "expired" {
		t.Fatalf("a retried line's row must stay (expired): %v", rows)
	}
}

// owner_replied(job) acknowledges every attempt of the job and pre-acknowledges its latest finished
// attempt in one go. Exact sequence: attempt 1 pending, resume, attempt 2 finishes, owner_replied
// before attempt 2's line begins -> attempt 2's line does not block.
func TestOwnerRepliedAcksAllAttempts(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	t0 := time.Unix(1760000000, 0)
	insertJob(t, st, "w", true, t0)
	if _, err := st.db.Exec(`UPDATE jobs SET attempt = 1 WHERE id = 'w'`); err != nil {
		t.Fatal(err)
	}
	if err := st.BeginOwnerPending("w", OwnerDelivering, t0); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkNotified("w", t0); err != nil {
		t.Fatal(err)
	}
	// Resume: attempt 2 runs, then finishes.
	if _, err := st.db.Exec(`UPDATE jobs SET attempt = 2, notified_at = NULL, status = 'running' WHERE id = 'w'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE jobs SET status = 'succeeded' WHERE id = 'w'`); err != nil {
		t.Fatal(err)
	}
	if cleared, err := st.ClearOwnerPending("w", t0); err != nil || !slices.Equal(cleared, []string{"w"}) {
		t.Fatalf("owner_replied: %v %v", cleared, err)
	}
	if err := st.BeginOwnerPending("w", OwnerDelivering, t0); err != nil {
		t.Fatal(err)
	}
	if ids, _ := pendingIDs(t, st, t0, 6*time.Hour); len(ids) != 0 {
		t.Fatalf("attempt 2 blocks after owner_replied: %v", ids)
	}
	if err := st.MarkNotified("w", t0); err != nil {
		t.Fatal(err)
	}
	if ids, _ := pendingIDs(t, st, t0, 6*time.Hour); len(ids) != 0 {
		t.Fatalf("attempt 2 delivered blocks after owner_replied: %v", ids)
	}
	if rows := ownerRows(t, st, "w"); rows[1] != "acknowledged" || rows[2] != "acknowledged" {
		t.Fatalf("rows: %v", rows)
	}
}

// An owner_pending table keyed by job_id alone (the earlier owner-waits builds) is rebuilt into the
// (job, attempt) table on Open: a replied row becomes acknowledged, an open one keeps its state.
func TestOpenRebuildsOwnerPendingPerAttempt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	insertJob(t, st, "open", true, time.Unix(1760000000, 0))
	insertJob(t, st, "replied", true, time.Unix(1760000000, 0))
	if _, err := st.db.Exec(`DROP TABLE owner_pending;
		CREATE TABLE owner_pending (job_id TEXT PRIMARY KEY, created_at INTEGER NOT NULL,
			attempt INTEGER NOT NULL DEFAULT 0, state TEXT NOT NULL DEFAULT 'delivered', replied INTEGER NOT NULL DEFAULT 0);
		INSERT INTO owner_pending VALUES ('open', 1760000000, 0, 'delivering', 0), ('replied', 1760000000, 0, 'delivered', 1)`); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	for range 2 {
		if st, err = Open(path); err != nil {
			t.Fatal(err)
		}
		if rows := ownerRows(t, st, "open"); len(rows) != 1 || rows[0] != OwnerDelivering {
			t.Fatalf("open: %v", rows)
		}
		if rows := ownerRows(t, st, "replied"); len(rows) != 1 || rows[0] != OwnerAcknowledged {
			t.Fatalf("replied: %v", rows)
		}
		_ = st.Close()
	}
}
