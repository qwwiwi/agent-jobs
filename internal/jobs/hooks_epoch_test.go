package jobs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// #188: an emission planned in the previous context but committed after a newer epoch invalidated it
// must not recreate the suppression. Ordering: A holds an emission (output done, Commit not yet run);
// compaction bind B times out on A's lock and drops the record without it; B's reminder times out too
// and its stdout fails (Release); A commits; a fresh hook must emit the snapshot.
func TestHookStaleEmissionCommitDoesNotRecreateSuppression(t *testing.T) {
	for _, tc := range []struct {
		name     string
		seedFile bool // a record is on disk when A takes the lock
	}{
		{"record on disk", true},
		{"no record on disk", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newManager(t)
			const sid, hash = "s1", "h1"
			if tc.seedFile {
				seed := m.planHook(sid, hookKindOutcome, "h0")
				if seed == nil {
					t.Fatal("seed emission: nil")
				}
				if err := seed.Commit(); err != nil {
					t.Fatal(err)
				}
			}
			a := m.planHook(sid, hookKindOutcome, hash) // A: previous context, holds the lock
			if a == nil || a.tx == nil {
				t.Fatalf("A: %+v, want a held emission", a)
			}
			old := hookLockTries
			hookLockTries = 1 // B's lock attempts time out at once
			t.Cleanup(func() { hookLockTries = old })

			if err := m.StartHookEpoch(sid); err != nil { // B: bind times out, drops the record
				t.Fatalf("StartHookEpoch: %v", err)
			}
			b := m.planHook(sid, hookKindOutcome, hash) // B's reminder: no lock, no transaction
			if b == nil || b.tx != nil {
				t.Fatalf("B: %+v, want an untracked emission", b)
			}
			b.Release() // B's stdout failed

			if err := a.Commit(); err != nil { // A resumes and commits the previous context
				t.Fatalf("A.Commit: %v", err)
			}
			hookLockTries = old

			f := m.planHook(sid, hookKindOutcome, hash) // a fresh hook
			if f == nil {
				t.Fatal("fresh hook suppressed after a stale commit: the post-compaction snapshot is lost")
			}
			if err := f.Commit(); err != nil {
				t.Fatal(err)
			}
			if again := m.planHook(sid, hookKindOutcome, hash); again != nil { // the delivered snapshot dedupes again
				t.Fatal("emission after a delivered snapshot: dedup is broken")
			}
			before := m.readHookInvalid()
			if before == "" {
				t.Fatal("no invalidation token after the timed-out reset")
			}
			if err := m.StartHookEpoch(sid); err != nil { // a bind that gets the lock keeps the token
				t.Fatal(err)
			}
			if got := m.readHookInvalid(); got != before {
				t.Errorf("invalidation token after a successful epoch start = %q, want %q kept", got, before)
			}
			if _, err := os.Stat(filepath.Join(m.StateDir, hookInvalidFile)); errors.Is(err, os.ErrNotExist) {
				t.Error("invalidation token file removed by a successful epoch start")
			}
			if again := m.planHook(sid, hookKindOutcome, hash); again == nil { // the reset forces one more emission
				t.Error("no emission after a successful epoch start")
			} else {
				again.Release()
			}
		})
	}
}

// #188: reset C saves its epoch and unlocks, then pauses before its cleanup. Meanwhile emission A takes the
// lock (it reads no token) and prints; reset B times out on the lock, publishes token T and drops the
// records; B's reminder fails to print. C resumes: it must not unlink T, or A's commit (token "") would
// match a fresh hook and suppress the required snapshot.
func TestHookDelayedEpochCleanupKeepsNewerInvalidationToken(t *testing.T) {
	m := newManager(t)
	const sid, hash = "s1", "h1"
	var a *HookEmission
	old := hookLockTries
	t.Cleanup(func() { hookLockTries = old; hookAfterEpochSave = nil })
	hookAfterEpochSave = func() {
		hookAfterEpochSave = nil                   // only C pauses
		a = m.planHook(sid, hookKindOutcome, hash) // A: holds the lock, token ""
		if a == nil || a.tx == nil {
			t.Errorf("A: %+v, want a held emission", a)
			return
		}
		hookLockTries = 1
		if err := m.StartHookEpoch(sid); err != nil { // B: lock timeout, publishes T, drops the records
			t.Errorf("B StartHookEpoch: %v", err)
		}
		b := m.planHook(sid, hookKindOutcome, hash) // B's reminder: untracked, stdout fails
		if b == nil || b.tx != nil {
			t.Errorf("B reminder: %+v, want an untracked emission", b)
		} else {
			b.Release()
		}
		hookLockTries = old
	}
	if err := m.StartHookEpoch(sid); err != nil { // C
		t.Fatalf("C StartHookEpoch: %v", err)
	}
	if a == nil {
		t.Fatal("seam did not run")
	}
	if err := a.Commit(); err != nil { // A resumes with the old context
		t.Fatalf("A.Commit: %v", err)
	}
	f := m.planHook(sid, hookKindOutcome, hash) // a fresh hook
	if f == nil {
		t.Fatal("fresh hook suppressed: C's delayed cleanup removed B's token and A's stale record matches")
	}
	f.Release()
}
