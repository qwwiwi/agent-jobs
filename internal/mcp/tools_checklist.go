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
	// Set by checklist_init / checklist_set only.
	Written       bool     `json:"written,omitempty"`
	BriefWritten  bool     `json:"brief_written,omitempty"`
	VersionBumped bool     `json:"version_bumped,omitempty"`
	PreviousVoid  string   `json:"previous_void,omitempty"`
	Redacted      int      `json:"redacted,omitempty"`
	WaiversAdded  int      `json:"waivers_added,omitempty"`
	Warnings      []string `json:"warnings,omitempty"`
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

func checklistView(st *store.Store, top, main string, c *checklist.Checklist, brief []byte) (ChecklistView, error) {
	last, rec, err := latestApproval(st, main)
	if err != nil {
		return ChecklistView{}, err
	}
	approved, hash, reason := c.Status(brief, rec)
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
	Repo          string                   `json:"repo" jsonschema:"absolute path of the project's git work tree"`
	Rows          []ChecklistRowInput      `json:"rows,omitempty" jsonschema:"rows to add (new id) or replace (existing id); a row stays in the file for good: to retire it, set it to dropped or deferred with the owner's words"`
	Additions     []ChecklistAdditionInput `json:"additions,omitempty" jsonschema:"later owner additions, appended to BRIEF.md under ## Additions in the same operation"`
	Approve       bool                     `json:"approve,omitempty" jsonschema:"the owner approved the checklist as it is after this call: the approval (id, version, hash, BRIEF hash, owner quote) is stored in the agent-jobs store, the only source of approved; the file's approved: <time> owner sha256:<hash> line is informational"`
	ApproveQuote  string                   `json:"approve_quote,omitempty" jsonschema:"with approve: the owner's words approving, verbatim (redacted, stored with the approval); required"`
	ExpectVersion int                      `json:"expect_version,omitempty" jsonschema:"refuse the call unless the checklist is at this version"`
	Waivers       []ChecklistWaiverInput   `json:"waivers,omitempty" jsonschema:"owner waivers per requirement (F7), stored in the agent-jobs store bound to the approved hash and version and the sha; the checklist must be approved after this call"`
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
		v, err := checklistView(d.Manager.Store, top, main, c, brief)
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
			"after the approval) and the owner waivers bound to the current hash.",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in ChecklistRepoInput) (*sdk.CallToolResult, ChecklistView, error) {
		top, main, err := jobs.ChecklistRepo(in.Repo)
		if err != nil {
			return nil, ChecklistView{}, err
		}
		// Read under the lock: never half of a checklist_set's two renames.
		unlock, err := d.Manager.LockChecklist(top)
		if err != nil {
			return nil, ChecklistView{}, err
		}
		defer unlock()
		f, err := checklist.Read(top)
		if err != nil {
			return nil, ChecklistView{}, err
		}
		if f.Checklist == nil {
			return nil, ChecklistView{}, checklist.ErrNoChecklist
		}
		if f.Brief == nil {
			return nil, ChecklistView{}, fmt.Errorf("%s has %s but no %s", top, checklist.ChecklistRel, checklist.BriefRel)
		}
		// Redacted before it is parsed, as checklist_set reads it: a secret put there by hand is never returned.
		text, _ := jobs.Redact(string(f.Checklist))
		c, err := checklist.Parse(text)
		if err != nil {
			return nil, ChecklistView{}, fmt.Errorf("%s: %w", checklist.ChecklistRel, err)
		}
		v, err := checklistView(d.Manager.Store, top, main, c, f.Brief)
		return nil, v, err
	})
}

func addChecklistSet(s *sdk.Server, d Deps) {
	addManagerTool(s, d, &sdk.Tool{
		Name: "checklist_set",
		Description: "Change the repo's brief checklist; the only writer of version, approved and owner waivers. One call: " +
			"rows upserted, owner additions appended to BRIEF.md ## Additions, both files written together (both or neither), " +
			"secrets redacted before writing and hashing. A change of the hashed content (rows incl. Rationale, BRIEF.md) raises " +
			"the version and drops the approval; approve: true records the owner's approval of the result. Waivers need the " +
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
		if res.Brief != nil || res.Checklist != nil {
			if err := checklist.WriteBoth(top, res.Brief, res.Checklist, f.Brief); err != nil {
				return nil, ChecklistView{}, err
			}
		}
		// The approval is stored after the files are written: a failure in between leaves the new content
		// without a stored approval, so not approved (fail closed).
		if r := res.Record; r != nil {
			if err := m.Store.PutApproval(store.ChecklistApproval{Repo: main, ChecklistID: r.ChecklistID, Version: r.Version,
				ChecklistSHA256: r.SHA256, BriefSHA256: r.BriefSHA256, Quote: quote, ApprovedAt: r.At}); err != nil {
				return nil, ChecklistView{}, fmt.Errorf("files written; the approval was not stored, so the checklist is not approved: %w", err)
			}
		}
		added := 0
		for _, w := range waivers {
			if err := m.Store.PutWaiver(w); err != nil {
				return nil, ChecklistView{}, fmt.Errorf("files written; %d of %d waivers stored: %w", added, len(waivers), err)
			}
			added++
		}
		v, err := checklistView(m.Store, top, main, res.List, res.BriefAfter)
		if err != nil {
			return nil, ChecklistView{}, err
		}
		v.Written, v.BriefWritten = res.Brief != nil || res.Checklist != nil, res.Brief != nil
		v.VersionBumped, v.PreviousVoid, v.Redacted, v.Warnings = res.VersionBumped, res.PreviousVoid, res.Redacted, res.Warnings
		v.WaiversAdded = added
		return nil, v, nil
	})
}
