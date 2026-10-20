package casefold

import (
	"strings"
	"testing"
)

func TestFold(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"jobs.db", "jobs.db"},
		{"JOBS.DB", "jobs.db"},
		{"JOBſ.DB", "jobs.db"}, // long s
		{"Kelvin", "kelvin"},   // Kelvin sign
		{".GIT", ".git"},
		{"cafÉ", "café"},
		{"Å", "å"},           // Angstrom sign
		{"STRAẞE", "straße"}, // capital sharp s
		{"Σς", "σσ"},
	} {
		if got := Fold(c.in); got != c.want {
			t.Errorf("Fold(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// Fold agrees with strings.EqualFold.
	for _, p := range [][2]string{{"rsync", "RſYNC"}, {"unlink", "UNLINK"}, {"å", "Å"}} {
		if !strings.EqualFold(p[0], p[1]) || Fold(p[0]) != Fold(p[1]) {
			t.Errorf("%q and %q fold apart", p[0], p[1])
		}
	}
	// Characters outside the orbits stay as written: dotless i is not i.
	if Fold("ı") == "i" {
		t.Error("dotless i folded to i")
	}
}
