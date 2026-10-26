package jobs

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// fakeDockerFlaky puts a docker on PATH that logs its arguments, fails its first call, and then lists one
// container of the job named by the caller.
func fakeDockerFlaky(t *testing.T, container string) (log string) {
	t.Helper()
	bin := t.TempDir()
	log = filepath.Join(bin, "calls.log")
	script := "#!/bin/sh\necho \"$@\" >> " + log + "\n" +
		"if [ ! -e " + bin + "/failed-once ]; then : > " + bin + "/failed-once; echo down >&2; exit 1; fi\n" +
		"if [ \"$1\" = ps ]; then echo " + container + "; fi\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func readLog(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

func TestSourcesContainerCleanupPending(t *testing.T) {
	t.Run("[G02] a failed container cleanup leaves containers.pending; a later tick retries it out of the lock", func(t *testing.T) {
		m := newManager(t)
		j := storedJobWithSources(t, m) // job_start refuses sources while G02 is deferred
		if err := m.Store.Finish(j.ID, "", store.StatusCancelled, 0, 0, 0, "test", m.Now()); err != nil {
			t.Fatal(err)
		}
		log := fakeDockerFlaky(t, "aj-"+j.ID+"-x")
		if err := os.MkdirAll(m.JobDir(j.ID), 0o700); err != nil { // the job ran: it has a folder
			t.Fatal(err)
		}
		marker := filepath.Join(m.JobDir(j.ID), containersPendingFile)
		m.settled(j.ID)
		if _, err := os.Stat(marker); err != nil {
			t.Fatalf("settle left no %s: %v", containersPendingFile, err)
		}
		if got := readLog(t, log); got != "" {
			t.Fatalf("settle ran docker under the lifecycle lock: %q", got)
		}
		m.retryContainerCleanups() // docker fails: the marker stays
		if _, err := os.Stat(marker); err != nil {
			t.Fatalf("a failed cleanup dropped the marker: %v", err)
		}
		if ids, err := m.PendingContainerCleanups(); err != nil || !slices.Contains(ids, j.ID) {
			t.Fatalf("pending cleanups = %v, %v; want %s", ids, err, j.ID)
		}
		if rep, err := m.DoctorProcs(false); err != nil || !slices.Contains(rep.PendingContainers, j.ID) {
			t.Fatalf("doctor procs does not report the pending cleanup: %+v, %v", rep.PendingContainers, err)
		}
		if err := m.Tick(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("the retry did not clear the marker: %v", err)
		}
		if got := readLog(t, log); !strings.Contains(got, "rm -f aj-"+j.ID+"-x") {
			t.Fatalf("the retry did not remove the container: %q", got)
		}
		if ids, _ := m.PendingContainerCleanups(); len(ids) != 0 {
			t.Fatalf("pending after the retry: %v", ids)
		}
	})
	t.Run("[G02] the wrapper's cleanup writes the marker when docker fails", func(t *testing.T) {
		dir := t.TempDir()
		fakeDockerFlaky(t, "aj-w-x")
		if cleanupContainers(dir, "w", t.Logf) {
			t.Fatal("cleanup reported success with docker failing")
		}
		if _, err := os.Stat(filepath.Join(dir, containersPendingFile)); err != nil {
			t.Fatalf("no marker: %v", err)
		}
		if !cleanupContainers(dir, "w", t.Logf) {
			t.Fatal("cleanup failed with docker working")
		}
		if _, err := os.Stat(filepath.Join(dir, containersPendingFile)); !os.IsNotExist(err) {
			t.Fatalf("marker left after a cleanup: %v", err)
		}
	})
}

func TestSourcesGitEnvBareRepository(t *testing.T) {
	t.Run("[G02] safe.bareRepository=explicit is set only for a job with sources", func(t *testing.T) {
		has := func(env []string) bool {
			return slices.ContainsFunc(env, func(kv string) bool { return strings.HasSuffix(kv, "=safe.bareRepository") })
		}
		plain := t.TempDir()
		env, err := GitEnv(plain, nil)
		if err != nil {
			t.Fatal(err)
		}
		if has(env) {
			t.Fatal("a job without sources gets safe.bareRepository (git inside .git or a bare dir breaks)")
		}
		withSources := t.TempDir()
		if err := os.Mkdir(filepath.Join(withSources, SourcesDir), 0o700); err != nil {
			t.Fatal(err)
		}
		if env, err = GitEnv(withSources, nil); err != nil {
			t.Fatal(err)
		}
		if !has(env) {
			t.Fatal("a job with sources lacks safe.bareRepository=explicit")
		}
	})
}

func TestSourcesInternalNets(t *testing.T) {
	t.Run("[G02] NAT64, 6to4, benchmark and reserved ranges are not public", func(t *testing.T) {
		for _, a := range []string{"64:ff9b::7f00:1", "2002:7f00:1::1", "198.18.0.1", "198.19.255.1", "240.0.0.1"} {
			if publicAddr(netip.MustParseAddr(a)) {
				t.Errorf("%s: public, want internal", a)
			}
		}
		if !publicAddr(netip.MustParseAddr("140.82.112.3")) {
			t.Error("a public address is refused")
		}
	})
}

func TestSourcesPrefetchBackoff(t *testing.T) {
	t.Skip(g02Off + "the daemon no longer clones")
	t.Run("[G02] a failed sources.json write backs off instead of cloning again every tick", func(t *testing.T) {
		m := newManager(t)
		t.Cleanup(func() { _ = removeTree(filepath.Join(m.StateDir, "jobs")) })
		now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
		m.Now = func() time.Time { return now }
		j := enqueue(t, m, "look at lib", func(r *StartRequest) {
			r.Kind = "research"
			r.Sources = []string{"https://github.com/a/lib"}
		})
		var clones atomic.Int32
		prev := cloneOne
		t.Cleanup(func() { cloneOne = prev })
		dir := m.JobDir(j.ID)
		cloneOne = func(_ context.Context, u, target string, _ int64, _ string) sourceResult {
			if clones.Add(1) == 1 {
				_ = os.Chmod(dir, 0o500) // sources.json cannot be written
			} else {
				_ = os.Chmod(dir, 0o700)
			}
			return sourceResult{URL: u, Name: filepath.Base(target), Path: target}
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		m.prefetchSources()
		if m.sourcesCloned(j) {
			t.Fatal("sources.json written although the folder is read-only")
		}
		_ = os.Chmod(dir, 0o700)
		m.prefetchSources()
		if n := clones.Load(); n != 1 {
			t.Fatalf("cloned %d times within the backoff, want 1", n)
		}
		now = now.Add(sourcesRetryBackoff + time.Second)
		m.prefetchSources()
		if n := clones.Load(); n != 2 || !m.sourcesCloned(j) {
			t.Fatalf("after the backoff: %d clones, sources.json %v", n, m.sourcesCloned(j))
		}
	})
}
