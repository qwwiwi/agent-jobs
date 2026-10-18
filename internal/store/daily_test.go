package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestDailyClaimOncePerDay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if d, err := st.GetDaily(); err != nil || d != nil {
		t.Fatalf("fresh store: %+v, %v", d, err)
	}
	now := time.Unix(1_800_000_000, 0)
	if ok, err := st.ClaimDaily("2027-01-15", now); err != nil || !ok {
		t.Fatalf("first claim: %v, %v", ok, err)
	}
	if ok, err := st.ClaimDaily("2027-01-15", now.Add(time.Hour)); err != nil || ok {
		t.Fatalf("second claim of the same day: %v, %v", ok, err)
	}
	if err := st.FinishDaily("2027-01-15", now.Add(time.Minute), `{"x":1}`); err != nil {
		t.Fatal(err)
	}
	// A reopened store (daemon restart) keeps the day.
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if st, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if ok, err := st.ClaimDaily("2027-01-15", now.Add(2*time.Hour)); err != nil || ok {
		t.Fatalf("claim after reopen: %v, %v", ok, err)
	}
	d, err := st.GetDaily()
	if err != nil || d == nil || d.Day != "2027-01-15" || d.FinishedAt == nil || d.Result != `{"x":1}` ||
		!d.StartedAt.Equal(now) {
		t.Fatalf("daily row: %+v, %v", d, err)
	}
	if ok, err := st.ClaimDaily("2027-01-16", now.Add(24*time.Hour)); err != nil || !ok {
		t.Fatalf("next day: %v, %v", ok, err)
	}
	// A late finish of the previous day never overwrites the new claim.
	if err := st.FinishDaily("2027-01-15", now.Add(25*time.Hour), `{"old":1}`); err != nil {
		t.Fatal(err)
	}
	if d, err = st.GetDaily(); err != nil || d.Day != "2027-01-16" || d.FinishedAt != nil || d.Result != "" {
		t.Fatalf("after the next claim: %+v, %v", d, err)
	}
}

// A clock or TZ step backwards never claims (re-runs) a day earlier than the stored one.
func TestDailyClaimNeverGoesBack(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Unix(1_800_000_000, 0)
	if ok, err := st.ClaimDaily("2027-01-16", now); err != nil || !ok {
		t.Fatalf("claim: %v, %v", ok, err)
	}
	if ok, err := st.ClaimDaily("2027-01-15", now.Add(time.Minute)); err != nil || ok {
		t.Fatalf("claim of an earlier day: %v, %v", ok, err)
	}
	if d, err := st.GetDaily(); err != nil || d.Day != "2027-01-16" {
		t.Fatalf("daily row: %+v, %v", d, err)
	}
}
