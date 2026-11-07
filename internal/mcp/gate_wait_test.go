package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

// job_list and system_doctor show a running job whose heavy step waits for the box-wide gate lock (#208).
func TestGateWaitShownInJobListAndDoctor(t *testing.T) {
	m := newManager(t)
	m.SelfBin = selfBin
	cs := connect(t, mustServer(t, m))
	var jv JobView
	call(t, cs, "job_start", startArgs(nil), &jv)
	if err := m.Store.Claim(jv.ID, "tok", time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(m.JobDir(jv.ID), 0o700); err != nil {
		t.Fatal(err)
	}
	st := jobs.GateWaitState{Waits: []jobs.GateWaitEntry{{PID: os.Getpid(), Since: time.Now().Add(-20 * time.Second).UTC(),
		Holder: "pid 1 job other since x: npm run build"}}}
	b, _ := json.Marshal(st)
	if err := os.WriteFile(filepath.Join(m.JobDir(jv.ID), jobs.GateWaitFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	var list JobListOutput
	call(t, cs, "job_list", map[string]any{}, &list)
	if len(list.Jobs) != 1 || list.Jobs[0].GateWait == nil {
		t.Fatalf("job_list: %+v", list.Jobs)
	}
	if g := list.Jobs[0].GateWait; !g.Waiting || g.Note != "waiting for gate lock, untrusted holder line: pid 1 job other since x: npm run build" ||
		g.WaitedSec < 19 {
		t.Fatalf("job_list gate_wait: %+v", g)
	}
	var out SystemDoctorOutput
	call(t, cs, "system_doctor", map[string]any{}, &out)
	if w := out.Queue.GateLock.Waiting; len(w) != 1 || w[0].ID != jv.ID || !w[0].Waiting {
		t.Fatalf("system_doctor gate_lock: %+v", out.Queue.GateLock)
	}
}
