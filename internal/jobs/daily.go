package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Daily automation (reference, «Как мы убираем»): «Раз в сутки jobs prune чистит папки джоб…» and
// «Раз в сутки проверка призраков (doctor procs)… По умолчанию только сообщает». The daemon runs both
// on its own, inside its tick loop (no extra timer): once per local day, at the first tick after
// daily_hour; prune for real (with the log archive), doctor procs in report mode only (killing stays
// manual, `doctor procs --kill`). The claimed day is stored, so a daemon restart does not repeat it
// and a missed day runs once at the next tick after the hour, never twice and never caught up.

// DailyStepTimeout bounds each step (prune, doctor procs) of the daily run.
var DailyStepTimeout = 10 * time.Minute

// DailyResult is the summary of one daily run, stored as JSON in the daily row.
type DailyResult struct {
	JobsPruned     int    `json:"jobs_pruned"`
	BytesFreed     int64  `json:"bytes_freed"`
	JobsArchived   int    `json:"jobs_archived"`
	ArchivedBytes  int64  `json:"archived_bytes"`
	ArchiveExpired int    `json:"archive_expired"`
	PruneError     string `json:"prune_error,omitempty"`
	Ghosts         int    `json:"ghosts"`
	JobGhosts      int    `json:"job_ghosts"`
	Leaders        int    `json:"leaders"`
	Suspects       int    `json:"suspects"`
	DeadRows       int    `json:"dead_rows"`
	ProcsError     string `json:"procs_error,omitempty"`
	ProcsNote      string `json:"procs_note,omitempty"`
	// Error is a failure of the run itself (a recovered panic).
	Error string `json:"error,omitempty"`
}

// DailyStatus is the last daily run as shown by `agent-jobs status` and system_doctor.
type DailyStatus struct {
	Enabled bool `json:"enabled"`
	Hour    int  `json:"hour"` // local hour after which the run is due
	// LastDay is the local day of the last run ("" = never ran).
	LastDay    string       `json:"last_day,omitempty"`
	StartedAt  *time.Time   `json:"started_at,omitempty"`
	FinishedAt *time.Time   `json:"finished_at,omitempty"`
	Running    bool         `json:"running"` // claimed but not finished (or the daemon died during it)
	Result     *DailyResult `json:"result,omitempty"`
}

// Daily returns the last daily run and the current settings.
func (m *Manager) Daily() (DailyStatus, error) {
	st := DailyStatus{Enabled: !m.Config().DailyOff, Hour: m.Config().DailyAt()}
	d, err := m.Store.GetDaily()
	if err != nil || d == nil {
		return st, err
	}
	started := d.StartedAt
	st.LastDay, st.StartedAt, st.FinishedAt, st.Running = d.Day, &started, d.FinishedAt, d.FinishedAt == nil
	if d.Result != "" {
		var r DailyResult
		if err := json.Unmarshal([]byte(d.Result), &r); err != nil {
			return st, fmt.Errorf("daily result: %w", err)
		}
		st.Result = &r
	}
	return st, nil
}

// dailyDay is the local day a daily run at now is due for, or "" when none is due yet (before the
// hour). The day is the local calendar date, so DST changes never make a day run twice.
func (m *Manager) dailyDay(now time.Time) string {
	now = now.Local()
	if now.Hour() < m.Config().DailyAt() {
		return ""
	}
	return now.Format(time.DateOnly)
}

// StartDaily starts the daily run in its own goroutine when one is due: enabled, past the hour,
// ctx (the daemon) not shutting down, no daily run in progress in this process, and the day not
// claimed yet in the store. It returns at once (true when a run was started), so it never delays
// launching queued jobs; WaitDaily waits for the run. Errors are logged, never returned.
func (m *Manager) StartDaily(ctx context.Context) bool {
	if m.Config().DailyOff || ctx.Err() != nil {
		return false
	}
	day := m.dailyDay(m.Now())
	if day == "" {
		return false
	}
	if !m.dailyMu.TryLock() {
		m.noteDailySkip(day) // the previous run is still going
		return false
	}
	ok, err := m.Store.ClaimDaily(day, m.Now())
	if err != nil || !ok {
		m.dailyMu.Unlock()
		if err != nil {
			m.Log.Printf("daily: %v", err)
		}
		return false
	}
	m.dailyNoteMu.Lock()
	m.dailyHeld = day
	m.dailyNoteMu.Unlock()
	m.dailyWG.Add(1)
	go func() {
		defer m.dailyWG.Done()
		if scan := m.runDaily(ctx, day); scan != nil {
			// An abandoned doctor procs scan keeps the lock until it really ends (the next day's run
			// never overlaps it), but the run is over for WaitDaily, so shutdown is not held up.
			go func() {
				<-scan
				m.dailyMu.Unlock()
			}()
			return
		}
		m.dailyMu.Unlock()
	}()
	return true
}

// noteDailySkip logs, at most once per day, that day's run is skipped because an earlier day's run
// (a hung step, or a doctor procs scan abandoned at its time-box) still holds the daily lock. The
// tick that finds today's own run in progress logs nothing.
func (m *Manager) noteDailySkip(day string) {
	m.dailyNoteMu.Lock()
	defer m.dailyNoteMu.Unlock()
	if m.dailyHeld == day || m.dailySkipped == day {
		return
	}
	m.dailySkipped = day
	m.Log.Printf("daily %s: previous daily run (%s) still in progress, skipping", day, m.dailyHeld)
}

// WaitDaily waits until a daily run started by StartDaily has ended. A doctor procs scan abandoned
// at its time-box is not waited for (it has no cancellation points); it may still be running.
func (m *Manager) WaitDaily() { m.dailyWG.Wait() }

// runDaily runs prune and doctor procs (report mode), each bounded by DailyStepTimeout and by ctx;
// a failing step never stops the other. A panic is recovered and logged: the daemon never crashes.
// scan is non-nil when the doctor procs scan was abandoned at the time-box; it is closed when the
// scan ends.
func (m *Manager) runDaily(ctx context.Context, day string) (scan <-chan struct{}) {
	var r DailyResult
	defer func() {
		if p := recover(); p != nil {
			r.Error = fmt.Sprintf("daily run panicked: %v", p)
			m.Log.Printf("daily %s: %s", day, r.Error)
		}
		m.finishDaily(day, r)
	}()
	m.Log.Printf("daily %s: prune and doctor procs (report only)", day)
	pctx, cancel := context.WithTimeout(ctx, DailyStepTimeout)
	rep, err := m.dailyPrune(pctx)
	cancel()
	r.JobsPruned, r.BytesFreed, r.ArchivedBytes, r.ArchiveExpired = len(rep.Jobs), rep.Bytes, rep.ArchivedBytes, len(rep.ArchiveExpired)
	for _, j := range rep.Jobs {
		if len(j.Archived) > 0 {
			r.JobsArchived++
		}
	}
	if err != nil {
		r.PruneError = err.Error()
		m.Log.Printf("daily %s: prune: %v", day, err)
	}
	if ctx.Err() != nil {
		r.ProcsError = "skipped: the daemon is shutting down"
		return nil
	}
	dctx, cancel := context.WithTimeout(ctx, DailyStepTimeout)
	procs, scan, err := m.dailyProcs(dctx)
	cancel()
	r.Ghosts, r.DeadRows, r.ProcsNote = len(procs.Ghosts), len(procs.DeadRows), procs.Note
	for _, g := range procs.Ghosts {
		switch g.Kind {
		case GhostJob:
			r.JobGhosts++
		case GhostLeader:
			r.Leaders++
		case GhostSuspect:
			r.Suspects++
		}
	}
	if err != nil {
		r.ProcsError = err.Error()
		m.Log.Printf("daily %s: doctor procs: %v", day, err)
	}
	return scan
}

func (m *Manager) finishDaily(day string, r DailyResult) {
	m.Log.Printf("daily %s: prune %d jobs, %d bytes freed, %d jobs archived (%d bytes), %d archived months expired; "+
		"doctor procs %d ghosts (%d job, %d leaders, %d suspects), %d dead rows (report only, nothing killed)",
		day, r.JobsPruned, r.BytesFreed, r.JobsArchived, r.ArchivedBytes, r.ArchiveExpired,
		r.Ghosts, r.JobGhosts, r.Leaders, r.Suspects, r.DeadRows)
	b, err := json.Marshal(r)
	if err == nil {
		err = m.Store.FinishDaily(day, m.Now(), string(b))
	}
	if err != nil {
		m.Log.Printf("daily %s: record the result: %v", day, err)
	}
}

// dailyPrune is the prune step (overridable in tests).
func (m *Manager) dailyPrune(ctx context.Context) (PruneReport, error) {
	if m.dailyPruneFn != nil {
		return m.dailyPruneFn(ctx)
	}
	return m.PruneContext(ctx, false)
}

// dailyProcs is the doctor procs step, never killing (overridable in tests). The scan has no
// cancellation points of its own: it is waited for until ctx ends, then left to finish alone, and
// the returned channel (nil when the scan finished in time) is closed when it ends. A panic in the
// scan is recovered and returned as an error.
func (m *Manager) dailyProcs(ctx context.Context) (ProcsReport, <-chan struct{}, error) {
	f := m.dailyProcsFn
	if f == nil {
		f = func(context.Context) (ProcsReport, error) { return m.DoctorProcs(false) }
	}
	type res struct {
		rep ProcsReport
		err error
	}
	ch, done := make(chan res, 1), make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if p := recover(); p != nil {
				ch <- res{err: fmt.Errorf("doctor procs panicked: %v", p)}
			}
		}()
		rep, err := f(ctx)
		ch <- res{rep, err}
	}()
	select {
	case r := <-ch:
		return r.rep, nil, r.err
	case <-ctx.Done():
		return ProcsReport{}, done, fmt.Errorf("doctor procs: %w", ctx.Err())
	}
}
