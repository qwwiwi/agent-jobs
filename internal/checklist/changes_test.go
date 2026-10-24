package checklist

import (
	"slices"
	"testing"
)

// ChangedRows names the added rows, the changed ones with the hashed fields that differ, and the
// removed ones; an unchanged row is not listed.
func TestChangedRows(t *testing.T) {
	approved := []Row{
		{ID: "R01", Verbatim: "a login page", Contract: "live", Rationale: "brief"},
		{ID: "R02", Verbatim: "dark mode", Contract: "live", Rationale: "brief"},
		{ID: "R03", Verbatim: "export", Contract: "live", Rationale: "brief"},
		{ID: "R04", Verbatim: "same", Contract: "live", Rationale: "brief"},
	}
	now := []Row{
		{ID: "R01", Verbatim: "a login page", Contract: "live", Rationale: "owner: YooKassa, not Stripe"},
		{ID: "R02", Verbatim: "dark mode", Contract: "dropped", Rationale: "owner: «no dark mode»"},
		{ID: "R04", Verbatim: "same", Contract: "live", Rationale: "brief"},
		{ID: "G01", Verbatim: "a CSV export", Contract: "live", PlaceholderOK: true, Rationale: "addition 2026-10-24"},
	}
	got := ChangedRows(approved, now)
	want := []struct {
		id, change string
		fields     []string
	}{
		{"R01", RowChanged, []string{"rationale"}},
		{"R02", RowChanged, []string{"contract", "rationale"}},
		{"G01", RowAdded, nil},
		{"R03", RowRemoved, nil},
	}
	if len(got) != len(want) {
		t.Fatalf("changes: %+v", got)
	}
	for i, w := range want {
		if got[i].ID != w.id || got[i].Change != w.change || !slices.Equal(got[i].Fields, w.fields) {
			t.Fatalf("change %d: %+v, want %+v", i, got[i], w)
		}
	}
	if got[3].Row.Verbatim != "export" || got[2].Row.Verbatim != "a CSV export" {
		t.Fatalf("rows carried: %+v", got)
	}
	if c := ChangedRows(now, now); c == nil || len(c) != 0 {
		t.Fatalf("no change: %+v", c)
	}
}
