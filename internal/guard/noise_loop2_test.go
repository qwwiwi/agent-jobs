package guard

import (
	"os"
	"path/filepath"
	"testing"
)

// Fix loop 2 of the noise relaxations (noise.go). Inline code: a piece with `?`, `[`, `]` or `{` is
// refused as before the relaxation, whatever the language's glob dialect does with it (python fnmatch
// reads `[!x]` as a negation and `[]t]` as a set holding `]`, Go's filepath.Match does not).
func TestNoiseLoop2InlineCodeDialects(t *testing.T) {
	_, own, cfg := noiseSetup(t)
	// A real worktree has no link to jobs.db: the state check expands `*` of `a * b` on disk.
	if err := os.Remove(filepath.Join(own, "work", "lnk")); err != nil {
		t.Fatal(err)
	}
	runRefused(t, cfg, []refusalCase{
		{"python negated set", `python3 -c 'import glob;open(glob.glob(".gi[!x]/config")[0],"w").close()'`, "glob/brace"},
		{"python leading bracket set", `python3 -c 'import glob;open(glob.glob(".gi[]t]/config")[0],"w").close()'`, "glob/brace"},
		// Go's matcher reads `[!x]` as the set {!, x}: no match of .git, the piece passed.
		{"python negated set joined", `python3 -c 'import glob,os;open(os.path.join(glob.glob(".gi[!x]")[0],"config"),"w")'`, "glob/brace"},
		{"perl question mark", `perl -e 'print glob("a?")'`, "glob/brace"},
		{"python brace", `python3 -c 'print({1,2})'`, "glob/brace"},
		{"python index", `python3 -c "import sys; print(sys.argv[1])"`, "glob/brace"},
		{"node array", `node -e "console.log([1,2].map(x => x*2))"`, "glob/brace"},
		{"perl brace in substitution", `perl -0pi -e 's/(a)/{$1}/' notes.md`, "glob/brace"},
		// awk has no glob, but its system() hands a string to the shell: the action block stays refused.
		{"awk action block", `awk '{print $1*2}' data.txt`, "glob/brace"},
		// node's glob reads `*(…)` as an extglob (`*(.git)/config` matches .git/config).
		{"node extglob", `node -e "require('fs').globSync('*(.git)/config')"`, "inline code"},
	})
	runAllowed(t, cfg, []string{
		`python3 -c "print(2*3)"`,
		`python3 -c "a, b = 2, 3; print(a ** b)"`,
		`python3 -c "print(2**8)"`,
		`perl -pe 's/.*/x/' notes.md`,
		`perl -e 'print 2*3'`,
		`awk '$1*2 > 3' data.txt`,
		`python3 -c "import glob; print(glob.glob('internal/*.go'))"`,
	})
}

// git archive and format-patch write files: -o (attached or separate), --output, --output-directory
// in any abbreviation, and format-patch into the current directory by default.
func TestNoiseLoop2GitWriters(t *testing.T) {
	state, _, cfg := noiseSetup(t)
	db := filepath.Join(state, "jobs.db")
	runRefused(t, cfg, []refusalCase{
		{"archive attached -o git config", "git archive -o.git/config HEAD", ""},
		{"archive separate -o hooks", "git archive -o .git/hooks/pre-commit HEAD", ""},
		{"archive clustered -o", "git archive -vo.git/config HEAD", ""},
		{"archive --output git config", "git archive --output=.git/config HEAD", ""},
		{"archive -o protected", "git archive -o " + db + " HEAD", ""},
		{"archive -o glob", "git archive -o .gi?/config HEAD", "glob/brace"},
		{"format-patch -o git dir", "git format-patch -1 -o .git", ""},
		{"format-patch -o hooks", "git format-patch -1 -o .git/hooks", ""},
		{"format-patch attached -o hooks", "git format-patch -1 -o.git/hooks", ""},
		{"format-patch --output-directory hooks", "git format-patch -1 --output-directory=.git/hooks", ""},
		{"format-patch abbreviated output-directory", "git format-patch -1 --output-dir .git/hooks", ""},
		{"format-patch -C git dir", "git -C .git format-patch -1", ""},
		{"format-patch -C hooks -o .", "git -C .git/hooks format-patch -1 -o .", ""},
		{"archive in subst", `echo "$(git archive HEAD)"`, ""},
		{"format-patch in subst", `echo "$(git format-patch -1 --stdout)"`, ""},
	})
	// format-patch creates files in its directory, it does not reach below: a protected file deeper in
	// the worktree (brief/BRIEF.md in a real job) does not refuse it; the protected dir itself does.
	skills := filepath.Join(filepath.Dir(filepath.Dir(state)), "lib", "skills")
	cfg.Protected = append(cfg.Protected, filepath.Join(cfg.Dir, "brief", "BRIEF.md"), filepath.Join(state, "subagents"), skills)
	runRefused(t, cfg, []refusalCase{
		{"format-patch into protected dir", "git format-patch -1 -o " + filepath.Join(state, "subagents"), ""},
		{"format-patch -C protected dir", "git -C " + state + " format-patch -1 -o subagents", ""},
		// An absolute -C after a relative one starts from there.
		{"archive -C relative then absolute", "git -C internal -C " + filepath.Dir(skills) + " archive -o skills/x.tar HEAD", ""},
	})
	runAllowed(t, cfg, []string{
		"git archive -o out.tar HEAD",
		"git archive --format=tar HEAD",
		"git format-patch -1 -o patches",
		"git format-patch -1 --stdout",
		"git format-patch -1",
	})
	// A review job: archive and format-patch write files, format-patch into the worktree by default.
	_, review, _ := readOnlySetup(t)
	runRefused(t, review, []refusalCase{
		{"read-only format-patch -o .", "git format-patch -1 -o .", "review job, read-only"},
		{"read-only format-patch output-directory", "git format-patch -1 --output-directory=.", "review job, read-only"},
		{"read-only format-patch default", "git format-patch -1", "review job, read-only"},
		{"read-only archive -o", "git archive -o out.tar HEAD", "review job, read-only"},
		{"read-only archive attached -o", "git archive -oout.tar HEAD", "review job, read-only"},
		// An absolute -C after another one starts from there: the output lands in the worktree.
		{"read-only -C absolute after -C", "git -C /tmp -C " + review.ReadOnly + " diff --output=notes.md", "review job, read-only"},
	})
	runAllowed(t, review, []string{"git diff HEAD", "git log --oneline -3"})
}
