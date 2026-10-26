package jobs

import (
	"context"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// Fix loop 1 of the review of 0ac658d (G02): an embedded bare repository, codex jobs, clones under the
// lifecycle lock, internal network reach, containers that outlive the job, clones too deep to remove.

// repoWith makes a bare repository with one commit of files (path -> body) and returns its path.
func repoWith(t *testing.T, files map[string]string) string {
	t.Helper()
	src := t.TempDir()
	gitT(t, src, "init", "-q", "-b", "main")
	for name, body := range files {
		p := filepath.Join(src, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitT(t, src, "add", "-A")
	gitT(t, src, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "one")
	bare := filepath.Join(t.TempDir(), "r.git")
	gitT(t, t.TempDir(), "clone", "-q", "--bare", src, bare)
	return bare
}

// embeddedBareRepo is a repository whose folder bin/ is laid out as a bare repository (HEAD, objects,
// refs) whose config sets a diff textconv that touches marker, for every file (info/attributes).
func embeddedBareRepo(t *testing.T, marker string) string {
	t.Helper()
	inner := t.TempDir()
	gitT(t, inner, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(inner, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, inner, "add", "a.txt")
	gitT(t, inner, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "inner")
	files := map[string]string{"README.md": "outer\n",
		"bin/config":          "[core]\n\trepositoryformatversion = 0\n\tbare = true\n[diff \"x\"]\n\ttextconv = \"touch " + marker + "; cat\"\n",
		"bin/info/attributes": "* diff=x\n", "bin/HEAD": "ref: refs/heads/main\n"}
	gitDir := filepath.Join(inner, ".git")
	for _, sub := range []string{"objects", "refs"} {
		err := filepath.WalkDir(filepath.Join(gitDir, sub), func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, err := os.ReadFile(p)
			rel, _ := filepath.Rel(gitDir, p)
			files["bin/"+rel] = string(b)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return repoWith(t, files)
}

func TestSourcesEmbeddedBareRepo(t *testing.T) {
	t.Run("[G02] git in a clone's folder laid out as a bare repository runs none of its config", func(t *testing.T) {
		marker := filepath.Join(t.TempDir(), "textconv-ran")
		const url = "https://git.example.test/org/lib.git"
		withLocalRemote(t, map[string]string{url: embeddedBareRepo(t, marker)})
		target := filepath.Join(t.TempDir(), "lib")
		t.Cleanup(func() { _ = removeTree(target) })
		if res := cloneSource(context.Background(), url, target, 1<<20, t.TempDir()); res.Err != "" {
			t.Fatal(res.Err)
		}
		jobDir := t.TempDir()
		if err := os.Mkdir(filepath.Join(jobDir, SourcesDir), 0o700); err != nil { // a job with sources
			t.Fatal(err)
		}
		env, err := GitEnv(jobDir, os.Environ())
		if err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"-C", filepath.Join(target, "bin"), "log", "-p"}, {"log", "-p"}} {
			cmd := exec.Command("git", args...)
			cmd.Dir, cmd.Env = filepath.Join(target, "bin"), env
			out, _ := cmd.CombinedOutput()
			if _, err := os.Stat(marker); err == nil {
				t.Fatalf("git %v ran the embedded repository's textconv:\n%s", args, out)
			}
		}
		// The clone root itself stays readable through git.
		cmd := exec.Command("git", "-C", target, "log", "-p")
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(out), "outer") {
			t.Fatalf("git log at the clone root: %v\n%s", err, out)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatal("git log at the clone root ran the textconv")
		}
	})
}

func TestSourcesCodexRefused(t *testing.T) {
	m := newCodexManager(t)
	const url = "https://github.com/a/lib"
	t.Run("[G02] a codex job with sources is refused: the guard does not run inside codex", func(t *testing.T) {
		_, err := m.Enqueue(StartRequest{Title: "t", Brief: "b", Kind: "review", Model: "gpt-6.1-sol", Sources: []string{url}})
		if err == nil || !strings.Contains(err.Error(), "sources need the guard, which does not run inside codex") {
			t.Fatalf("codex job with sources: %v", err)
		}
	})
	t.Run("[G02] a codex job on a task with planned sources is refused unless it gives sources []", func(t *testing.T) {
		v, err := m.CreateMilestone(context.Background(), MilestoneCreate{Title: "S", Tasks: []MilestoneTaskInput{
			{Key: "T1", Title: "study", Sources: deps(url)}}})
		if err != nil {
			t.Fatal(err)
		}
		_, err = m.Enqueue(StartRequest{Title: "t", Brief: "b", Kind: "review", Model: "gpt-6.1-sol", Milestone: v.ID, Task: "T1"})
		if err == nil || !strings.Contains(err.Error(), "sources need the guard, which does not run inside codex") {
			t.Fatalf("codex job on a task with sources: %v", err)
		}
		j, err := m.Enqueue(StartRequest{Title: "t", Brief: "b", Kind: "review", Model: "gpt-6.1-sol", Milestone: v.ID, Task: "T1",
			Sources: []string{}})
		if err != nil || len(j.Sources) != 0 || j.Runner != store.RunnerCodex {
			t.Fatalf("codex job with sources []: %v %+v", err, j)
		}
	})
}

// slowClone replaces cloneOne with a clone that waits until its context ends; started gets each URL.
func slowClone(t *testing.T) (started chan string, calls *atomic.Int32) {
	t.Helper()
	started, calls = make(chan string, 16), &atomic.Int32{}
	old := cloneOne
	cloneOne = func(ctx context.Context, u, target string, _ int64, _ string) sourceResult {
		calls.Add(1)
		started <- u
		<-ctx.Done()
		return sourceResult{URL: u, Name: filepath.Base(target), Path: target, Err: "clone aborted: " + ctx.Err().Error()}
	}
	t.Cleanup(func() { cloneOne = old })
	return started, calls
}

func TestSourcesCloneOutsideLock(t *testing.T) {
	m := newManager(t)
	t.Cleanup(func() { _ = removeTree(filepath.Join(m.StateDir, "jobs")) })
	t.Run("[G02] a slow clone holds neither cancel nor another tick, and the cancel ends it", func(t *testing.T) {
		started, _ := slowClone(t)
		j := enqueue(t, m, "x", func(r *StartRequest) { r.Kind, r.Sources = "research", []string{"https://git.example.test/a/slow"} })
		first := make(chan error, 1)
		go func() { first <- m.Tick() }()
		select {
		case <-started:
		case <-time.After(10 * time.Second):
			t.Fatal("the clone did not start")
		}
		second := make(chan error, 1)
		go func() { second <- m.Tick() }()
		select {
		case err := <-second:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a second tick (collect, schedules, checks) waited for the clone")
		}
		begin := time.Now()
		if err := m.Cancel(j.ID); err != nil {
			t.Fatal(err)
		}
		if d := time.Since(begin); d > 3*time.Second {
			t.Fatalf("cancel waited %s for the clone", d)
		}
		select {
		case err := <-first:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the clone outlived the cancel")
		}
		if st := get(t, m, j.ID).Status; st != store.StatusCancelled {
			t.Fatalf("status %s", st)
		}
	})
	t.Run("[G02] one deadline covers all the clones of a job", func(t *testing.T) {
		_, calls := slowClone(t)
		old := sourceCloneTimeout
		sourceCloneTimeout = 300 * time.Millisecond
		t.Cleanup(func() { sourceCloneTimeout = old })
		j := enqueue(t, m, "x", func(r *StartRequest) {
			r.Kind, r.Sources = "research", []string{"https://git.example.test/a/one", "https://git.example.test/a/two",
				"https://git.example.test/a/three"}
		})
		begin := time.Now()
		end := runToEnd(t, m, j.ID)
		if n := calls.Load(); n != 1 {
			t.Fatalf("%d clones started; after the first used the deadline the others must not start", n)
		}
		if d := time.Since(begin); d > 10*time.Second {
			t.Fatalf("the job took %s", d)
		}
		if end.Status != store.StatusSucceeded && end.Status != store.StatusFailed {
			t.Fatalf("job did not run: %s %s", end.Status, end.Error)
		}
		prompt, _ := os.ReadFile(filepath.Join(m.JobDir(j.ID), "prompt.md"))
		if strings.Count(string(prompt), "not cloned") != 3 || !strings.Contains(string(prompt), "deadline") {
			t.Fatalf("prompt does not say the clones passed their deadline:\n%s", prompt)
		}
	})
}

func TestSourcesNetworkReach(t *testing.T) {
	old := sourceLookup
	t.Cleanup(func() { sourceLookup = old })
	ctx := context.Background()
	lookup := func(ips ...string) {
		sourceLookup = func(context.Context, string) ([]netip.Addr, error) {
			var out []netip.Addr
			for _, ip := range ips {
				out = append(out, netip.MustParseAddr(ip))
			}
			return out, nil
		}
	}
	t.Run("[G02] a source host that resolves to an internal address is not cloned", func(t *testing.T) {
		for _, ip := range []string{"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254",
			"100.100.79.127", "100.64.0.1", "0.0.0.0", "224.0.0.1", "255.255.255.255", "::1", "::", "fe80::1",
			"fd00::1", "fc00::1", "ff02::1", "::ffff:127.0.0.1", "::ffff:10.0.0.1"} {
			lookup("140.82.112.3", ip)
			if _, err := sourcePin(ctx, "https://git.example.test/a/b"); err == nil || !strings.Contains(err.Error(), "address") {
				t.Errorf("%s: pinned: %v", ip, err)
			}
			res := cloneSource(ctx, "https://git.example.test/a/b", filepath.Join(t.TempDir(), "b"), 1<<20, t.TempDir())
			if !strings.Contains(res.Err, "internal address") {
				t.Errorf("%s: clone not refused before git: %q", ip, res.Err)
			}
		}
	})
	t.Run("[G02] the vetted address is pinned and redirects are not followed", func(t *testing.T) {
		lookup("140.82.112.3")
		pin, err := sourcePin(ctx, "https://github.com/a/b")
		if err != nil || pin != "github.com:443:140.82.112.3" {
			t.Fatalf("pin %q %v", pin, err)
		}
		lookup("2606:4700::1111")
		if pin, err = sourcePin(ctx, "https://github.com/a/b"); err != nil || pin != "github.com:443:[2606:4700::1111]" {
			t.Fatalf("IPv6 pin %q %v", pin, err)
		}
		args := strings.Join(cloneArgs("/tmpl", "https://github.com/a/b", "/t", "github.com:443:140.82.112.3"), " ")
		for _, want := range []string{"-c http.followRedirects=false", "-c http.curloptResolve=github.com:443:140.82.112.3",
			"clone --quiet --depth 1"} {
			if !strings.Contains(args, want) {
				t.Errorf("clone args lack %q: %s", want, args)
			}
		}
	})
}

func TestSourcesTooDeep(t *testing.T) {
	t.Run("[G02] a clone deeper than prune can remove is refused at clone time", func(t *testing.T) {
		const url = "https://git.example.test/org/deep.git"
		withLocalRemote(t, map[string]string{url: repoWith(t, map[string]string{
			strings.Repeat("d/", maxChmodDepth) + "f": "x\n", "README.md": "x\n"})})
		target := filepath.Join(t.TempDir(), "deep")
		res := cloneSource(context.Background(), url, target, 64<<20, t.TempDir())
		if !strings.Contains(res.Err, "deep") {
			t.Fatalf("deep clone: %q", res.Err)
		}
		if _, err := os.Lstat(target); !os.IsNotExist(err) {
			t.Fatalf("deep clone left behind: %v", err)
		}
	})
}

// fakeDocker puts a docker on PATH that logs its arguments and lists two containers of the job it is
// asked for (the name filter's prefix) and one of another job.
func fakeDocker(t *testing.T) (logPath string) {
	t.Helper()
	bin := t.TempDir()
	logPath = filepath.Join(t.TempDir(), "docker.log")
	script := "#!/bin/sh\necho \"$*\" >> \"$FAKE_DOCKER_LOG\"\nif [ \"$1\" = ps ]; then\n  for a in \"$@\"; do case \"$a\" in name=*) p=\"${a#name=}\";; esac; done\n" +
		"  printf '%sa\\n%sb\\naj-other-c\\n' \"$p\" \"$p\"\nfi\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_DOCKER_LOG", logPath)
	return logPath
}

func dockerRemoved(logPath, id string) bool {
	b, _ := os.ReadFile(logPath)
	return slices.Contains(strings.Split(string(b), "\n"), "rm -f aj-"+id+"-a aj-"+id+"-b")
}

func TestSourcesContainersRemoved(t *testing.T) {
	t.Run("[G02] the job's containers (and only them) are removed with docker rm -f", func(t *testing.T) {
		logPath := fakeDocker(t)
		if err := removeJobContainers("261026-1"); err != nil {
			t.Fatal(err)
		}
		if !dockerRemoved(logPath, "261026-1") {
			b, _ := os.ReadFile(logPath)
			t.Fatalf("docker log:\n%s", b)
		}
		if b, _ := os.ReadFile(logPath); strings.Contains(string(b), "aj-other") && strings.Contains(string(b), "rm -f aj-other") {
			t.Fatalf("another job's container removed:\n%s", b)
		}
	})
	t.Run("[G02] no docker on PATH: removing is a no-op", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		if err := removeJobContainers("261026-2"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("[G02] a cancelled job and a finished job with sources get their containers removed", func(t *testing.T) {
		m := newManager(t)
		t.Cleanup(func() { _ = removeTree(filepath.Join(m.StateDir, "jobs")) })
		const url = "https://git.example.test/org/lib.git"
		withLocalRemote(t, map[string]string{url: bareRepo(t, 0)})
		logPath := fakeDocker(t)
		j := enqueue(t, m, "FAKE_SLEEP=30", func(r *StartRequest) { r.Kind, r.Sources = "research", []string{url} })
		if err := m.Tick(); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "running", 15*time.Second, func() bool { return get(t, m, j.ID).Status == store.StatusRunning })
		if err := m.Cancel(j.ID); err != nil {
			t.Fatal(err)
		}
		// Settle marks the cleanup; the next tick removes the containers outside the lifecycle lock.
		if err := m.Tick(); err != nil {
			t.Fatal(err)
		}
		if !dockerRemoved(logPath, j.ID) {
			b, _ := os.ReadFile(logPath)
			t.Fatalf("cancel: containers not removed:\n%s", b)
		}
		j2 := enqueue(t, m, "x", func(r *StartRequest) { r.Kind, r.Sources = "research", []string{url} })
		runToEnd(t, m, j2.ID)
		if !dockerRemoved(logPath, j2.ID) {
			b, _ := os.ReadFile(logPath)
			t.Fatalf("finished job: containers not removed:\n%s", b)
		}
	})
}
