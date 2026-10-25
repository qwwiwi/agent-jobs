package guard

import (
	"path/filepath"
	"slices"
	"testing"
)

// Fix loop 3 of the noise relaxations. format-patch's --stdout has a negation (`--no-stdout`, any
// abbreviation of it such as `--no-std`): the last of the two decides whether the patches go into the
// output directory (the current one by default).
func TestNoiseLoop3FormatPatchNoStdout(t *testing.T) {
	_, _, cfg := noiseSetup(t)
	runRefused(t, cfg, []refusalCase{
		{"stdout then no-stdout", "git -C .git format-patch --stdout --no-stdout -1", ""},
		{"no-std abbreviation", "git -C .git format-patch --stdout --no-std -1", ""},
		{"no-stdout into hooks", "git format-patch -1 --stdout --no-stdout -o .git/hooks", ""},
	})
	runAllowed(t, cfg, []string{
		"git format-patch -1 --no-stdout --stdout",
		"git format-patch -1 --stdout --no-stdout -o patches",
	})
}

// A git write target computed at run time (a variable, a substitution) cannot be checked against git's
// files and the protected paths: it is refused like such a redirection target.
func TestNoiseLoop3GitDynamicTargets(t *testing.T) {
	_, _, cfg := noiseSetup(t)
	const want = "cannot verify the write target"
	runRefused(t, cfg, []refusalCase{
		{"archive -o variable", `git archive -o "$G" HEAD`, want},
		{"archive -o bare variable", `git archive -o $G HEAD`, want},
		{"archive attached -o variable", `git archive -o"$G" HEAD`, want},
		{"archive -o substitution", `git archive -o "$(echo .git/config)" HEAD`, want},
		{"archive -o backticks", "git archive -o `echo .git/config` HEAD", want},
		{"archive --output variable", `git archive --output="$X" HEAD`, want},
		{"diff --output variable", `git diff --output="$X"`, want},
		{"diff --output braced variable", `git diff --output ${X}`, want},
		{"format-patch -o variable", `git format-patch -1 -o "$G"`, want},
		{"format-patch -o substitution", `git format-patch -1 -o "$(echo .git)"`, want},
		{"format-patch --output-directory variable", `git format-patch -1 --output-directory="$G"`, want},
	})
	runAllowed(t, cfg, []string{
		"git archive -o out.tar HEAD",
		"git diff --output=diff.txt",
		`git log --format="%H $x"`,
	})
}

// git bundle create, fast-export and fast-import marks files are written like archive's -o: a fix job
// does not write them into git's files, a protected path or a target computed at run time; a review job
// does not run these subcommands.
func TestNoiseLoop3GitBundleAndMarks(t *testing.T) {
	state, _, cfg := noiseSetup(t)
	db := filepath.Join(state, "jobs.db")
	runRefused(t, cfg, []refusalCase{
		{"bundle into git dir", "git bundle create .git/x.bundle HEAD", ""},
		{"bundle over git config", "git bundle create .git/config HEAD", ""},
		{"bundle quiet over hooks", "git bundle create -q .git/hooks/pre-commit HEAD", ""},
		{"bundle version over config", "git bundle create --version=3 .git/config HEAD", ""},
		{"bundle separate version over config", "git bundle create --version 3 .git/config HEAD", ""},
		{"bundle -C git dir", "git -C .git bundle create config HEAD", ""},
		{"bundle protected", "git bundle create " + db + " HEAD", ""},
		{"bundle variable", `git bundle create "$B" HEAD`, "cannot verify the write target"},
		{"fast-export export-marks config", "git fast-export --export-marks=.git/config HEAD", ""},
		{"fast-export separate export-marks", "git fast-export --export-marks .git/config HEAD", ""},
		{"fast-export abbreviated export-marks", "git fast-export --export=.git/config HEAD", ""},
		{"fast-export export-marks protected", "git fast-export --export-marks=" + db + " HEAD", ""},
		{"fast-export export-marks variable", `git fast-export --export-marks="$M" HEAD`, "cannot verify the write target"},
		{"fast-import export-marks hooks", "git fast-import --export-marks=.git/hooks/post-commit", ""},
		// The stream's own export-marks features name files the guard does not see.
		{"fast-import unsafe features", "git fast-import --allow-unsafe-features", "cannot verify the write target"},
	})
	runAllowed(t, cfg, []string{
		"git bundle create repo.bundle HEAD",
		"git bundle create -q --version=3 repo.bundle main",
		"git bundle verify repo.bundle",
		"git fast-export --export-marks=marks.txt HEAD",
		"git fast-export HEAD",
	})
	_, review, fix := readOnlySetup(t)
	runRefused(t, review, []refusalCase{
		{"read-only bundle create", "git bundle create x.bundle HEAD", "review job, read-only"},
		{"read-only fast-export marks", "git fast-export --export-marks=marks HEAD", "review job, read-only"},
		{"read-only fast-import", "git fast-import --quiet", "review job, read-only"},
	})
	// --relative-marks (on the command line or as a feature of the stream) puts a relative marks file
	// below .git/info/fast-import (a fix job without agent state around its worktree).
	runRefused(t, fix, []refusalCase{
		{"fast-import relative marks", "git fast-import --relative-marks --export-marks=../../config", ""},
		{"fast-import relative marks from the stream", "git fast-import --import-marks-if-exists=../../hooks/pre-commit", ""},
	})
}

// Inside ${…} bash reads up to the matching `}` as one unit of the word: a blank, `#`, `;` or a quote
// there is part of the expansion, so `${x:- #}` starts no comment and the commands after it run.
func TestNoiseLoop3ParamExpansionUnit(t *testing.T) {
	state, _, cfg := noiseSetup(t)
	db := filepath.Join(state, "jobs.db")
	runRefused(t, cfg, []refusalCase{
		{"hash in default", "echo ${x:- #}; git push origin main", ""},
		{"hash in default rm", "echo ${x:- #}; rm -f " + db, ""},
		{"hash in prefix removal", "echo ${x# #}; cat " + db, "agent-jobs state"},
		{"hash in default newline", "echo ${x:- #}\ncat " + db, "agent-jobs state"},
		{"nested expansion hash", "echo ${x:-${y:- #}}; git push origin main", ""},
		// Inside "${…}" bash reads single quotes as quotes: the `"` between them does not end the word.
		{"single quotes in quoted expansion", `echo "${x:-'"'}"; git push origin main; echo "'"`, ""},
		// A bare `{` does not nest: the first `}` ends the expansion (bash prints `{ab}` for ${x:-{a}b}).
		{"bare brace does not nest", "echo ${x:-{a} ; git push origin main ; : }", ""},
		// Substitutions inside the expansion still run and are checked.
		{"substitution in default", "echo ${x:-$(git push origin main)}", ""},
		{"backticks in default", "echo ${x:- `cat " + db + "`}", "agent-jobs state"},
		{"substitution in quoted default", `echo "${x:-"$(cat ` + db + `)"}"`, "agent-jobs state"},
	})
	runAllowed(t, cfg, []string{
		"echo ${#x}",
		"echo ${x#pre}",
		"echo ${x##pat}",
		`echo "${x%%/*}"`,
		"echo ${x:-a b} done",
		"echo ${x:- #} # a comment",
		`f=${HOME}/x; echo "$f"`,
		`echo "${x:-$(git rev-parse HEAD)}"`,
		"echo ${x:-'}'}",
	})
}

// The reader's words and commands for ${…}: one word up to the matching `}`, the commands after it.
func TestShellParamExpansionWords(t *testing.T) {
	for _, tc := range []struct {
		src  string
		want [][]string
	}{
		{"echo ${x:- #}; rm y", [][]string{{"echo", "${x:- #}"}, {"rm", "y"}}},
		{"echo ${#x} ${x#pre} ${x##p*}o", [][]string{{"echo", "${#x}", "${x#pre}", "${x##p*}o"}}},
		{"echo ${x:-{a} ; rm y}", [][]string{{"echo", "${x:-{a}"}, {"rm", "y}"}}},
		{"echo ${x:-${y:-a b}c}d", [][]string{{"echo", "${x:-${y:-a b}c}d"}}},
		{`echo ${x:-"a;b"} ${x:-'}'}`, [][]string{{"echo", "${x:-a;b}", "${x:-}}"}}},
		{`echo "${x:-'"'}"; rm y`, [][]string{{"echo", `${x:-'"'}`}, {"rm", "y"}}},
		// Unterminated: read on as before.
		{"echo ${x:- y", [][]string{{"echo", "${x:-", "y"}}},
	} {
		sc, _ := parseScript(tc.src)
		var got [][]string
		for _, pl := range sc.pipelines {
			for _, c := range pl.cmds {
				got = append(got, c.words)
			}
		}
		if !slices.EqualFunc(got, tc.want, slices.Equal[[]string]) {
			t.Errorf("%q: commands %q, want %q", tc.src, got, tc.want)
		}
	}
}
