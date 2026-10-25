package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// BriefingQuestion is one question of a briefing (F8) for the owner: a choice with the answer the job
// proposes, naming the checklist rows the answer settles.
type BriefingQuestion struct {
	N           int      `json:"n"`
	Closes      []string `json:"closes"`
	Kind        string   `json:"kind"`
	Q           string   `json:"q"`
	Recommended string   `json:"recommended"`
	Why         string   `json:"why"`
}

// BriefingRound is one round of questions put to the owner at once.
type BriefingRound struct {
	Questions []BriefingQuestion `json:"questions"`
}

// BriefingDecided is a point the job settled itself, without the owner: an ASSUMPTION the owner sees
// and may overturn, naming the rows it settles.
type BriefingDecided struct {
	Text   string   `json:"text"`
	Closes []string `json:"closes"`
}

// BriefingResult is the stored result of a briefing job (F8): the validated agent-jobs/briefing/v1
// block, bound to the checklist id, version and hash it was run on (a draft: the checklist need not be
// approved). Written only by the job's wrapper under its launch token.
type BriefingResult struct {
	Seq   int64  `json:"seq"`
	JobID string `json:"job_id"`
	// Attempt is the job's attempt that stored the result (as SlicingResult.Attempt).
	Attempt         int    `json:"attempt"`
	Repo            string `json:"repo"`
	ChecklistID     string `json:"checklist_id"`
	Version         int    `json:"version"`
	ChecklistSHA256 string `json:"checklist_sha256"`
	Model           string `json:"model"`
	Runner          string `json:"runner"`
	// EnforcedBlind: as SlicingResult.EnforcedBlind (false for a codex job).
	EnforcedBlind bool               `json:"enforced_blind"`
	Rounds        []BriefingRound    `json:"rounds"`
	Decided       []BriefingDecided  `json:"decided"`
	Adversarial   []BriefingQuestion `json:"adversarial"`
	CreatedAt     time.Time          `json:"created_at"`
}

// briefingBody is the result column of gate_briefing.
type briefingBody struct {
	Rounds      []BriefingRound    `json:"rounds"`
	Decided     []BriefingDecided  `json:"decided"`
	Adversarial []BriefingQuestion `json:"adversarial"`
}

// PutBriefing stores a briefing result for the running launch token of its job (the wrapper's write).
func (s *Store) PutBriefing(token string, r BriefingResult) (int64, error) {
	if r.JobID == "" || r.Repo == "" || r.ChecklistID == "" || r.Version < 1 || r.ChecklistSHA256 == "" {
		return 0, ErrGateIncomplete
	}
	body, err := json.Marshal(briefingBody{Rounds: nonNil(r.Rounds), Decided: nonNil(r.Decided), Adversarial: nonNil(r.Adversarial)})
	if err != nil {
		return 0, fmt.Errorf("put briefing result: %w", err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	attempt, err := runningUnder(tx, r.JobID, token)
	if err != nil {
		return 0, fmt.Errorf("put briefing result: %w", err)
	}
	res, err := tx.Exec(`INSERT INTO gate_briefing (job_id, attempt, repo, checklist_id, version, checklist_sha256, model, runner,
		enforced_blind, result, created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)`, r.JobID, attempt, r.Repo, r.ChecklistID, r.Version,
		r.ChecklistSHA256, r.Model, r.Runner, r.EnforcedBlind, string(body), r.CreatedAt.Unix())
	if err != nil {
		return 0, fmt.Errorf("put briefing result: %w", err)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	return seq, tx.Commit()
}

const briefingCols = `seq, job_id, attempt, repo, checklist_id, version, checklist_sha256, model, runner, enforced_blind, result, created_at`

func scanBriefing(r scanner) (*BriefingResult, error) {
	var out BriefingResult
	var body string
	var at int64
	if err := r.Scan(&out.Seq, &out.JobID, &out.Attempt, &out.Repo, &out.ChecklistID, &out.Version, &out.ChecklistSHA256,
		&out.Model, &out.Runner, &out.EnforcedBlind, &body, &at); err != nil {
		return nil, err
	}
	var b briefingBody
	if err := json.Unmarshal([]byte(body), &b); err != nil {
		return nil, fmt.Errorf("briefing result %d: %w", out.Seq, err)
	}
	out.Rounds, out.Decided, out.Adversarial = nonNil(b.Rounds), nonNil(b.Decided), nonNil(b.Adversarial)
	out.CreatedAt = time.Unix(at, 0).UTC()
	return &out, nil
}

func (s *Store) oneBriefing(what, q string, args ...any) (*BriefingResult, error) {
	r, err := scanBriefing(s.db.QueryRow(`SELECT `+briefingCols+` FROM gate_briefing WHERE `+q+` ORDER BY seq DESC LIMIT 1`, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return r, nil
}

// LatestBriefing is the latest briefing result of checklist id in repo, any version (nil = none).
func (s *Store) LatestBriefing(repo, id string) (*BriefingResult, error) {
	return s.oneBriefing("latest briefing result", `repo=? AND checklist_id=?`, repo, id)
}

// BriefingFor is the latest briefing result of checklist id in repo run on that version and hash
// (nil = none).
func (s *Store) BriefingFor(repo, id string, version int, sha256 string) (*BriefingResult, error) {
	return s.oneBriefing("briefing result", `repo=? AND checklist_id=? AND version=? AND checklist_sha256=?`, repo, id, version, sha256)
}

// BriefingByJob is the briefing result attempt of a job stored (nil = none).
func (s *Store) BriefingByJob(jobID string, attempt int) (*BriefingResult, error) {
	return s.oneBriefing("briefing result", `job_id=? AND attempt=?`, jobID, attempt)
}
