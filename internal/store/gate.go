package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// GateInput is what the daemon assembled for a gate job (Wave F: the slicing check, F3, and the blind
// judge, F5): read from the repository's brief/ files when the job was queued, never from the
// orchestrator. The job's prompt is built from it; the wrapper validates the job's JSON result against it.
type GateInput struct {
	// Repo is the repository's main worktree (the key results are stored under); Top the work tree
	// the brief files were read from; GitCommonDir its git common dir. A gate job reads none of them
	// (the guard's --no-read).
	Repo         string `json:"repo"`
	Top          string `json:"top"`
	GitCommonDir string `json:"git_common_dir"`
	ChecklistID  string `json:"checklist_id"`
	Version      int    `json:"version"`
	// ChecklistSHA256 is the checklist hash (F1) of the files as read; BriefSHA256 the sha256 of BRIEF.md.
	ChecklistSHA256 string `json:"checklist_sha256"`
	BriefSHA256     string `json:"brief_sha256"`
	// SHA is the commit the judge checks (judge only).
	SHA string `json:"sha,omitempty"`
	// Brief is BRIEF.md in full (with ## Additions), redacted.
	Brief string    `json:"brief"`
	Rows  []GateRow `json:"rows"`
	Live  []string  `json:"live"`
	At    time.Time `json:"at"`
}

// GateRow is one checklist row as a gate job is given it: ID, brief quote, contract and rationale.
type GateRow struct {
	ID            string `json:"id"`
	Verbatim      string `json:"verbatim"`
	Contract      string `json:"contract"`
	PlaceholderOK bool   `json:"placeholder_ok,omitempty"`
	Rationale     string `json:"rationale,omitempty"`
}

// gateInputJSON is the gate_input column: "" for none.
func gateInputJSON(g *GateInput) string {
	if g == nil {
		return ""
	}
	b, err := json.Marshal(g)
	if err != nil {
		return "" // strings, ints, bools and a time always marshal
	}
	return string(b)
}

// SlicingResult is the stored result of a slicing check (F3): the three lists of a validated
// agent-jobs/slicing/v1 block, bound to the checklist id, version and hash it was run on. Written only
// by the job's wrapper under its launch token.
type SlicingResult struct {
	Seq   int64  `json:"seq"`
	JobID string `json:"job_id"`
	// Attempt is the job's attempt that stored the result (read from the job row under the launch
	// token, never given by the caller): a later attempt of the same job needs its own result.
	Attempt         int    `json:"attempt"`
	Repo            string `json:"repo"`
	ChecklistID     string `json:"checklist_id"`
	Version         int    `json:"version"`
	ChecklistSHA256 string `json:"checklist_sha256"`
	Model           string `json:"model"`
	Runner          string `json:"runner"`
	// EnforcedBlind is true when the job ran under the guard's read allowlist (claude); a codex job
	// has no OS confinement of its reads (ticket 684), so its result is stored with false.
	EnforcedBlind bool `json:"enforced_blind"`
	// VoidsApproval is the seq of the repository's approval this result voided when it was stored (0 =
	// none): the latest approval was of this checklist id, version and hash and the result listed an
	// unmapped quote without an owner decision. Set by PutSlicing, never by the caller.
	VoidsApproval int64     `json:"voids_approval,omitempty"`
	Unmapped      []Quote   `json:"unmapped"`
	Half          []Half    `json:"half"`
	Invented      []Invent  `json:"invented"`
	CreatedAt     time.Time `json:"created_at"`
}

// Quote is a brief quote that no row maps (slicing unmapped).
type Quote struct {
	Quote string `json:"quote"`
	Why   string `json:"why,omitempty"`
}

// Half is a brief quote only partly covered by the rows named.
type Half struct {
	Quote   string   `json:"quote"`
	Rows    []string `json:"rows"`
	Missing string   `json:"missing,omitempty"`
}

// Invent is a checklist row the brief does not support.
type Invent struct {
	Row string `json:"row"`
	Why string `json:"why,omitempty"`
}

// SlicingDecision is the owner's explicit decision on one unmapped quote of a slicing result, written
// only by checklist_set with the owner's words, bound to the checklist id, version and hash.
type SlicingDecision struct {
	Repo            string    `json:"repo"`
	ChecklistID     string    `json:"checklist_id"`
	Version         int       `json:"version"`
	ChecklistSHA256 string    `json:"checklist_sha256"`
	Unmapped        string    `json:"unmapped"`
	Quote           string    `json:"quote"`
	CreatedAt       time.Time `json:"created_at"`
}

// JudgeVerdict is the stored verdict of a blind judge (F5): the validated agent-jobs/judge/v1 block,
// keyed by repo, sha, checklist id and version, judge model and family. Written only by the job's
// wrapper under its launch token.
type JudgeVerdict struct {
	Seq   int64  `json:"seq"`
	JobID string `json:"job_id"`
	// Attempt is the job's attempt that stored the verdict (as SlicingResult.Attempt).
	Attempt         int    `json:"attempt"`
	Repo            string `json:"repo"`
	SHA             string `json:"sha"`
	ChecklistID     string `json:"checklist_id"`
	Version         int    `json:"version"`
	ChecklistSHA256 string `json:"checklist_sha256"`
	JudgeModel      string `json:"judge_model"`
	JudgeRunner     string `json:"judge_runner"`
	JudgeFamily     string `json:"judge_family"`
	// EnforcedBlind: as SlicingResult.EnforcedBlind (false for a codex judge).
	EnforcedBlind bool `json:"enforced_blind"`
	// CoderFamilies are the model families of the feature jobs (fix, general) whose covers name this
	// checklist id, CoderJobs those jobs; FamilyDiffers is true only when there is at least one and none
	// shares the judge's family; SameFamily when one does (stored, flagged: it cannot pass release_check).
	// Both are a snapshot of the cover jobs when the verdict was stored: a later coder job does not
	// change them, so release_check (F7) recomputes them.
	CoderFamilies []string `json:"coder_families"`
	CoderJobs     []string `json:"coder_jobs"`
	FamilyDiffers bool     `json:"family_differs"`
	SameFamily    bool     `json:"same_family"`
	// ManifestCheck is the manifest-check record (F5 / F6) of the job's attempt that stored the verdict:
	// the run of scripts/manifest-check.sh the wrapper made at the sha before the judge started (null only
	// for a verdict stored by an earlier build, which release_check does not take).
	ManifestCheck *ManifestCheck `json:"manifest_check"`
	// Counts per verdict value (met, partial, missing, placeholder, cannot_check).
	Counts map[string]int `json:"counts"`
	// Verdict is the judge's validated block (JSON object), redacted.
	Verdict   any       `json:"verdict"`
	CreatedAt time.Time `json:"created_at"`
}

// gateSchema is applied on every Open. Results and verdicts are logs (the latest per key, by seq, is
// the one read); decisions: one per unmapped quote and checklist version and hash, a second replaces it.
const gateSchema = `
CREATE TABLE IF NOT EXISTS gate_slicing (
	seq              INTEGER PRIMARY KEY AUTOINCREMENT,
	job_id           TEXT NOT NULL,
	repo             TEXT NOT NULL,
	checklist_id     TEXT NOT NULL,
	version          INTEGER NOT NULL,
	checklist_sha256 TEXT NOT NULL,
	model            TEXT NOT NULL,
	runner           TEXT NOT NULL,
	result           TEXT NOT NULL,
	created_at       INTEGER NOT NULL,
	attempt          INTEGER NOT NULL DEFAULT 0,
	enforced_blind   INTEGER NOT NULL DEFAULT 0,
	voids_approval   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS gate_slicing_key ON gate_slicing(repo, checklist_id, version, checklist_sha256, seq);
CREATE TABLE IF NOT EXISTS slicing_decisions (
	repo             TEXT NOT NULL,
	checklist_id     TEXT NOT NULL,
	version          INTEGER NOT NULL,
	checklist_sha256 TEXT NOT NULL,
	unmapped         TEXT NOT NULL,
	quote            TEXT NOT NULL,
	created_at       INTEGER NOT NULL,
	PRIMARY KEY (repo, checklist_id, version, checklist_sha256, unmapped)
);
CREATE TABLE IF NOT EXISTS gate_verdicts (
	seq              INTEGER PRIMARY KEY AUTOINCREMENT,
	job_id           TEXT NOT NULL,
	repo             TEXT NOT NULL,
	sha              TEXT NOT NULL,
	checklist_id     TEXT NOT NULL,
	version          INTEGER NOT NULL,
	checklist_sha256 TEXT NOT NULL,
	judge_model      TEXT NOT NULL,
	judge_runner     TEXT NOT NULL,
	judge_family     TEXT NOT NULL,
	coder_families   TEXT NOT NULL,
	coder_jobs       TEXT NOT NULL,
	family_differs   INTEGER NOT NULL,
	same_family      INTEGER NOT NULL,
	manifest_check   TEXT,
	counts           TEXT NOT NULL,
	verdict          TEXT NOT NULL,
	created_at       INTEGER NOT NULL,
	attempt          INTEGER NOT NULL DEFAULT 0,
	enforced_blind   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS gate_verdicts_key ON gate_verdicts(repo, sha, checklist_id, version, seq);
`

// gateColumns are the columns a gate table created by an earlier build of this branch lacks; added on
// every Open (a column already there is no error).
var gateColumns = []string{
	`ALTER TABLE gate_slicing ADD COLUMN attempt INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE gate_slicing ADD COLUMN enforced_blind INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE gate_slicing ADD COLUMN voids_approval INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE gate_verdicts ADD COLUMN attempt INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE gate_verdicts ADD COLUMN enforced_blind INTEGER NOT NULL DEFAULT 0`,
}

// ErrGateIncomplete is a gate record without its key fields.
var ErrGateIncomplete = errors.New("gate record needs job, repo, checklist id, version and hash")

// ErrNotRunningLaunch is a gate write whose job is not running under the launch token given.
var ErrNotRunningLaunch = errors.New("the job is not running under this launch token")

// runningUnder checks, inside tx, that job id runs under token, and returns the attempt it runs.
func runningUnder(tx *sql.Tx, id, token string) (int, error) {
	var attempt int
	err := tx.QueryRow(`SELECT attempt FROM jobs WHERE id=? AND status='running' AND launch_token=? AND launch_token<>''`, id, token).Scan(&attempt)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotRunningLaunch
	}
	return attempt, err
}

// PutSlicing stores a slicing result for the running launch token of its job (the wrapper's write).
func (s *Store) PutSlicing(token string, r SlicingResult) (int64, error) {
	if r.JobID == "" || r.Repo == "" || r.ChecklistID == "" || r.Version < 1 || r.ChecklistSHA256 == "" {
		return 0, ErrGateIncomplete
	}
	lists, err := json.Marshal(struct {
		Unmapped []Quote  `json:"unmapped"`
		Half     []Half   `json:"half"`
		Invented []Invent `json:"invented"`
	}{nonNil(r.Unmapped), nonNil(r.Half), nonNil(r.Invented)})
	if err != nil {
		return 0, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	attempt, err := runningUnder(tx, r.JobID, token)
	if err != nil {
		return 0, fmt.Errorf("put slicing result: %w", err)
	}
	voids, err := slicingVoids(tx, r)
	if err != nil {
		return 0, fmt.Errorf("put slicing result: %w", err)
	}
	res, err := tx.Exec(`INSERT INTO gate_slicing (job_id, attempt, repo, checklist_id, version, checklist_sha256, model, runner,
		enforced_blind, voids_approval, result, created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, r.JobID, attempt, r.Repo, r.ChecklistID,
		r.Version, r.ChecklistSHA256, r.Model, r.Runner, r.EnforcedBlind, voids, string(lists), r.CreatedAt.Unix())
	if err != nil {
		return 0, fmt.Errorf("put slicing result: %w", err)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	return seq, tx.Commit()
}

// slicingVoids is, inside tx, the seq of the repository's latest approval when r voids it (0 = it does
// not): the approval is of r's checklist id, version and hash and r lists an unmapped quote without a
// stored owner decision. The mark stays: decisions stored later do not restore that approval, only a new
// one (a higher seq) does.
func slicingVoids(tx *sql.Tx, r SlicingResult) (int64, error) {
	var seq int64
	err := tx.QueryRow(`SELECT seq FROM checklist_approvals WHERE repo=? ORDER BY seq DESC LIMIT 1`, r.Repo).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var hit int
	err = tx.QueryRow(`SELECT COUNT(*) FROM checklist_approvals WHERE seq=? AND checklist_id=? AND version=? AND checklist_sha256=?`,
		seq, r.ChecklistID, r.Version, r.ChecklistSHA256).Scan(&hit)
	if err != nil || hit == 0 {
		return 0, err
	}
	for _, q := range r.Unmapped {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM slicing_decisions WHERE repo=? AND checklist_id=? AND version=? AND
			checklist_sha256=? AND unmapped=?`, r.Repo, r.ChecklistID, r.Version, r.ChecklistSHA256, q.Quote).Scan(&n); err != nil {
			return 0, err
		}
		if n == 0 {
			return seq, nil
		}
	}
	return 0, nil
}

// ApprovalVoidedBy is the job id of the first slicing result that voided the approval with seq
// approval of repo ("" = none did).
func (s *Store) ApprovalVoidedBy(repo string, approval int64) (string, error) {
	if approval == 0 {
		return "", nil
	}
	var id string
	err := s.db.QueryRow(`SELECT job_id FROM gate_slicing WHERE repo=? AND voids_approval=? ORDER BY seq LIMIT 1`, repo,
		approval).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("approval voided by: %w", err)
	}
	return id, nil
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

const slicingCols = `seq, job_id, attempt, repo, checklist_id, version, checklist_sha256, model, runner, enforced_blind,
	voids_approval, result, created_at`

func scanSlicing(r scanner) (*SlicingResult, error) {
	var out SlicingResult
	var lists string
	var at int64
	if err := r.Scan(&out.Seq, &out.JobID, &out.Attempt, &out.Repo, &out.ChecklistID, &out.Version, &out.ChecklistSHA256,
		&out.Model, &out.Runner, &out.EnforcedBlind, &out.VoidsApproval, &lists, &at); err != nil {
		return nil, err
	}
	var l struct {
		Unmapped []Quote  `json:"unmapped"`
		Half     []Half   `json:"half"`
		Invented []Invent `json:"invented"`
	}
	if err := json.Unmarshal([]byte(lists), &l); err != nil {
		return nil, fmt.Errorf("slicing result %d: %w", out.Seq, err)
	}
	out.Unmapped, out.Half, out.Invented = nonNil(l.Unmapped), nonNil(l.Half), nonNil(l.Invented)
	out.CreatedAt = time.Unix(at, 0).UTC()
	return &out, nil
}

// LatestSlicing is the latest slicing result of checklist id in repo, any version (nil = none).
func (s *Store) LatestSlicing(repo, id string) (*SlicingResult, error) {
	r, err := scanSlicing(s.db.QueryRow(`SELECT `+slicingCols+` FROM gate_slicing WHERE repo=? AND checklist_id=?
		ORDER BY seq DESC LIMIT 1`, repo, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("latest slicing result: %w", err)
	}
	return r, nil
}

// SlicingFor is the latest slicing result of checklist id in repo run on that version and hash
// (nil = none): a result for another version or hash does not count.
func (s *Store) SlicingFor(repo, id string, version int, sha256 string) (*SlicingResult, error) {
	r, err := scanSlicing(s.db.QueryRow(`SELECT `+slicingCols+` FROM gate_slicing WHERE repo=? AND checklist_id=? AND version=?
		AND checklist_sha256=? ORDER BY seq DESC LIMIT 1`, repo, id, version, sha256))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("slicing result: %w", err)
	}
	return r, nil
}

// SlicingResultsFor are all slicing results of checklist id in repo run on that version and hash,
// oldest first: the approval rule takes the unmapped quotes of every one of them.
func (s *Store) SlicingResultsFor(repo, id string, version int, sha256 string) ([]*SlicingResult, error) {
	rows, err := s.db.Query(`SELECT `+slicingCols+` FROM gate_slicing WHERE repo=? AND checklist_id=? AND version=?
		AND checklist_sha256=? ORDER BY seq`, repo, id, version, sha256)
	if err != nil {
		return nil, fmt.Errorf("slicing results: %w", err)
	}
	defer rows.Close()
	var out []*SlicingResult
	for rows.Next() {
		r, err := scanSlicing(rows)
		if err != nil {
			return nil, fmt.Errorf("slicing results: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SlicingByJob is the slicing result attempt of a job stored (nil = none): a result of an earlier
// attempt is not this attempt's.
func (s *Store) SlicingByJob(jobID string, attempt int) (*SlicingResult, error) {
	r, err := scanSlicing(s.db.QueryRow(`SELECT `+slicingCols+` FROM gate_slicing WHERE job_id=? AND attempt=? ORDER BY seq DESC
		LIMIT 1`, jobID, attempt))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("slicing result: %w", err)
	}
	return r, nil
}

// PutSlicingDecision stores (or replaces) the owner's decision on one unmapped quote.
func (s *Store) PutSlicingDecision(d SlicingDecision) error {
	if d.Repo == "" || d.ChecklistID == "" || d.Version < 1 || d.ChecklistSHA256 == "" || d.Unmapped == "" || d.Quote == "" {
		return errors.New("slicing decision needs repo, checklist id, version and hash, the unmapped quote and the owner's words")
	}
	_, err := s.db.Exec(`INSERT OR REPLACE INTO slicing_decisions (repo, checklist_id, version, checklist_sha256, unmapped, quote,
		created_at) VALUES (?,?,?,?,?,?,?)`, d.Repo, d.ChecklistID, d.Version, d.ChecklistSHA256, d.Unmapped, d.Quote, d.CreatedAt.Unix())
	if err != nil {
		return fmt.Errorf("put slicing decision: %w", err)
	}
	return nil
}

// SlicingDecisions lists the owner's decisions bound to checklist id, version and hash in repo.
func (s *Store) SlicingDecisions(repo, id string, version int, sha256 string) ([]SlicingDecision, error) {
	rows, err := s.db.Query(`SELECT repo, checklist_id, version, checklist_sha256, unmapped, quote, created_at FROM slicing_decisions
		WHERE repo=? AND checklist_id=? AND version=? AND checklist_sha256=? ORDER BY unmapped`, repo, id, version, sha256)
	if err != nil {
		return nil, fmt.Errorf("slicing decisions: %w", err)
	}
	defer rows.Close()
	out := []SlicingDecision{}
	for rows.Next() {
		var d SlicingDecision
		var at int64
		if err := rows.Scan(&d.Repo, &d.ChecklistID, &d.Version, &d.ChecklistSHA256, &d.Unmapped, &d.Quote, &at); err != nil {
			return nil, fmt.Errorf("slicing decisions: %w", err)
		}
		d.CreatedAt = time.Unix(at, 0).UTC()
		out = append(out, d)
	}
	return out, rows.Err()
}

// PutVerdict stores a judge verdict for the running launch token of its job (the wrapper's write).
func (s *Store) PutVerdict(token string, v JudgeVerdict) (int64, error) {
	if v.JobID == "" || v.Repo == "" || v.SHA == "" || v.ChecklistID == "" || v.Version < 1 || v.ChecklistSHA256 == "" ||
		v.Verdict == nil {
		return 0, ErrGateIncomplete
	}
	verdict, err := json.Marshal(v.Verdict)
	if err != nil {
		return 0, fmt.Errorf("put verdict: %w", err)
	}
	families, _ := json.Marshal(nonNil(v.CoderFamilies))
	coders, _ := json.Marshal(nonNil(v.CoderJobs))
	counts, _ := json.Marshal(v.Counts)
	var manifest any // NULL unless a record is given
	if v.ManifestCheck != nil {
		b, err := json.Marshal(v.ManifestCheck)
		if err != nil {
			return 0, fmt.Errorf("put verdict: %w", err)
		}
		manifest = string(b)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	attempt, err := runningUnder(tx, v.JobID, token)
	if err != nil {
		return 0, fmt.Errorf("put verdict: %w", err)
	}
	res, err := tx.Exec(`INSERT INTO gate_verdicts (job_id, attempt, repo, sha, checklist_id, version, checklist_sha256, judge_model,
		judge_runner, judge_family, enforced_blind, coder_families, coder_jobs, family_differs, same_family, manifest_check, counts,
		verdict, created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, v.JobID, attempt, v.Repo, v.SHA, v.ChecklistID,
		v.Version, v.ChecklistSHA256, v.JudgeModel, v.JudgeRunner, v.JudgeFamily, v.EnforcedBlind, string(families), string(coders),
		v.FamilyDiffers, v.SameFamily, manifest, string(counts), string(verdict), v.CreatedAt.Unix())
	if err != nil {
		return 0, fmt.Errorf("put verdict: %w", err)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	return seq, tx.Commit()
}

const verdictCols = `seq, job_id, attempt, repo, sha, checklist_id, version, checklist_sha256, judge_model, judge_runner,
	judge_family, enforced_blind, coder_families, coder_jobs, family_differs, same_family, manifest_check, counts, verdict, created_at`

func scanVerdict(r scanner) (*JudgeVerdict, error) {
	var v JudgeVerdict
	var families, coders, counts, verdict string
	var manifest sql.NullString
	var at int64
	if err := r.Scan(&v.Seq, &v.JobID, &v.Attempt, &v.Repo, &v.SHA, &v.ChecklistID, &v.Version, &v.ChecklistSHA256, &v.JudgeModel,
		&v.JudgeRunner, &v.JudgeFamily, &v.EnforcedBlind, &families, &coders, &v.FamilyDiffers, &v.SameFamily, &manifest, &counts, &verdict, &at); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(families), &v.CoderFamilies); err != nil {
		return nil, fmt.Errorf("verdict %d: %w", v.Seq, err)
	}
	if err := json.Unmarshal([]byte(coders), &v.CoderJobs); err != nil {
		return nil, fmt.Errorf("verdict %d: %w", v.Seq, err)
	}
	if err := json.Unmarshal([]byte(counts), &v.Counts); err != nil {
		return nil, fmt.Errorf("verdict %d: %w", v.Seq, err)
	}
	if manifest.Valid {
		if err := json.Unmarshal([]byte(manifest.String), &v.ManifestCheck); err != nil {
			return nil, fmt.Errorf("verdict %d: manifest_check: %w", v.Seq, err)
		}
	}
	if err := json.Unmarshal([]byte(verdict), &v.Verdict); err != nil {
		return nil, fmt.Errorf("verdict %d: %w", v.Seq, err)
	}
	v.CreatedAt = time.Unix(at, 0).UTC()
	return &v, nil
}

// LatestVerdict is the latest verdict of checklist id in repo, for sha when sha is not empty (nil = none).
func (s *Store) LatestVerdict(repo, id, sha string) (*JudgeVerdict, error) {
	q := `SELECT ` + verdictCols + ` FROM gate_verdicts WHERE repo=? AND checklist_id=?`
	args := []any{repo, id}
	if sha != "" {
		q += ` AND sha=?`
		args = append(args, sha)
	}
	v, err := scanVerdict(s.db.QueryRow(q+` ORDER BY seq DESC LIMIT 1`, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("latest verdict: %w", err)
	}
	return v, nil
}

// VerdictsAt lists every verdict of checklist id at version for sha in repo, oldest first: release_check
// (F7) takes them all, so a verdict that disagrees with the latest one is not passed over.
func (s *Store) VerdictsAt(repo, id string, version int, sha string) ([]*JudgeVerdict, error) {
	rows, err := s.db.Query(`SELECT `+verdictCols+` FROM gate_verdicts WHERE repo=? AND checklist_id=? AND version=? AND sha=?
		ORDER BY seq`, repo, id, version, sha)
	if err != nil {
		return nil, fmt.Errorf("verdicts: %w", err)
	}
	defer rows.Close()
	var out []*JudgeVerdict
	for rows.Next() {
		v, err := scanVerdict(rows)
		if err != nil {
			return nil, fmt.Errorf("verdicts: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// VerdictByJob is the verdict attempt of a job stored (nil = none).
func (s *Store) VerdictByJob(jobID string, attempt int) (*JudgeVerdict, error) {
	v, err := scanVerdict(s.db.QueryRow(`SELECT `+verdictCols+` FROM gate_verdicts WHERE job_id=? AND attempt=? ORDER BY seq DESC
		LIMIT 1`, jobID, attempt))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("verdict: %w", err)
	}
	return v, nil
}

// CoverJob is a feature job (fix or general) whose covers name a checklist: who coded against it.
type CoverJob struct {
	ID     string
	Kind   string
	Runner string
	Model  string
	// Covers are the requirement IDs the job was started with.
	Covers []string
}

// CoverJobs lists the fix and general jobs of repo whose covers were read from checklist id (any
// version), oldest first.
func (s *Store) CoverJobs(repo, id string) ([]CoverJob, error) {
	rows, err := s.db.Query(`SELECT id, kind, runner, model, covers, cover_contract FROM jobs WHERE repo_path=? AND cover_contract<>''
		AND kind IN ('fix','general') ORDER BY created_at, id`, repo)
	if err != nil {
		return nil, fmt.Errorf("cover jobs: %w", err)
	}
	defer rows.Close()
	var out []CoverJob
	for rows.Next() {
		var j CoverJob
		var covers, cc string
		if err := rows.Scan(&j.ID, &j.Kind, &j.Runner, &j.Model, &covers, &cc); err != nil {
			return nil, fmt.Errorf("cover jobs: %w", err)
		}
		var c CoverContract
		if err := json.Unmarshal([]byte(cc), &c); err != nil {
			return nil, fmt.Errorf("cover jobs: job %s: cover_contract: %w", j.ID, err)
		}
		if c.ChecklistID == id {
			j.Covers = SplitNames(covers)
			out = append(out, j)
		}
	}
	return out, rows.Err()
}
