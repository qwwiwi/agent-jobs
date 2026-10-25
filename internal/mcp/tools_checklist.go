package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qwwiwi/agent-jobs/internal/checklist"
	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// ChecklistView is a repository's brief checklist (Wave F, F1/F2) as the checklist tools return it.
type ChecklistView struct {
	Repo          string `json:"repo"`
	BriefPath     string `json:"brief_path"`
	ChecklistPath string `json:"checklist_path"`
	ID            string `json:"id"`
	Version       int    `json:"version"`
	// Approved: the repository's latest approval stored by checklist_set approve names the current hash,
	// version and id (the approved line in the file is informational); Reason says why not. ApprovedAt,
	// ApprovedVersion and ApprovedSHA256 are that stored approval, whether it still holds or not.
	Approved        bool   `json:"approved"`
	ApprovedAt      string `json:"approved_at,omitempty"`
	ApprovedVersion int    `json:"approved_version,omitempty"`
	ApprovedSHA256  string `json:"approved_sha256,omitempty"`
	Reason          string `json:"reason,omitempty"`
	// SHA256 is the hash of the files as they are now; BriefSHA256 the sha256 of BRIEF.md in it.
	SHA256      string          `json:"sha256"`
	BriefSHA256 string          `json:"brief_sha256"`
	Rows        []checklist.Row `json:"rows"`
	// Live are the live requirements (R, Ri, G with contract live): only they count in the 100%.
	Live []string `json:"live"`
	// Waivers are the owner waivers bound to the approved hash and version (any sha); none while the
	// checklist is not approved.
	Waivers []store.Waiver `json:"waivers"`
	// Slicing is the slicing check (F3) for the current version and hash (current true), else the
	// latest one of the checklist id (current false: it does not count); null when none ran.
	Slicing *jobs.SlicingSummary `json:"slicing"`
	// ChangedSinceApproval is what differs from the repository's latest approval (the one approved
	// reads), when that approval is of this checklist id and the hash is not the approved one: the owner
	// re-reads and re-approves only these rows (F8). Null otherwise (the hash is the approved one, or the
	// latest approval is of another id or there is none).
	ChangedSinceApproval *ApprovalChanges `json:"changed_since_approval"`
	// Set by checklist_init / checklist_set only.
	Written        bool     `json:"written,omitempty"`
	BriefWritten   bool     `json:"brief_written,omitempty"`
	VersionBumped  bool     `json:"version_bumped,omitempty"`
	PreviousVoid   string   `json:"previous_void,omitempty"`
	Redacted       int      `json:"redacted,omitempty"`
	WaiversAdded   int      `json:"waivers_added,omitempty"`
	DecisionsAdded int      `json:"unmapped_decisions_added,omitempty"`
	Warnings       []string `json:"warnings,omitempty"`
}

// ApprovalChanges is the difference between the checklist as approved and as it is now.
type ApprovalChanges struct {
	ApprovedVersion int `json:"approved_version"`
	// Known is false for an approval stored before its rows were recorded: then every row is to be
	// re-read, and Rows and BriefChanged say nothing.
	Known bool                  `json:"known"`
	Rows  []checklist.RowChange `json:"rows"`
	// BriefChanged: BRIEF.md differs from the approved one (an Addition since the approval).
	BriefChanged bool `json:"brief_changed"`
}

// approvalChanges compares checklist c with the repository's latest approval last (nil when there is
// none, when last approves the same hash or when it is of another checklist id).
func approvalChanges(last *store.ChecklistApproval, c *checklist.Checklist, brief []byte, hash string) *ApprovalChanges {
	if last == nil || last.ChecklistID != c.ID || last.ChecklistSHA256 == hash {
		return nil
	}
	out := &ApprovalChanges{ApprovedVersion: last.Version, Rows: []checklist.RowChange{}}
	if last.Rows == nil || last.BriefFileSHA256 == "" {
		return out
	}
	approved := make([]checklist.Row, 0, len(last.Rows))
	for _, r := range last.Rows {
		approved = append(approved, checklist.Row{ID: r.ID, Verbatim: r.Verbatim, Contract: r.Contract,
			PlaceholderOK: r.PlaceholderOK, Rationale: r.Rationale})
	}
	out.Known, out.Rows = true, checklist.ChangedRows(approved, c.Rows)
	out.BriefChanged = last.BriefFileSHA256 != checklist.SHA256Hex(brief)
	return out
}

// approvedRows are rows as an approval records them.
func approvedRows(rows []checklist.Row) []store.GateRow {
	out := make([]store.GateRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, store.GateRow{ID: r.ID, Verbatim: r.Verbatim, Contract: r.Contract, PlaceholderOK: r.PlaceholderOK,
			Rationale: r.Rationale})
	}
	return out
}

// latestApproval is the repository's latest stored approval, as stored and as a checklist.Record (both
// nil when there is none).
func latestApproval(st *store.Store, main string) (*store.ChecklistApproval, *checklist.Record, error) {
	a, err := st.LatestApproval(main)
	if err != nil || a == nil {
		return nil, nil, err
	}
	return a, &checklist.Record{ChecklistID: a.ChecklistID, Version: a.Version, SHA256: a.ChecklistSHA256,
		BriefSHA256: a.BriefSHA256, At: a.ApprovedAt}, nil
}

// binding is the store's binding of checklist id in the repository main (nil when it has none).
func binding(st *store.Store, main, id string) (*checklist.Binding, error) {
	b, err := st.Binding(main, id)
	if err != nil || b == nil {
		return nil, err
	}
	return &checklist.Binding{BriefSHA256: b.BriefSHA256, Approved: b.Approved}, nil
}

func checklistView(m *jobs.Manager, top, main string, c *checklist.Checklist, brief []byte) (ChecklistView, error) {
	st := m.Store
	approved, hash, reason, last, err := m.ChecklistStatus(main, c, brief)
	if err != nil {
		return ChecklistView{}, err
	}
	v := ChecklistView{Repo: top, BriefPath: checklist.BriefPath(top), ChecklistPath: checklist.ChecklistPath(top),
		ID: c.ID, Version: c.Version, Approved: approved, Reason: reason, SHA256: hash,
		BriefSHA256: checklist.SHA256Hex(brief), Rows: c.Rows, Live: c.LiveIDs()}
	if v.Rows == nil {
		v.Rows = []checklist.Row{}
	}
	if v.Live == nil {
		v.Live = []string{}
	}
	if last != nil {
		v.ApprovedAt, v.ApprovedVersion, v.ApprovedSHA256 = last.ApprovedAt.UTC().Format(time.RFC3339), last.Version, last.ChecklistSHA256
	}
	v.ChangedSinceApproval = approvalChanges(last, c, brief, hash)
	sl, err := m.SlicingOf(main, c.ID, c.Version, hash)
	if err != nil {
		return ChecklistView{}, err
	}
	if sl != nil {
		v.Slicing = sl.Summary
	}
	v.Waivers = []store.Waiver{}
	if approved {
		w, err := st.Waivers(main, last.ChecklistSHA256, last.Version)
		if err != nil {
			return ChecklistView{}, err
		}
		if w != nil {
			v.Waivers = w
		}
	}
	return v, nil
}

// ChecklistInitInput is checklist_init's input.
type ChecklistInitInput struct {
	Repo  string `json:"repo" jsonschema:"absolute path of the project's git work tree; brief/BRIEF.md and brief/CHECKLIST.md are created there (never committed)"`
	ID    string `json:"id,omitempty" jsonschema:"checklist id; default the repository's directory name"`
	Brief string `json:"brief" jsonschema:"the owner's words verbatim (voice transcript or text); secrets are redacted before writing"`
}

// ChecklistRepoInput is checklist_get's input.
type ChecklistRepoInput struct {
	Repo string `json:"repo" jsonschema:"absolute path of the project's git work tree"`
}

// ChecklistRowInput is one row of checklist_set.
type ChecklistRowInput struct {
	ID            string `json:"id" jsonschema:"R<nn> (from the brief), R<nn>i (implied, the owner approves it), G<nn> (added later by the owner, needs an addition in the same call) or D<nn> (a constraint proven by a build, not counted)"`
	Verbatim      string `json:"verbatim" jsonschema:"the owner's words this row stands for, verbatim"`
	Contract      string `json:"contract" jsonschema:"live | deferred | dropped; deferred and dropped are the owner's call and need the owner's words in rationale"`
	PlaceholderOK bool   `json:"placeholder_ok,omitempty" jsonschema:"a visible placeholder is an accepted result for this row"`
	Rationale     string `json:"rationale,omitempty" jsonschema:"why the row exists: the owner's fork decisions, the drop or defer quote (hashed)"`
}

// ChecklistAdditionInput is one later owner addition to BRIEF.md.
type ChecklistAdditionInput struct {
	Quote string `json:"quote" jsonschema:"the owner's words verbatim"`
	Date  string `json:"date,omitempty" jsonschema:"YYYY-MM-DD; default today (UTC)"`
}

// ChecklistWaiverInput is one owner waiver (F7).
type ChecklistWaiverInput struct {
	Requirement string `json:"requirement" jsonschema:"a live requirement of the approved checklist"`
	SHA         string `json:"sha" jsonschema:"the full commit id the waiver is for"`
	Quote       string `json:"quote" jsonschema:"the owner's words waiving it, verbatim"`
}

// ChecklistSetInput is checklist_set's input.
type ChecklistSetInput struct {
	Repo              string                   `json:"repo" jsonschema:"absolute path of the project's git work tree"`
	Rows              []ChecklistRowInput      `json:"rows,omitempty" jsonschema:"rows to add (new id) or replace (existing id); a row stays in the file for good: to retire it, set it to dropped or deferred with the owner's words"`
	Additions         []ChecklistAdditionInput `json:"additions,omitempty" jsonschema:"later owner additions, appended to BRIEF.md under ## Additions in the same operation"`
	Approve           bool                     `json:"approve,omitempty" jsonschema:"the owner approved the checklist as it is after this call: the approval (id, version, hash, BRIEF hash, owner quote) is stored in the agent-jobs store, the only source of approved; the file's approved: <time> owner sha256:<hash> line is informational"`
	ApproveQuote      string                   `json:"approve_quote,omitempty" jsonschema:"with approve: the owner's words approving, verbatim (redacted, stored with the approval); required"`
	ExpectVersion     int                      `json:"expect_version,omitempty" jsonschema:"refuse the call unless the checklist is at this version"`
	Waivers           []ChecklistWaiverInput   `json:"waivers,omitempty" jsonschema:"owner waivers per requirement (F7), stored in the agent-jobs store bound to the approved hash and version and the sha; the checklist must be approved after this call"`
	UnmappedDecisions []ChecklistDecisionInput `json:"unmapped_decisions,omitempty" jsonschema:"the owner's decision per unmapped brief quote of the slicing check (F3) for the checklist's version and hash after this call, stored in the agent-jobs store; approve needs one for every unmapped quote of that result"`
}

// ChecklistDecisionInput is the owner's decision on one unmapped quote of the slicing check (F3).
type ChecklistDecisionInput struct {
	Unmapped string `json:"unmapped" jsonschema:"the unmapped quote as slicing_get lists it"`
	Quote    string `json:"quote" jsonschema:"the owner's words deciding it (out of scope, covered by R03, …), verbatim"`
}

func addChecklistInit(s *sdk.Server, d Deps) {
	addManagerTool(s, d, &sdk.Tool{
		Name: "checklist_init",
		Description: "Start the brief flow of a LARGE task (Wave F): create brief/BRIEF.md (the owner's words verbatim, " +
			"secrets redacted) and an empty, unapproved brief/CHECKLIST.md (version 1) in the repo's work tree. " +
			"Refused when either file exists. Writes files only: never commits or pushes.",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in ChecklistInitInput) (*sdk.CallToolResult, ChecklistView, error) {
		top, main, err := jobs.ChecklistRepo(in.Repo)
		if err != nil {
			return nil, ChecklistView{}, err
		}
		unlock, err := d.Manager.LockChecklist(top)
		if err != nil {
			return nil, ChecklistView{}, err
		}
		defer unlock()
		f, err := checklist.Read(top)
		if err != nil {
			return nil, ChecklistView{}, err
		}
		if f.Brief != nil || f.Checklist != nil {
			return nil, ChecklistView{}, fmt.Errorf("%s already has %s or %s: use checklist_get / checklist_set",
				top, checklist.BriefRel, checklist.ChecklistRel)
		}
		id := in.ID
		if strings.TrimSpace(id) == "" {
			id = filepath.Base(top)
		}
		brief, list, n, err := checklist.Init(id, in.Brief, jobs.Redact)
		if err != nil {
			return nil, ChecklistView{}, err
		}
		c, err := checklist.Parse(string(list))
		if err != nil {
			return nil, ChecklistView{}, err
		}
		// A checklist id is bound to the BRIEF it was created with: an id created (or approved) in this
		// repository before is never reused, a new brief takes a new id.
		if b, err := d.Manager.Store.Binding(main, c.ID); err != nil {
			return nil, ChecklistView{}, err
		} else if b != nil {
			return nil, ChecklistView{}, fmt.Errorf("checklist %q was created in this repository before: a new brief takes a new id", c.ID)
		}
		if err := checklist.WriteBoth(top, brief, list, nil); err != nil {
			return nil, ChecklistView{}, err
		}
		// The init record binds the id to the BRIEF prefix; without it the files are removed again (they
		// did not exist before the call, and the lock is held), so a failed init leaves nothing behind.
		if err := d.Manager.Store.PutInit(store.ChecklistInit{Repo: main, ChecklistID: c.ID,
			BriefSHA256: checklist.BriefPrefixSHA256(brief), CreatedAt: d.Manager.Now()}); err != nil {
			_ = os.Remove(checklist.ChecklistPath(top))
			_ = os.Remove(checklist.BriefPath(top))
			return nil, ChecklistView{}, fmt.Errorf("checklist_init: the init record was not stored, nothing written: %w", err)
		}
		v, err := checklistView(d.Manager, top, main, c, brief)
		if err != nil {
			return nil, ChecklistView{}, err
		}
		v.Written, v.BriefWritten, v.Redacted = true, true, n
		return nil, v, nil
	})
}

func addChecklistGet(s *sdk.Server, d Deps) {
	addManagerTool(s, d, &sdk.Tool{
		Name: "checklist_get",
		Description: "The repo's brief checklist: header, rows, live requirements, the hash of the files as they are now, " +
			"approved (false with a reason when not approved, or when a hashed field, a Rationale or BRIEF.md was edited " +
			"after the approval), the owner waivers bound to the current hash and slicing: the slicing check (F3) " +
			"{version, sha256, current, job_id, unmapped, half, invented, decided, undecided, at} (null when none ran), and " +
			"changed_since_approval: the rows added, changed (which fields) or removed since the repository's latest approval and " +
			"whether BRIEF.md changed (null when the hash is the approved one, or that approval is of another id or absent): the owner " +
			"re-approves only those rows; with known false (an approval stored before its rows were recorded) rows is empty and " +
			"the owner re-reads every current row.",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in ChecklistRepoInput) (*sdk.CallToolResult, ChecklistView, error) {
		top, main, err := jobs.ChecklistRepo(in.Repo)
		if err != nil {
			return nil, ChecklistView{}, err
		}
		v, err := currentChecklist(d.Manager, top, main)
		return nil, v, err
	})
}

// currentChecklist is the checklist of the work tree top as checklist_get returns it, read under the lock
// (never half of a checklist_set's two renames).
func currentChecklist(m *jobs.Manager, top, main string) (ChecklistView, error) {
	unlock, err := m.LockChecklist(top)
	if err != nil {
		return ChecklistView{}, err
	}
	defer unlock()
	f, err := checklist.Read(top)
	if err != nil {
		return ChecklistView{}, err
	}
	if f.Checklist == nil {
		return ChecklistView{}, checklist.ErrNoChecklist
	}
	if f.Brief == nil {
		return ChecklistView{}, fmt.Errorf("%s has %s but no %s", top, checklist.ChecklistRel, checklist.BriefRel)
	}
	// Redacted before it is parsed, as checklist_set reads it: a secret put there by hand is never returned.
	text, _ := jobs.Redact(string(f.Checklist))
	c, err := checklist.Parse(text)
	if err != nil {
		return ChecklistView{}, fmt.Errorf("%s: %w", checklist.ChecklistRel, err)
	}
	return checklistView(m, top, main, c, f.Brief)
}

func addChecklistSet(s *sdk.Server, d Deps) {
	addManagerTool(s, d, &sdk.Tool{
		Name: "checklist_set",
		Description: "Change the repo's brief checklist; the only writer of version, approved and owner waivers. One call: " +
			"rows upserted, owner additions appended to BRIEF.md ## Additions, both files written together (both or neither), " +
			"secrets redacted before writing and hashing. A change of the hashed content (rows incl. Rationale, BRIEF.md) raises " +
			"the version and drops the approval; approve: true records the owner's approval of the result, refused without a " +
			"slicing check (slicing_start) of that version and hash held to its input (a codex one does not count) or while one of " +
			"its unmapped quotes has no owner decision (unmapped_decisions); an approval a later slicing check voided is restored " +
			"only by a new approve, not by decisions alone. Owner replies to a briefing (F8): new scope is a G<nn> row with the " +
			"owner's words in additions (dated) in the same call; a choice goes into the Rationale of the row it concerns; a " +
			"cancellation sets the row to dropped with the owner's words in rationale; each raises the version, and " +
			"changed_since_approval lists the rows the owner re-approves (with known false, every current row). Waivers need the " +
			"checklist approved after the call. Warns on fewer than 10 rows from a long brief. Writes files only: never commits or pushes.",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in ChecklistSetInput) (*sdk.CallToolResult, ChecklistView, error) {
		m := d.Manager
		top, main, err := jobs.ChecklistRepo(in.Repo)
		if err != nil {
			return nil, ChecklistView{}, err
		}
		unlock, err := m.LockChecklist(top)
		if err != nil {
			return nil, ChecklistView{}, err
		}
		defer unlock()
		f, err := checklist.Read(top)
		if err != nil {
			return nil, ChecklistView{}, err
		}
		ch := checklist.Change{Approve: in.Approve, ExpectVersion: in.ExpectVersion}
		for _, r := range in.Rows {
			ch.Rows = append(ch.Rows, checklist.Row{ID: r.ID, Verbatim: r.Verbatim, Contract: r.Contract,
				PlaceholderOK: r.PlaceholderOK, Rationale: r.Rationale})
		}
		for _, a := range in.Additions {
			ch.Additions = append(ch.Additions, checklist.Addition{Quote: a.Quote, Date: a.Date})
		}
		var quote string
		quoteRedacted := 0
		if in.Approve {
			quote, quoteRedacted = jobs.Redact(in.ApproveQuote) // raw, before any trimming
			if quote = strings.TrimSpace(quote); quote == "" {
				return nil, ChecklistView{}, errors.New("approve needs approve_quote: the owner's words approving, verbatim")
			}
		}
		_, last, err := latestApproval(m.Store, main)
		if err != nil {
			return nil, ChecklistView{}, err
		}
		now := m.Now()
		res, err := checklist.Apply(f, ch, now, jobs.Redact, last, func(id string) (*checklist.Binding, error) {
			return binding(m.Store, main, id)
		})
		if err != nil {
			return nil, ChecklistView{}, err
		}
		res.Redacted += quoteRedacted
		// Waivers are checked against the result before anything is written.
		var waivers []store.Waiver
		for i, w := range in.Waivers {
			if !res.Approved {
				return nil, ChecklistView{}, fmt.Errorf("waivers need an approved checklist after this call (%s)", res.Reason)
			}
			row, ok := res.List.Row(strings.TrimSpace(w.Requirement))
			if !ok || !row.Live() {
				return nil, ChecklistView{}, fmt.Errorf("waiver %d: %q is not a live requirement of the checklist", i+1, w.Requirement)
			}
			if err := jobs.CheckCommit(main, strings.TrimSpace(w.SHA)); err != nil {
				return nil, ChecklistView{}, fmt.Errorf("waiver %d: %w", i+1, err)
			}
			quote, n := jobs.Redact(w.Quote) // raw, before any trimming
			if quote = strings.TrimSpace(quote); quote == "" {
				return nil, ChecklistView{}, fmt.Errorf("waiver %d: the owner's quote is empty", i+1)
			}
			res.Redacted += n
			waivers = append(waivers, store.Waiver{Repo: main, ChecklistID: res.List.ID, ChecklistSHA256: res.Hash,
				ChecklistVersion: res.List.Version, SHA: strings.TrimSpace(w.SHA), Requirement: row.ID, Quote: quote, CreatedAt: now})
		}
		// The slicing rule (F3): checked against the result before anything is written.
		var given []jobs.UnmappedDecision
		for _, u := range in.UnmappedDecisions {
			given = append(given, jobs.UnmappedDecision{Unmapped: u.Unmapped, Quote: u.Quote})
		}
		decisions, n, err := m.SlicingGate(main, res.List.ID, res.List.Version, res.Hash, in.Approve, given, now)
		if err != nil {
			if res.VersionBumped {
				// The slicing check reads the files as written: the new version must be on disk first.
				err = fmt.Errorf("%w; this call raises the version (a change or a void approval line): call checklist_set "+
					"without approve and unmapped_decisions first, then slicing_start on the version it writes", err)
			}
			return nil, ChecklistView{}, err
		}
		res.Redacted += n
		if len(waivers) > 0 && !in.Approve {
			// Waivers bind to the approved version and hash: not to an approval a later slicing result voided.
			void, err := m.SlicingVoid(main, res.List.ID, res.List.Version, res.Hash, decisions...)
			if err != nil {
				return nil, ChecklistView{}, err
			}
			if void != "" {
				return nil, ChecklistView{}, fmt.Errorf("waivers need an approved checklist after this call (%s)", void)
			}
		}
		if res.Brief != nil || res.Checklist != nil {
			if err := checklist.WriteBoth(top, res.Brief, res.Checklist, f.Brief); err != nil {
				return nil, ChecklistView{}, err
			}
		}
		// The decisions are stored before the approval: one that fails leaves the checklist not approved.
		for i, dec := range decisions {
			if err := m.Store.PutSlicingDecision(dec); err != nil {
				return nil, ChecklistView{}, fmt.Errorf("files written; %d of %d unmapped decisions stored, the approval was not: %w",
					i, len(decisions), err)
			}
		}
		// The approval is stored after the files are written: a failure in between leaves the new content
		// without a stored approval, so not approved (fail closed).
		if r := res.Record; r != nil {
			if err := m.Store.PutApproval(store.ChecklistApproval{Repo: main, ChecklistID: r.ChecklistID, Version: r.Version,
				ChecklistSHA256: r.SHA256, BriefSHA256: r.BriefSHA256, Quote: quote, ApprovedAt: r.At,
				Rows: approvedRows(res.List.Rows), BriefFileSHA256: checklist.SHA256Hex(res.BriefAfter)}); err != nil {
				return nil, ChecklistView{}, fmt.Errorf("files written; the approval was not stored, so the checklist is not approved: %w", err)
			}
			// The merge hook (F9 (b)) matches the gated repository by these names once its work tree is gone.
			if err := m.RecordGateNames(main); err != nil {
				res.Warnings = append(res.Warnings, "approved; the remote names for the merge gate were not stored "+
					"(release_check stores them on a pass, the daemon's backfill at its next start; the merge hook only reads them): "+jobs.RedactPrintable(err.Error(), 200))
			}
		}
		added := 0
		for _, w := range waivers {
			if err := m.Store.PutWaiver(w); err != nil {
				return nil, ChecklistView{}, fmt.Errorf("files written; %d of %d waivers stored: %w", added, len(waivers), err)
			}
			added++
		}
		if res.Brief != nil || res.Checklist != nil || res.Record != nil {
			// A milestone tied to this checklist shows its rows (uncovered, acceptance): its change cursor moves.
			if err := m.Store.TouchChecklistMilestones(main, res.List.ID); err != nil {
				res.Warnings = append(res.Warnings, "the milestones tied to the checklist were not marked changed: "+
					jobs.RedactPrintable(err.Error(), 200))
			}
		}
		v, err := checklistView(m, top, main, res.List, res.BriefAfter)
		if err != nil {
			return nil, ChecklistView{}, err
		}
		v.Written, v.BriefWritten = res.Brief != nil || res.Checklist != nil, res.Brief != nil
		v.VersionBumped, v.PreviousVoid, v.Redacted, v.Warnings = res.VersionBumped, res.PreviousVoid, res.Redacted, res.Warnings
		v.WaiversAdded, v.DecisionsAdded = added, len(decisions)
		return nil, v, nil
	})
}
