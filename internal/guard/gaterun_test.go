package guard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Inside a job exactly `agent-jobs gate run [gate flags] -- CMD...` runs (#208, owner 2026-11-07): the
// box-wide gate lock serializes the jobs' heavy steps. Every other agent-jobs form stays refused, and
// CMD is checked as if it were run directly, so a gate never carries a refused command.
// These command strings are inspected by Check, never executed.
func TestGateRunInsideJob(t *testing.T) {
	state, own, cfg := stateReadSetup(t)
	cfg.GitName, cfg.GitEmail = "Job Bot", "bot@example.com"
	cfg.Protected = append(cfg.Protected, filepath.Join(own, "gitconfig"))
	work := cfg.Dir
	gateLinkSetup(t, &cfg)
	cases := []struct {
		name  string
		cmd   string
		block bool
	}{
		{"plain", `agent-jobs gate run --head HEAD -- go test ./...`, false},
		{"repo dot", `agent-jobs gate run --head HEAD --repo . -- scripts/check.sh`, false},
		{"repo worktree", `agent-jobs gate run --head 0d28d35 --repo ` + work + ` -- go vet ./...`, false},
		{"single dash flags", `agent-jobs gate run -head=HEAD -repo=. -timeout 20m -lock-wait=10m -- make test`, false},
		{"long = flags", `agent-jobs gate run --head=HEAD --timeout=30m --lock-wait 5m -- npm test`, false},
		{"installed path", `/home/u/.local/bin/agent-jobs gate run --head HEAD -- go test ./internal/...`, false},
		{"behind nohup", `nohup agent-jobs gate run --head HEAD -- go test ./... > gate.log 2>&1 &`, false},
		{"behind timeout", `timeout 40m agent-jobs gate run --head HEAD -- go build ./...`, false},
		{"cmd behind env", `agent-jobs gate run --head HEAD -- env GOFLAGS=-count=1 go test ./...`, false},
		{"in a chain", `git add -A && agent-jobs gate run --head HEAD -- go test ./... && git commit -m x`, false},
		{"tail of output", `agent-jobs gate run --head HEAD -- go test ./... 2>&1 | tail -20`, false},
		{"suffix build installed", `/usr/local/bin/agent-jobs-linux gate run --head HEAD -- go test ./...`, false},
		{"no command", `agent-jobs gate run --head HEAD --`, false},
		{"test package named agent-jobs", `agent-jobs gate run --head HEAD -- go test ./cmd/agent-jobs/`, false},

		// The runner itself in another form.
		{"no --", `agent-jobs gate run --head HEAD go test ./...`, true},
		{"unknown flag", `agent-jobs gate run --head HEAD --lock /tmp/x -- go test ./...`, true},
		{"help flag", `agent-jobs gate run -h -- go test`, true},
		{"gate retire", `agent-jobs gate retire --pr 1`, true},
		{"gate alone", `agent-jobs gate`, true},
		{"gate other", `agent-jobs gate status`, true},
		{"start", `agent-jobs start --title t`, true},
		{"gate then start", `agent-jobs gate run --head HEAD -- agent-jobs start --title t`, true},
		{"gate in gate", `agent-jobs gate run --head HEAD -- agent-jobs gate run --head HEAD -- go test ./...`, true},
		{"gate in gate behind env", `agent-jobs gate run --head HEAD -- env agent-jobs gate run --head HEAD -- true`, true},
		{"job-built relative", `./agent-jobs gate run --head HEAD -- go test ./...`, true},
		{"job-built in own dir", own + `/work/bin/agent-jobs gate run --head HEAD -- go test ./...`, true},
		{"program from variable", `$AJ gate run --head HEAD -- go test ./...`, true},
		{"flag from variable", `agent-jobs gate run --head HEAD $X -- go test ./...`, true},
		// An unquoted expansion splits into words the guard never sees (an injected `--` or --repo).
		{"bare head value", `agent-jobs gate run --head $X -- HEAD:refs/heads/x`, true},
		{"bare attached head", `agent-jobs gate run --head=$X -- true`, true},
		{"bare substitution", `agent-jobs gate run --head $(cat f) -- true`, true},
		{"bare timeout", `agent-jobs gate run --timeout $T --head HEAD -- true`, true},
		{"glob head", `agent-jobs gate run --head HEAD* -- true`, true},
		{"quoted head value", `agent-jobs gate run --head "$SHA" -- go test ./...`, false},
		{"quoted timeout", `agent-jobs gate run --head HEAD --timeout "$T" -- go test ./...`, false},
		{"sh -c gate start", `sh -c 'agent-jobs gate run --head HEAD -- agent-jobs tick'`, true},
		{"xargs gate", `echo go | xargs agent-jobs gate run --head HEAD --`, true},
		{"find -exec gate", `find . -exec agent-jobs gate run --head HEAD -- true \;`, true},

		// A refused command smuggled through the gate.
		{"push", `agent-jobs gate run --head HEAD -- git push origin HEAD`, true},
		{"push behind env", `agent-jobs gate run --head HEAD -- env git push`, true},
		{"gh pr create", `agent-jobs gate run --head HEAD -- gh pr create --fill`, true},
		{"nested agent", `agent-jobs gate run --head HEAD -- claude -p hi`, true},
		{"nested codex", `agent-jobs gate run --head HEAD -- codex exec x`, true},
		{"shell -c push", `agent-jobs gate run --head HEAD -- sh -c 'git push'`, true},
		{"bash -c agent", `agent-jobs gate run --head HEAD -- bash -c 'claude -p x'`, true},
		{"ssh", `agent-jobs gate run --head HEAD -- ssh host true`, true},
		{"rsync remote", `agent-jobs gate run --head HEAD -- rsync -a . host:/x`, true},
		{"systemd-run", `agent-jobs gate run --head HEAD -- systemd-run --user sleep 1`, true},
		{"read state", `agent-jobs gate run --head HEAD -- cat ` + filepath.Join(state, "jobs.db"), true},
		{"write protected", `agent-jobs gate run --head HEAD -- cp x ` + filepath.Join(own, "gitconfig"), true},
		{"unset marker", `agent-jobs gate run --head HEAD -- env -u AGENT_JOBS_JOB go test ./...`, true},
		{"marker prefix", `AGENT_JOBS_JOB= agent-jobs gate run --head HEAD -- go test ./...`, true},
		{"gate env marker", `agent-jobs gate run --head HEAD -- env AGENT_JOBS_GATE_HOLDER=1 go test ./...`, true},
		{"eval", `agent-jobs gate run --head HEAD -- eval 'git push'`, true},
		{"xargs git", `agent-jobs gate run --head HEAD -- xargs git push < /dev/null`, true},

		// --repo is where CMD runs: it is checked like a cd.
		{"repo into state", `agent-jobs gate run --head HEAD --repo ` + state + ` -- cat jobs.db`, true},
		{"repo parent then protected", `agent-jobs gate run --head HEAD --repo .. -- cp x gitconfig`, true},
		{"repo variable", `agent-jobs gate run --head HEAD --repo "$D" -- rm -f x`, true},
		{"repo = state", `agent-jobs gate run --head HEAD --repo=` + state + ` -- cat config.json`, true},
		// A `..` in --repo is refused as written: filepath.Clean makes `link/..` the current directory,
		// while the kernel resolves it to the parent of the link's target (here the protected brief/).
		{"repo link/..", `agent-jobs gate run --head HEAD --repo b/.. -- cp a BRIEF.md`, true},
		{"repo ./link/..", `agent-jobs gate run --head HEAD --repo ./b/.. -- cp a BRIEF.md`, true},
		{"repo = link/..", `agent-jobs gate run --head HEAD --repo=b/.. -- go test ./...`, true},
		{"repo absolute link/..", `agent-jobs gate run --head HEAD --repo ` + work + `/b/.. -- cp a BRIEF.md`, true},
		{"repo absolute dir/..", `agent-jobs gate run --head HEAD --repo ` + work + `/sub/.. -- go test ./...`, true},
		{"repo ./", `agent-jobs gate run --head HEAD --repo ./ -- go test ./...`, false},
		{"repo worktree slash", `agent-jobs gate run --head HEAD --repo ` + work + `/ -- go test ./...`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Check(c.cmd, cfg)
			if (got != "") != c.block {
				t.Fatalf("Check(%q) = %q, want block=%v", c.cmd, got, c.block)
			}
		})
	}
}

// gateLinkSetup adds a protected brief/BRIEF.md to the work tree, a directory brief/d and a link
// work/b -> brief/d (both allowed to create), and a plain directory work/sub: `b/..` is brief/ for the
// kernel and the work tree for filepath.Clean.
func gateLinkSetup(t *testing.T, cfg *Config) {
	t.Helper()
	brief := filepath.Join(cfg.Dir, "brief")
	for _, d := range []string{filepath.Join(brief, "d"), filepath.Join(cfg.Dir, "sub")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(brief, "BRIEF.md"), []byte("brief\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(brief, "d"), filepath.Join(cfg.Dir, "b")); err != nil {
		t.Fatal(err)
	}
	cfg.Protected = append(cfg.Protected, filepath.Join(brief, "BRIEF.md"))
}

// A gate never changes the verdict: `agent-jobs gate run --head HEAD -- CMD` is refused exactly when CMD
// alone is.
func TestGateRunSameVerdictAsDirect(t *testing.T) {
	state, own, cfg := stateReadSetup(t)
	cfg.GitName, cfg.GitEmail = "Job Bot", "bot@example.com"
	gateLinkSetup(t, &cfg)
	work := cfg.Dir
	// The --repo axis: a spelling of the current directory gives the direct verdict; any other spelling
	// (a `..` the kernel resolves through a link) may only be stricter than `cd REPO && CMD`.
	sameDir := []string{".", "./", work, work + "/"}
	otherDir := []string{"b/..", "./b/..", work + "/b/..", "sub/..", ".."}
	for _, cmd := range []string{`cp a BRIEF.md`, `rm -f BRIEF.md`, `cp a brief/BRIEF.md`, `go test ./...`, `git push`} {
		direct := Check(cmd, cfg) != ""
		for _, r := range sameDir {
			if gated := Check(`agent-jobs gate run --head HEAD --repo `+r+` -- `+cmd, cfg) != ""; gated != direct {
				t.Errorf("--repo %s %q: direct blocked=%v, through the gate blocked=%v", r, cmd, direct, gated)
			}
		}
		for _, r := range otherDir {
			viaCD := Check(`cd `+r+` && `+cmd, cfg) != ""
			if gated := Check(`agent-jobs gate run --head HEAD --repo `+r+` -- `+cmd, cfg) != ""; viaCD && !gated {
				t.Errorf("--repo %s %q: allowed through the gate, but `cd %s && %s` is blocked", r, cmd, r, cmd)
			}
		}
	}
	for _, cmd := range []string{
		`go test ./...`, `scripts/check.sh`, `npx tsc --noEmit`, `npm run build`, `make -j4 check`,
		`git push`, `git -C . push`, `gh pr merge 1`, `claude -p x`, `gemini-cli x`, `aider-chat`,
		`curl -sS https://example.com`, `ssh host`, `scp a host:b`, `rsync -a . host:x`, `docker run x`,
		`tmux new -d x`, `crontab -l`, `systemctl --user restart x`, `rm -rf ` + state, `rm -rf ` + own + `/work/tmp`,
		`cat ` + filepath.Join(state, "config.json"), `python3 -c 'import os; os.system("git push")'`,
		`node -e 'require("child_process").execSync("claude")'`, `go run ./cmd/agent-jobs start`,
		`env -i go test ./...`, `sudo true`, `git config core.hooksPath x`, `git commit --author="X <x@y>" -m x`,
	} {
		direct := Check(cmd, cfg) != ""
		gated := Check(`agent-jobs gate run --head HEAD -- `+cmd, cfg) != ""
		if direct != gated {
			t.Errorf("%q: direct blocked=%v, through the gate blocked=%v", cmd, direct, gated)
		}
	}
}

// The refusal of another agent-jobs form names the allowed gate form, so the job can correct itself.
func TestGateRunRefusalNamesTheForm(t *testing.T) {
	got := Check(`agent-jobs gate run --head HEAD go test ./...`, Config{})
	if !strings.Contains(got, "agent-jobs gate run --head") || !strings.Contains(got, "-- CMD") {
		t.Fatalf("refusal %q does not name the gate form", got)
	}
}
