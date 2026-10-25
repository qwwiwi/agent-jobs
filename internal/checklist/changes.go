package checklist

import "slices"

// RowChange is one row that differs between an approved checklist and the current one: what the owner
// re-reads before approving the new version (F8). Change is added (a row the approval did not have),
// changed (Fields name the contract fields that differ) or removed (a row of the approval the file
// no longer has: only a manual edit removes one).
type RowChange struct {
	ID     string   `json:"id"`
	Change string   `json:"change"`
	Fields []string `json:"fields,omitempty"`
	// Row is the current row (the approved one for removed).
	Row Row `json:"row"`
}

// Row change kinds.
const (
	RowAdded   = "added"
	RowChanged = "changed"
	RowRemoved = "removed"
)

// ChangedRows lists the rows of now that differ from approved in a hashed field (verbatim, contract,
// placeholder-ok, rationale: compared as Hash reads them), then the rows of approved that now lacks,
// in file order.
func ChangedRows(approved, now []Row) []RowChange {
	out := []RowChange{}
	for _, r := range now {
		i := slices.IndexFunc(approved, func(a Row) bool { return a.ID == r.ID })
		if i < 0 {
			out = append(out, RowChange{ID: r.ID, Change: RowAdded, Row: r})
			continue
		}
		a := approved[i]
		var fields []string
		if a.Verbatim != r.Verbatim {
			fields = append(fields, "verbatim")
		}
		if a.Contract != r.Contract {
			fields = append(fields, "contract")
		}
		if a.PlaceholderOK != r.PlaceholderOK {
			fields = append(fields, "placeholder_ok")
		}
		if a.Rationale != r.Rationale {
			fields = append(fields, "rationale")
		}
		if len(fields) > 0 {
			out = append(out, RowChange{ID: r.ID, Change: RowChanged, Fields: fields, Row: r})
		}
	}
	for _, a := range approved {
		if !slices.ContainsFunc(now, func(r Row) bool { return r.ID == a.ID }) {
			out = append(out, RowChange{ID: a.ID, Change: RowRemoved, Row: a})
		}
	}
	return out
}
