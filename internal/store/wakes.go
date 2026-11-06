package store

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"
)

// Wake kinds: a Delivery is a line typed into the orchestrator session (one wake of it), an
// Injection is text a hook added to the session's context (no wake of its own, but bytes it carries).
const (
	WakeDelivery  = "delivery"
	WakeInjection = "injection"
)

// Wake is one row of orchestrator_wakes (#170): what reached the orchestrator session, by category.
type Wake struct {
	ID       int64     `json:"id"`
	At       time.Time `json:"at"`
	Kind     string    `json:"kind"`
	Category string    `json:"category"`
	// Bytes is the size of the line or injected text as it reached the session.
	Bytes int `json:"bytes"`
	// BatchSize is how many things the one delivery carried (jobs of a coalesced line, alerts of a
	// check-missed line); Members names them (job ids, check alert ids).
	BatchSize int    `json:"batch_size"`
	Members   string `json:"members,omitempty"`
	// Ref is the delivery's identity: a second record with the same kind, category and ref is the
	// same delivery again (a retry after its mark failed) and is not counted twice. "" = no identity.
	Ref string `json:"-"`
	// MemberRefs are the identities of the things a delivery carries (a job's id, attempt and finish
	// time). A delivery whose members were all counted before is a retry even when the batch
	// regrouped (a member resumed or cancelled between the attempts). nil = identity by Ref only.
	MemberRefs []string `json:"-"`
}

// SLOSample is one recorded duration of an SLO name.
type SLOSample struct {
	ID      int64     `json:"id"`
	Name    string    `json:"name"`
	Seconds float64   `json:"seconds"`
	At      time.Time `json:"at"`
}

// SLOBreach is a sample over its configured norm. NotedAt is set once the breach was added to a
// delivery; InvestigatedAt once slo.investigate decided about it (started a job or skipped it).
type SLOBreach struct {
	ID             int64      `json:"id"`
	Name           string     `json:"name"`
	Seconds        float64    `json:"seconds"`
	Target         float64    `json:"target"`
	At             time.Time  `json:"at"`
	NotedAt        *time.Time `json:"noted_at,omitempty"`
	InvestigatedAt *time.Time `json:"investigated_at,omitempty"`
	// InvestigateJob is the research job started for the breach ("" = none started).
	InvestigateJob string `json:"investigate_job,omitempty"`
	// InvestigateSkip is why no job was started (cooldown, daily cap).
	InvestigateSkip string `json:"investigate_skip,omitempty"`
}

// wakesSchema is applied on every Open (CREATE … IF NOT EXISTS).
const wakesSchema = `
CREATE TABLE IF NOT EXISTS orchestrator_wakes (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	at         INTEGER NOT NULL,
	kind       TEXT NOT NULL,
	category   TEXT NOT NULL,
	bytes      INTEGER NOT NULL,
	batch_size INTEGER NOT NULL DEFAULT 1,
	members    TEXT NOT NULL DEFAULT '',
	ref        TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS orchestrator_wakes_at ON orchestrator_wakes(at);
CREATE UNIQUE INDEX IF NOT EXISTS orchestrator_wakes_ref ON orchestrator_wakes(kind, category, ref) WHERE ref <> '';
CREATE TABLE IF NOT EXISTS wake_member_refs (
	kind     TEXT NOT NULL,
	category TEXT NOT NULL,
	ref      TEXT NOT NULL,
	PRIMARY KEY (kind, category, ref)
);
CREATE TABLE IF NOT EXISTS slo_samples (
	id      INTEGER PRIMARY KEY AUTOINCREMENT,
	name    TEXT NOT NULL,
	seconds REAL NOT NULL,
	at      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS slo_samples_name ON slo_samples(name, at);
CREATE TABLE IF NOT EXISTS slo_breaches (
	id               INTEGER PRIMARY KEY AUTOINCREMENT,
	name             TEXT NOT NULL,
	seconds          REAL NOT NULL,
	target           REAL NOT NULL,
	at               INTEGER NOT NULL,
	noted_at         INTEGER,
	investigated_at  INTEGER,
	investigate_job  TEXT NOT NULL DEFAULT '',
	investigate_skip TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS slo_breaches_unnoted ON slo_breaches(id) WHERE noted_at IS NULL;
CREATE INDEX IF NOT EXISTS slo_breaches_uninvestigated ON slo_breaches(id) WHERE investigated_at IS NULL;
CREATE INDEX IF NOT EXISTS slo_breaches_started ON slo_breaches(investigated_at) WHERE investigate_job <> '';
`

// AddWake records a delivery or an injection. It reports false when a record with the same kind,
// category and non-empty ref already exists (the same delivery seen again): nothing is added.
func (s *Store) AddWake(w Wake) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("add wake: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if len(w.MemberRefs) > 0 {
		fresh := 0
		for _, ref := range w.MemberRefs {
			res, err := tx.Exec(`INSERT OR IGNORE INTO wake_member_refs (kind, category, ref) VALUES (?, ?, ?)`, w.Kind, w.Category, ref)
			if err != nil {
				return false, fmt.Errorf("add wake: %w", err)
			}
			if n, err := res.RowsAffected(); err == nil && n > 0 {
				fresh++
			}
		}
		if fresh == 0 {
			return false, nil
		}
	}
	res, err := tx.Exec(`INSERT OR IGNORE INTO orchestrator_wakes (at, kind, category, bytes, batch_size, members, ref)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, w.At.Unix(), w.Kind, w.Category, w.Bytes, max(w.BatchSize, 1), w.Members, w.Ref)
	if err != nil {
		return false, fmt.Errorf("add wake: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("add wake: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("add wake: %w", err)
	}
	return n > 0, nil
}

// Wakes returns the records at or after since, oldest first.
func (s *Store) Wakes(since time.Time) ([]Wake, error) {
	rows, err := s.db.Query(`SELECT id, at, kind, category, bytes, batch_size, members FROM orchestrator_wakes
		WHERE at >= ? ORDER BY id`, since.Unix())
	if err != nil {
		return nil, fmt.Errorf("wakes: %w", err)
	}
	defer rows.Close()
	var out []Wake
	for rows.Next() {
		var w Wake
		var at int64
		if err := rows.Scan(&w.ID, &at, &w.Kind, &w.Category, &w.Bytes, &w.BatchSize, &w.Members); err != nil {
			return nil, fmt.Errorf("wakes: %w", err)
		}
		w.At = time.Unix(at, 0)
		out = append(out, w)
	}
	return out, rows.Err()
}

// AddSLOSample stores a duration for name. With target > 0 and seconds over it, the breach is stored
// in the same transaction and returned (nil = no breach).
func (s *Store) AddSLOSample(name string, seconds, target float64, at time.Time) (*SLOBreach, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("slo record: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`INSERT INTO slo_samples (name, seconds, at) VALUES (?, ?, ?)`, name, seconds, at.Unix()); err != nil {
		return nil, fmt.Errorf("slo record: %w", err)
	}
	var b *SLOBreach
	if target > 0 && seconds > target {
		res, err := tx.Exec(`INSERT INTO slo_breaches (name, seconds, target, at) VALUES (?, ?, ?, ?)`, name, seconds, target, at.Unix())
		if err != nil {
			return nil, fmt.Errorf("slo record: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return nil, fmt.Errorf("slo record: %w", err)
		}
		b = &SLOBreach{ID: id, Name: name, Seconds: seconds, Target: target, At: time.Unix(at.Unix(), 0)}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("slo record: %w", err)
	}
	return b, nil
}

// SLOSamples returns the samples at or after since, oldest first.
func (s *Store) SLOSamples(since time.Time) ([]SLOSample, error) {
	rows, err := s.db.Query(`SELECT id, name, seconds, at FROM slo_samples WHERE at >= ? ORDER BY id`, since.Unix())
	if err != nil {
		return nil, fmt.Errorf("slo samples: %w", err)
	}
	defer rows.Close()
	var out []SLOSample
	for rows.Next() {
		var v SLOSample
		var at int64
		if err := rows.Scan(&v.ID, &v.Name, &v.Seconds, &at); err != nil {
			return nil, fmt.Errorf("slo samples: %w", err)
		}
		v.At = time.Unix(at, 0)
		out = append(out, v)
	}
	return out, rows.Err()
}

const breachCols = `id, name, seconds, target, at, noted_at, investigated_at, investigate_job, investigate_skip`

func scanBreach(r scanner) (SLOBreach, error) {
	var b SLOBreach
	var at int64
	var noted, inv sql.NullInt64
	if err := r.Scan(&b.ID, &b.Name, &b.Seconds, &b.Target, &at, &noted, &inv, &b.InvestigateJob, &b.InvestigateSkip); err != nil {
		return b, err
	}
	b.At = time.Unix(at, 0)
	if noted.Valid {
		t := time.Unix(noted.Int64, 0)
		b.NotedAt = &t
	}
	if inv.Valid {
		t := time.Unix(inv.Int64, 0)
		b.InvestigatedAt = &t
	}
	return b, nil
}

func (s *Store) queryBreaches(where string, args ...any) ([]SLOBreach, error) {
	rows, err := s.db.Query(`SELECT `+breachCols+` FROM slo_breaches WHERE `+where+` ORDER BY id`, args...)
	if err != nil {
		return nil, fmt.Errorf("slo breaches: %w", err)
	}
	defer rows.Close()
	var out []SLOBreach
	for rows.Next() {
		b, err := scanBreach(rows)
		if err != nil {
			return nil, fmt.Errorf("slo breaches: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// UnnotedSLOBreaches are the breaches no delivery carried yet, oldest first.
func (s *Store) UnnotedSLOBreaches() ([]SLOBreach, error) {
	return s.queryBreaches(`noted_at IS NULL`)
}

// MarkSLOBreachesNoted marks breaches as carried by a delivery; one already marked keeps its time.
func (s *Store) MarkSLOBreachesNoted(ids []int64, at time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := []any{at.Unix()}
	for _, id := range ids {
		args = append(args, id)
	}
	if _, err := s.db.Exec(`UPDATE slo_breaches SET noted_at = ? WHERE noted_at IS NULL AND id IN (`+marks+`)`, args...); err != nil {
		return fmt.Errorf("mark slo breaches noted: %w", err)
	}
	return nil
}

// UninvestigatedSLOBreaches are the breaches slo.investigate has not decided about yet.
func (s *Store) UninvestigatedSLOBreaches() ([]SLOBreach, error) {
	return s.queryBreaches(`investigated_at IS NULL`)
}

// InvestigatePending is the investigate_job of a breach whose investigation is reserved and whose job
// is not linked yet. It counts as a started investigation (a crash between the reservation and the
// link must not allow a second job for the breach); job ids never take this form.
const InvestigatePending = "pending"

// SetSLOBreachInvestigated stores the decision about a breach: the research job started for it, or
// (jobID "") why none was.
func (s *Store) SetSLOBreachInvestigated(id int64, at time.Time, jobID, skip string) error {
	return affected(s.db.Exec(`UPDATE slo_breaches SET investigated_at = ?, investigate_job = ?, investigate_skip = ? WHERE id = ?`,
		at.Unix(), jobID, skip, id))
}

// ClaimSLOInvestigation decides, in one transaction, whether the breach may start an investigation now:
// not within cooldown of the latest started one (however long ago it was, so a cooldown over a day is
// honoured) and fewer than dailyMax started within 24 hours. Allowed, the breach is reserved
// (investigate_job = InvestigatePending) before any job is queued and reserved is true; the caller then
// links the job (LinkSLOInvestigation) or releases the reservation (SetSLOBreachInvestigated with "").
// Refused, the breach is closed with skip as its reason. A breach already decided returns reserved
// false and an empty skip.
func (s *Store) ClaimSLOInvestigation(id int64, now time.Time, cooldown time.Duration, dailyMax int) (reserved bool, skip string, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, "", fmt.Errorf("slo investigation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var open int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM slo_breaches WHERE id = ? AND investigated_at IS NULL`, id).Scan(&open); err != nil {
		return false, "", fmt.Errorf("slo investigation: %w", err)
	}
	if open == 0 {
		return false, "", nil
	}
	var inDay int
	var last sql.NullInt64
	if err := tx.QueryRow(`SELECT COUNT(*) FROM slo_breaches WHERE investigate_job <> '' AND investigated_at >= ?`,
		now.Add(-24*time.Hour).Unix()).Scan(&inDay); err != nil {
		return false, "", fmt.Errorf("slo investigation: %w", err)
	}
	if err := tx.QueryRow(`SELECT MAX(investigated_at) FROM slo_breaches WHERE investigate_job <> ''`).Scan(&last); err != nil {
		return false, "", fmt.Errorf("slo investigation: %w", err)
	}
	switch {
	case inDay >= dailyMax:
		skip = fmt.Sprintf("daily cap: %d investigations in the last 24h", inDay)
	case last.Valid && now.Sub(time.Unix(last.Int64, 0)) < cooldown:
		skip = fmt.Sprintf("cooldown: last investigation started %s ago", now.Sub(time.Unix(last.Int64, 0)).Round(time.Second))
	}
	job := InvestigatePending
	if skip != "" {
		job = ""
	}
	if _, err := tx.Exec(`UPDATE slo_breaches SET investigated_at = ?, investigate_job = ?, investigate_skip = ? WHERE id = ?`,
		now.Unix(), job, skip, id); err != nil {
		return false, "", fmt.Errorf("slo investigation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, "", fmt.Errorf("slo investigation: %w", err)
	}
	return skip == "", skip, nil
}

// OpenQuickWriter opens the existing database at path for one small write from a hook: no schema
// work, a short busy wait, so a locked database fails fast and the hook goes on without recording.
func OpenQuickWriter(path string, busy time.Duration) (*Store, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(%d)", path, busy.Milliseconds())
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open db: %w", err)
	}
	return &Store{db: db}, nil
}
