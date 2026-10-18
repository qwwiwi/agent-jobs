package mcp

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// owner_replied is orchestrator-only: a job's server neither lists nor serves it.
func TestOwnerRepliedAudience(t *testing.T) {
	if !slices.Contains(OrchestratorOnly, "owner_replied") || slices.Contains(JobAllowed, "owner_replied") {
		t.Fatal("owner_replied must be orchestrator-only")
	}
	for _, register := range []bool{false, true} {
		s, err := newServer(RoleJob, Deps{}, register)
		if err != nil {
			t.Fatal(err)
		}
		cs := connect(t, s)
		if slices.Contains(toolNames(t, cs), "owner_replied") {
			t.Fatal("job role lists owner_replied")
		}
		if msg := callErr(t, cs, "owner_replied", map[string]any{}); !strings.Contains(msg, "is not available to the job role") {
			t.Fatalf("job role call: %q", msg)
		}
	}
}

// job_start owner_waits is stored; the delivered line records the pending reply, system_doctor shows
// it (not as a problem), owner_replied clears one id, refuses an id without a record, and clears all.
func TestOwnerRepliedTool(t *testing.T) {
	m := newManager(t)
	m.SelfBin = selfBin // the guard hook probe of system_doctor runs the real binary
	cs := connect(t, mustServer(t, m))
	var a, b, plain JobView
	call(t, cs, "job_start", startArgs(func(x map[string]any) { x["owner_waits"] = true }), &a)
	call(t, cs, "job_start", startArgs(func(x map[string]any) { x["owner_waits"] = true }), &b)
	call(t, cs, "job_start", startArgs(nil), &plain)
	if !a.OwnerWaits || !b.OwnerWaits || plain.OwnerWaits {
		t.Fatalf("owner_waits: %v %v %v", a.OwnerWaits, b.OwnerWaits, plain.OwnerWaits)
	}
	for _, id := range []string{a.ID, b.ID, plain.ID} {
		if err := m.Store.MarkNotified(id, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	var doc SystemDoctorOutput
	call(t, cs, "system_doctor", map[string]any{}, &doc)
	if len(doc.OwnerPending) != 2 || slices.ContainsFunc(doc.Problems, func(p string) bool { return strings.Contains(p, "owner repl") }) {
		t.Fatalf("doctor: %+v %v", doc.OwnerPending, doc.Problems)
	}
	if doc.OwnerHookSession != "" {
		t.Fatalf("no session bound yet: %q", doc.OwnerHookSession)
	}
	if err := m.Store.BindHookSession("orch-1", time.Now()); err != nil {
		t.Fatal(err)
	}
	call(t, cs, "system_doctor", map[string]any{}, &doc)
	if doc.OwnerHookSession != "orch-1" {
		t.Fatalf("bound session: %q", doc.OwnerHookSession)
	}

	var out OwnerRepliedOutput
	call(t, cs, "owner_replied", map[string]any{"job_id": a.ID}, &out)
	if !slices.Equal(out.Cleared, []string{a.ID}) || len(out.Pending) != 1 || out.Pending[0].JobID != b.ID {
		t.Fatalf("clear one: %+v", out)
	}
	if msg := callErr(t, cs, "owner_replied", map[string]any{"job_id": plain.ID}); !strings.Contains(msg, "no owner reply pending") {
		t.Fatalf("no record: %q", msg)
	}
	call(t, cs, "owner_replied", map[string]any{}, &out)
	if !slices.Equal(out.Cleared, []string{b.ID}) || len(out.Pending) != 0 {
		t.Fatalf("clear all: %+v", out)
	}
}
