package guard

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Review fixtures only: command strings are never executed.
func controlArchive(t *testing.T, path string, headers []*tar.Header) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	var tw *tar.Writer
	var gz *gzip.Writer
	if strings.HasSuffix(path, ".gz") {
		gz = gzip.NewWriter(f)
		tw = tar.NewWriter(gz)
	} else {
		tw = tar.NewWriter(f)
	}
	for _, h := range headers {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if gz != nil {
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestControlDeveloper(t *testing.T) {
	_, _, cfg := stateReadSetup(t)
	for _, d := range []string{"src", "build", "vendor", "copy"} {
		if err := os.MkdirAll(filepath.Join(cfg.Dir, d), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(cfg.Dir, "src/main.go"), []byte("package main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	controlArchive(t, filepath.Join(cfg.Dir, "repo.tar.gz"), []*tar.Header{{Name: "src/main.go", Mode: 0600}})
	for _, cmd := range []string{
		"npm run build", "npm test -- --runInBand", "pnpm run lint", "pnpm --filter web build",
		"go build ./...", "go build -o build/app ./cmd/app", "make -j4", "make -C src test",
		"docker build -t app:dev .", "git worktree list --porcelain", "git worktree add ../feature -b feature",
		"tar -xzf repo.tar.gz", "tar -xzf repo.tar.gz -C build && cat build/src/main.go",
		"cp -a src copy/", "rsync -a src/ copy/", "python3 -m pytest -q",
		"cp -a . ../repo-copy", "rsync -a src/ vendor/",
	} {
		t.Run(cmd, func(t *testing.T) {
			reason := Check(cmd, cfg)
			wantBlock := strings.HasPrefix(cmd, "docker ")
			if (reason != "") != wantBlock {
				t.Errorf("%s: %s; want blocked=%v", cmd, reason, wantBlock)
			}
		})
	}
}

func TestControlLinkRegressions(t *testing.T) {
	state, _, cfg := stateReadSetup(t)
	src := t.TempDir()
	if err := os.Symlink("../../../../thrall", filepath.Join(src, "L")); err != nil {
		t.Fatal(err)
	}
	controlArchive(t, filepath.Join(cfg.Dir, "hard.tar"), []*tar.Header{{Name: "copy", Linkname: filepath.Join(state, "jobs.db"), Typeflag: tar.TypeLink, Mode: 0600}})
	controlArchive(t, filepath.Join(cfg.Dir, "hardnested.tar"), []*tar.Header{{Name: "hard", Typeflag: tar.TypeDir, Mode: 0700}, {Name: "hard/jobs.db", Linkname: filepath.Join(state, "jobs.db"), Typeflag: tar.TypeLink, Mode: 0600}})
	controlArchive(t, filepath.Join(cfg.Dir, "links.tar"), []*tar.Header{{Name: "L", Linkname: state, Typeflag: tar.TypeSymlink, Mode: 0777}})
	for _, cmd := range []string{
		"tar -xPf hard.tar; printf x > copy",
		"tar -xPf hard.tar; cat copy",
		"tar -xPf hardnested.tar; printf x > hard/jobs.db",
		"sh -c 'tar xf links.tar'; printf x > L/jobs.db",
		"sh -c 'tar xf links.tar'; cat L/jobs.db",
		fmt.Sprintf("sh -c 'cp -P %s/L copied'; printf x > copied/jobs.db", src),
		"tar xf links.tar | { cat >/dev/null; }; printf x > L/jobs.db",
	} {
		t.Run(cmd, func(t *testing.T) {
			if reason := Check(cmd, cfg); reason == "" {
				t.Errorf("allowed: %s", cmd)
			} else {
				t.Log(reason)
			}
		})
	}
}

func TestControlLatency(t *testing.T) {
	_, _, cfg := stateReadSetup(t)
	for _, n := range []int{100, 1000, 5000} {
		for _, kind := range []string{"sequential", "pipeline", "nonreader_pipeline", "quoted"} {
			var cmd string
			switch kind {
			case "sequential":
				cmd = strings.Repeat("true; ", n)
			case "pipeline":
				cmd = "printf ok" + strings.Repeat(" | cat", n)
			case "nonreader_pipeline":
				cmd = "true" + strings.Repeat(" | true", n)
			case "quoted":
				cmd = "python3 -c '" + strings.Repeat("x = \"data\";", n) + "'"
			}
			t.Run(fmt.Sprintf("%s_%d", kind, n), func(t *testing.T) {
				start := time.Now()
				reason := Check(cmd, cfg)
				t.Logf("bytes=%d elapsed=%v refused=%v", len(cmd), time.Since(start), reason != "")
			})
		}
	}
}

func TestControlLongPipeline(t *testing.T) {
	_, _, cfg := stateReadSetup(t)
	cmd := "true" + strings.Repeat(" | true", 20000)
	start := time.Now()
	reason := Check(cmd, cfg)
	t.Logf("bytes=%d elapsed=%v reason=%s", len(cmd), time.Since(start), reason)
	if elapsed := time.Since(start); elapsed > protectedBudget+time.Second {
		t.Errorf("guard exceeded %v budget: %v", protectedBudget, elapsed)
	}
}
