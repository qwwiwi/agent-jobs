package guard

import (
	"strings"
	"testing"
)

// #54: the other names of the archive tools (p7zip and libarchive wrappers, unar, unrar, …) and a
// program name spelled with a character that case-folds to ASCII get the ancestor rule too (cwd
// <state>/jobs/j/work, so ../../../.. is the agent-jobs home).
func TestProtectedAncestorArchiverAliases(t *testing.T) {
	_, _, _, cfg := protectedSetup(t)
	for _, c := range []string{
		"7zr x a.7z -o../../../..",
		"7zr x a.7z -O../../../..",
		"7zG x a.7z -o../../../..",
		"7zzs x a.7z -o../../../..",
		"/usr/local/bin/7ZR x a.7z -o../../../..",
		"p7zip -d ../../../..",
		"bsdcpio -pdm ../../../.. < list",
		"bsdcpio -idmD../../../.. < a.cpio",
		"bsdunzip -o a.zip -d ../../../..",
		"bsdunzip -qd../../../.. a.zip",
		"bsdunzip -o --qd../../../.. a.zip",
		"gnutar -xC../../../.. -f a.tar",
		"unar -o ../../../.. a.zip",
		"unrar x a.rar ../../../..",
		"rar x a.rar ../../../..",
		"cabextract -d ../../../.. a.cab",
		"aunpack -X ../../../.. a.zip",
		"atool -x -X ../../../.. a.zip",
		"dtrx ../../../..",
		"xar -xf a.xar -C ../../../..",
		"jar xf a.jar ../../../..",
		// program names with a character that folds to an ASCII letter (U+017F long s, U+212A Kelvin)
		"rſync -a src/ ../../../..",
		"bſdtar -xC../../../.. -f a.tar",
		"unlinK ../../../..",
		"/usr/bin/ſhred -u ../../../..",
	} {
		if Check(c, cfg) == "" {
			t.Errorf("%q allowed", c)
		}
	}
	for _, c := range []string{
		"7zr l a.7z", "7zr x a.7z -obuild", "7zr x a.7z -o..", "p7zip -d a.7z",
		"bsdcpio -idm < a.cpio", "bsdcpio -idmDbuild < a.cpio", "bsdunzip -l a.zip",
		"bsdunzip -o a.zip -d build", "bsdunzip -qdbuild a.zip", "gnutar -tf a.tar",
		"unar -o build a.zip", "unar a.zip", "lsar a.zip", "unrar l a.rar", "unrar x a.rar build/",
		"cabextract -d build a.cab", "aunpack a.zip", "xar -xf a.xar -C build", "jar tf a.jar",
		"rsync -a src/ dst/", "rſync -a src/ dst/", "ls ../../../..", "du -sh ../..",
		"grep -r foo .", "cat README.md", "git status", "go test ./...",
	} {
		if r := Check(c, cfg); r != "" {
			t.Errorf("%q refused: %s", c, r)
		}
	}
}

// #54: on a case-insensitive file system names compare under Unicode case folding, not lower-casing:
// U+017F (long s) folds to `s` and U+212A (Kelvin sign) to `k`.
func TestProtectedUnicodeCaseFolded(t *testing.T) {
	state, _, _, cfg := protectedSetup(t)
	setFoldCase(t, true)
	ls := "ſ"
	for _, c := range []string{
		"rm " + state + "/job" + ls + ".db",
		"rm " + state + "/JOB" + ls + ".DB",
		"rm -rf " + state + "/" + ls + "ubagents",
		"rm ../../../job" + ls + ".db",
		"echo x > " + state + "/" + ls + "ubagents/j.log",
		"dd if=/dev/null of=" + state + "/job" + ls + ".db-wal",
		"python3 -c \"open('" + state + "/job" + ls + ".db','w')\"",
		"python3 -c \"open('../../../job" + ls + ".db','w')\"",
		"rm -r ~/.agent-job" + ls,
		"cp evil " + state + "/" + ls + "ubagents/",
		"cd \"$X\" && rm " + ls + "ubagents/j.log",
		"sqlite3 " + state + "/jobs.db-Kournal 'x'",
	} {
		if Check(c, cfg) == "" {
			t.Errorf("%q allowed", c)
		}
	}
	for _, p := range []string{
		state + "/job" + ls + ".db", state + "/" + ls + "ubagents/j.log", state + "/JOB" + ls + ".DB-SHM",
	} {
		if CheckFileWrite("Write", p, cfg) == "" {
			t.Errorf("CheckFileWrite(%q) allowed", p)
		}
	}
	for _, c := range []string{
		"cat " + state + "/job" + ls + ".db", "rm out/job" + ls + ".db", "cp " + ls + ".txt build/",
		"rm -rf build/Kelvin", "echo straße > out.txt", "touch café.txt",
	} {
		if r := Check(c, cfg); r != "" {
			t.Errorf("%q refused: %s", c, r)
		}
	}
	s := &pathScope{protected: []string{"/h/.agent-jobs/thrall/subagents", "/h/.agent-jobs/thrall/jobs.db"}}
	if !s.couldReach(ls+"UBAGENTS/j.log", false) || !s.couldReach("thrall/job"+ls+".db", false) {
		t.Error("couldReach misses a Unicode case variant")
	}
}

// #54: on a case-sensitive file system the Unicode variants are other files and stay allowed.
func TestProtectedUnicodeCaseSensitive(t *testing.T) {
	state, _, _, cfg := protectedSetup(t)
	setFoldCase(t, false)
	if r := Check("rm "+state+"/jobſ.db", cfg); r != "" {
		t.Errorf("rm jobſ.db refused: %s", r)
	}
	if r := CheckFileWrite("Write", state+"/jobſ.db", cfg); r != "" {
		t.Errorf("CheckFileWrite jobſ.db refused: %s", r)
	}
}

// progName and fold agree with strings.EqualFold on the characters that fold to ASCII.
func TestFoldUnicode(t *testing.T) {
	setFoldCase(t, true)
	for _, c := range [][2]string{
		{"jobs.db", "JOBſ.DB"}, {"k", "K"}, {"K", "K"}, {"s", "ſ"},
		{"å", "Å"}, {"straße", "STRAẞE"},
	} {
		if fold(c[0]) != fold(c[1]) {
			t.Errorf("fold(%q) = %q, fold(%q) = %q", c[0], fold(c[0]), c[1], fold(c[1]))
		}
		if !strings.EqualFold(c[0], c[1]) {
			t.Errorf("EqualFold(%q, %q) = false", c[0], c[1])
		}
	}
	for _, c := range [][2]string{{"rsync", "rſync"}, {"unlink", "UNLINK"}, {"tar", "TAR"}} {
		if progName(c[1]) != c[0] {
			t.Errorf("progName(%q) = %q, want %q", c[1], progName(c[1]), c[0])
		}
	}
	setFoldCase(t, false)
	if fold("JOBſ") != "JOBſ" {
		t.Error("fold changes a name on a case-sensitive file system")
	}
}
